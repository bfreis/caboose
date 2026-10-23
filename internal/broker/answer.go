package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// Reply is a person's answer to a request.
type Reply struct {
	// Verdict is "allow" or "deny".
	Verdict string
	// Scope is how far an allow reaches: ScopeOnce, ScopeHours or
	// ScopeAlways. A deny ignores it.
	Scope string
	// Hours is how long, for ScopeHours: 1 to MaxHours.
	Hours int
	// By is who answered: ByCLI or ByWeb (the daemon's client
	// kinds, as the connection or the page says, never the answer) or
	// ByDialog.
	By string
	// Confirm is, for ScopeAlways, the DiffHash of the diff the person
	// was shown (an Offer's) and confirmed; an "always" answer without
	// one is refused, and one whose diff is no longer what config.toml
	// would get is ErrStaleDiff. Either way nothing is written.
	Confirm string
}

// The verdicts of a Reply.
const (
	ReplyAllow = "allow"
	ReplyDeny  = "deny"
)

// AnswerResult is what an answer did.
type AnswerResult struct {
	// Request is the request as it stands after the answer.
	Request RequestInfo
	// Effect says what the answer does (policy.AnswerEffect), or did.
	Effect string
	// Diff and DiffHash are, for an "always" answer, the change to
	// config.toml and its hash.
	Diff, DiffHash string
	// Recorded is set once the answer is recorded.
	Recorded bool
	// Grant is the grant the answer made, "" for none.
	Grant string
	// Also are the other requests the answer decided with it.
	Also []uint64
	// OpenError is why the URL an allowed URL request named could not be
	// opened, "" when it was (or there was none to open).
	OpenError string
}

// DiffHash is the hash an "always" answer confirms a diff by.
func DiffHash(diff string) string {
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])
}

func (rp Reply) check() error {
	switch rp.By {
	case ByCLI, ByWeb, ByDialog:
	default:
		return newError(KindBadRequest, "an answer comes from %q, %q or %q, not %q", ByCLI, ByWeb, ByDialog, clip(rp.By, 32))
	}
	switch rp.Verdict {
	case ReplyDeny:
		return nil
	case ReplyAllow:
	default:
		return newError(KindBadRequest, "an answer is %q or %q, not %q", ReplyAllow, ReplyDeny, clip(rp.Verdict, 32))
	}
	switch rp.Scope {
	case ScopeOnce:
	case ScopeAlways:
		if rp.Confirm == "" {
			return newError(KindBadRequest, "an answer that allows always confirms the diff it was shown: ask for the request's offers first")
		}
	case ScopeHours:
		if rp.Hours < 1 || rp.Hours > MaxHours {
			return newError(KindBadRequest, "an answer for hours allows 1 to %d hours, not %d", MaxHours, rp.Hours)
		}
	default:
		return newError(KindBadRequest, "an answer's scope is %q, %q or %q, not %q", ScopeOnce, ScopeHours, ScopeAlways, clip(rp.Scope, 32))
	}
	return nil
}

func policyScope(s string) policy.Scope {
	switch s {
	case ScopeOnce:
		return policy.ScopeOnce
	case ScopeHours:
		return policy.ScopeHours
	case ScopeAlways:
		return policy.ScopeAlways
	}
	return 0
}

// Answer answers the pending request id. Only a pending request can be
// answered, and the first answer wins: a later one is ErrAlreadyAnswered,
// saying by whom. Allowing grants as the scope says -- once, for some
// hours, or always, which edits config.toml once the diff is confirmed
// -- and allowing a URL request opens its URL; denying records the
// answer and nothing else.
func (b *Broker) Answer(id uint64, rp Reply) (AnswerResult, error) {
	var res AnswerResult
	if err := b.owns(); err != nil {
		return res, err
	}
	if err := rp.check(); err != nil {
		return res, err
	}
	now := b.now()
	var (
		answerErr error
		expired   []reqRec
		also      []reqRec
		answered  reqRec
		reload    bool
	)
	err := b.txn(func(s *state) (bool, bool, error) {
		if s.reqsErr != nil {
			return false, false, newError(KindUnavailable, "%v", s.reqsErr)
		}
		expired = expireIn(&s.reqs, now)
		wr := len(expired) > 0
		q := s.reqs.find(id)
		if q == nil {
			answerErr = newError(KindNotFound, "no request #%d in environment %s", id, b.opts.Env)
			return false, wr, nil
		}
		if q.State != StatePending {
			answerErr = newError(KindAlreadyAnswered, "request #%d was already answered by %s (%s)", id, q.DecidedBy, q.State)
			return false, wr, nil
		}
		if rp.Verdict == ReplyDeny {
			s.reqs.decide(q, StateDenied, rp.By, "", 0, "denied by a person ("+rp.By+")", now)
			answered = *q
			return false, true, nil
		}
		_, set := b.snapshot(now)
		r := q.request()
		// Only what is offered now is answered: the same rules, the same
		// diff.
		o, err := b.offer(q, set, rp.Scope)
		if err != nil {
			answerErr = err
			return false, wr, nil
		}
		res.Effect = policy.AnswerEffect(r, policyScope(rp.Scope), rp.Hours, b.opts.Env, set.VMProfile, set.ConfigPath)
		var grant *grantRec
		allowed := privateAddrsOf(q)
		switch rp.Scope {
		case ScopeOnce:
			if r.Kind != policy.KindOpenURL {
				grant = &grantRec{Kind: q.Kind, Key: q.Key, Once: true, Addrs: allowed, Created: now, Expires: now.Add(OnceTTL), Request: q.ID}
			}
		case ScopeHours:
			grant = &grantRec{Kind: q.Kind, Key: q.Key, Addrs: allowed, Created: now, Expires: now.Add(time.Duration(rp.Hours) * time.Hour), Request: q.ID}
		case ScopeAlways:
			res.Diff, res.DiffHash = o.Diff, o.DiffHash
			if rp.Confirm != res.DiffHash {
				answerErr = newError(KindStaleDiff, "%s would change differently than the diff confirmed; look at it again", set.ConfigPath)
				return false, wr, nil
			}
			if err := b.opts.ConfigWriter.Write(o.edit, rp.Confirm); err != nil {
				if errors.Is(err, ErrStaleDiff) {
					answerErr = err
				} else {
					answerErr = newError(KindUnavailable, "cannot write %s: %v", set.ConfigPath, err)
				}
				return false, wr, nil
			}
			reload = true
		}
		wg := false
		if grant != nil && s.grantsErr == nil {
			// A grant made for this very request by an answer whose
			// record could not be written (grants.json is written before
			// requests.json) is the one this answer makes: answering
			// again never adds a second.
			for _, g := range s.grants.Grants {
				if g.Request == q.ID && g.Kind == grant.Kind && g.Key == grant.Key && g.Once == grant.Once && now.Before(g.Expires) {
					res.Grant = g.ID
					grant = nil
					break
				}
			}
		}
		if grant != nil {
			if s.grantsErr != nil {
				answerErr = newError(KindUnavailable, "%v: fix or remove it first", s.grantsErr)
				return false, wr, nil
			}
			if s.grants.NextID == 0 {
				s.grants.NextID = 1
			}
			grant.ID = "g" + strconv.FormatUint(s.grants.NextID, 10)
			s.grants.NextID++
			s.grants.Grants = append(s.grants.Grants, *grant)
			res.Grant, wg = grant.ID, true
		}
		s.reqs.decide(q, StateAllowed, rp.By, rp.Scope, rp.Hours, fmt.Sprintf("allowed by a person (%s): %s", rp.By, res.Effect), now)
		answered = *q
		if rp.Scope == ScopeHours || rp.Scope == ScopeAlways {
			for i := range s.reqs.Requests {
				o := &s.reqs.Requests[i]
				if o.ID == q.ID || o.State != StatePending || o.Kind != q.Kind || o.Key != q.Key || !holds(allowed, privateAddrsOf(o)) {
					continue
				}
				s.reqs.decide(o, StateAllowed, rp.By, rp.Scope, rp.Hours, fmt.Sprintf("allowed with #%d", q.ID), now)
				also = append(also, *o)
				res.Also = append(res.Also, o.ID)
			}
		}
		return wg, true, nil
	})
	for _, q := range expired {
		b.dlog.answer(now, &q, "")
	}
	if err == nil {
		err = answerErr
	}
	if err != nil {
		if rp.By == ByDialog && errors.Is(err, ErrAlreadyAnswered) {
			b.logf("the dialog's answer to request #%d is dropped: %v", id, err)
		}
		if reload {
			_ = b.Reload()
		}
		return res, err
	}
	res.Recorded = true
	res.Request = info(&answered)
	if reload {
		if err := b.Reload(); err != nil {
			b.logf("after writing request #%d's answer: %v", id, err)
		}
	}
	b.dlog.answer(now, &answered, res.Grant)
	for _, o := range also {
		b.dlog.answer(now, &o, res.Grant)
	}
	if answered.State == StateAllowed && answered.Kind == policy.KindOpenURL {
		// The requests decided with it asked to open URLs too: open each
		// URL once.
		urls := []string{answered.URL}
		for _, o := range also {
			if o.Kind == policy.KindOpenURL && !slices.Contains(urls, o.URL) {
				urls = append(urls, o.URL)
			}
		}
		for _, u := range urls {
			if err := b.opts.Opener.OpenURL(u); err != nil {
				if res.OpenError == "" {
					res.OpenError = err.Error()
				}
				b.logf("request #%d is allowed, but a URL it allows could not be opened: %v", id, err)
			}
		}
	}
	return res, nil
}

// privateAddrsOf are the private addresses of the request q stands for.
// It is the policy package's list (the addresses Denied holds, unmapped,
// without zones, each once), the one a grant must hold.
func privateAddrsOf(q *reqRec) []netip.Addr {
	return policy.NonPublicAddrs(policy.Request{Kind: q.Kind, Addrs: q.Addrs})
}

// holds reports whether every one of want is in have.
func holds(have, want []netip.Addr) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// alwaysEdit is the config.toml edit an "always" answer to q makes.
func (b *Broker) alwaysEdit(q *reqRec, set Settings) (config.Edit, error) {
	if b.opts.ConfigWriter == nil {
		return config.Edit{}, newError(KindUnavailable, "this host cannot write config.toml for an answer")
	}
	c := set.Config
	switch q.Kind {
	case policy.KindAlias:
		if !slices.ContainsFunc(c.Aliases, func(a config.Alias) bool { return a.Name == q.Alias }) {
			return config.Edit{}, newError(KindBadRequest, "config.toml has no [alias.%s] any more", clip(q.Alias, 64))
		}
		return config.Edit{Set: map[string]any{"alias." + q.Alias + ".policy": config.AliasAllow}}, nil
	case policy.KindEgress:
		if set.VMProfile == "" {
			return config.Edit{}, newError(KindBadRequest, "the sandbox runs under no vm profile, so there is no egress_allow to add %s to", clip(q.Host, 100))
		}
		key := "vm." + set.VMProfile + ".egress_allow"
		list := fileList(c, key)
		// A name the user writes into egress_allow reaches whatever it
		// resolves to, private addresses included: that is the meaning
		// "always" asks the user to confirm.
		if !slices.Contains(list, q.Host) {
			list = append(list, q.Host)
		}
		return config.Edit{Set: map[string]any{key: list}}, nil
	case policy.KindOpenURL:
		if c.Trust != config.TrustFallible {
			return config.Edit{}, newError(KindBadRequest, "trust = hostile asks before opening any URL, so open_urls_allow would change nothing: allow it once or for some hours")
		}
		key := "link.open_urls_allow"
		list := fileList(c, key)
		if !slices.Contains(list, q.Origin) {
			list = append(list, q.Origin)
		}
		return config.Edit{Set: map[string]any{key: list}}, nil
	}
	return config.Edit{}, newError(KindBadRequest, "a request of kind %q cannot be allowed always", clip(string(q.Kind), 32))
}

// fileList is the list config.toml gives key, by its dotted name: what
// is in the file, not a default.
func fileList(c *config.Config, key string) []string {
	if c.File == nil {
		return nil
	}
	vs, _ := c.File.Vals[key].([]any)
	var out []string
	for _, v := range vs {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
