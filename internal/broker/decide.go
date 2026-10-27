package broker

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
)

// The verdicts of an Outcome.
const (
	Allowed = "allowed"
	Denied  = "denied"
	Pending = "pending"
)

// Outcome is the broker's answer to a crossing.
type Outcome struct {
	// Verdict is Allowed, Denied or Pending.
	Verdict string
	// Reason is for the agent, written for a person.
	Reason string
	// Request is the pending request's number, when Pending.
	Request uint64
	// Rule is the policy rule that decided.
	Rule string
	// Until is when the timed grant that allowed ends; zero when no
	// timed grant allowed.
	Until time.Time
}

func denied(reason string) Outcome { return Outcome{Verdict: Denied, Reason: reason} }

// The bounds on what the sandbox may say of a request.
const (
	MaxContext        = 2000
	MaxOptions        = 5
	MaxOption         = 200
	MaxRecommendation = 500
	MaxMeanwhile      = 1000
	MaxSession        = 100

	maxAddrs = 64 // addresses kept of an egress request
)

// Ask is a request the sandbox files ahead of time: the crossing, with
// the facts the caller resolved for it on the host, and the sandbox's own
// words about it.
type Ask struct {
	Request        policy.Request
	Context        string
	Options        []string
	Recommendation string
	Meanwhile      string
	// Session is the session the sandbox says it asks for: informational,
	// and what keeps two sessions' requests apart; it never changes a
	// verdict.
	Session string
}

// check refuses text over its bounds, and any that a terminal would not
// show as it is.
func (a Ask) check() error {
	type field struct {
		name  string
		s     string
		multi bool
		max   int
	}
	fields := []field{
		{"context", a.Context, true, MaxContext},
		{"recommendation", a.Recommendation, true, MaxRecommendation},
		{"meanwhile", a.Meanwhile, true, MaxMeanwhile},
		{"session", a.Session, false, MaxSession},
	}
	if len(a.Options) > MaxOptions {
		return fmt.Errorf("at most %d options", MaxOptions)
	}
	for i, o := range a.Options {
		fields = append(fields, field{fmt.Sprintf("option %d", i+1), o, false, MaxOption})
	}
	for _, f := range fields {
		if len(f.s) > f.max {
			return fmt.Errorf("%s is longer than %d bytes", f.name, f.max)
		}
		if err := proposal.CheckText(f.name, f.s, f.multi, f.max); err != nil {
			return err
		}
	}
	return nil
}

// Decide decides a crossing: the policy's answer, a once grant it used
// consumed, an ask filed as a request (or merged into the same one
// pending). Its error is ErrNotOwner, with the Outcome denied.
func (b *Broker) Decide(r policy.Request) (Outcome, error) {
	if err := b.owns(); err != nil {
		return denied(err.Error()), err
	}
	return b.decide(r, "", nil, false), nil
}

// File is a request the sandbox makes ahead of time: decided as Decide
// decides it, and filed with the sandbox's words when it asks. Text past
// its bounds, or that a terminal would not show as it is, is denied and
// nothing is filed.
func (b *Broker) File(a Ask) (Outcome, error) {
	if err := b.owns(); err != nil {
		return denied(err.Error()), err
	}
	if err := a.check(); err != nil {
		return denied("the request is refused: " + err.Error()), nil
	}
	var text *AskText
	if a.Context != "" || len(a.Options) > 0 || a.Recommendation != "" || a.Meanwhile != "" {
		text = &AskText{Context: a.Context, Options: slices.Clone(a.Options), Recommendation: a.Recommendation, Meanwhile: a.Meanwhile}
	}
	return b.decide(a.Request, a.Session, text, true), nil
}

// decide decides r; filed is set for a request the sandbox made (File).
// A port is judged by the link on every scan, so a Decide of one never
// files and logs no ask, and a File of one never uses up a once grant:
// the link does, when it opens the forward.
func (b *Broker) decide(r policy.Request, session string, text *AskText, filed bool) Outcome {
	if r.Kind == policy.KindAlias {
		// What the sandboxes can write decides an alias: as of now.
		b.refreshWritable(writableTTL)
	}
	port := r.Kind == policy.KindPort
	now := b.now()
	b.expireDue(now)
	p, _ := b.snapshot(now)
	d := policy.Decide(p, r, now)
	var until time.Time
	if d.Verdict == policy.VerdictAllow && d.Grant != "" {
		for _, g := range p.Grants {
			if g.ID == d.Grant && !g.Once {
				until = g.Expires
			}
		}
	}
	if d.Verdict == policy.VerdictAllow && d.Grant != "" && onceGrant(p.Grants, d.Grant) && !(port && filed) {
		var err error
		if d, err = b.consume(r, now); err != nil {
			b.dlog.decision(now, r, session, d, 0)
			return Outcome{Verdict: Denied, Reason: err.Error(), Rule: d.Rule}
		}
	}
	switch d.Verdict {
	case policy.VerdictAllow:
		b.dlog.decision(now, r, session, d, 0)
		return Outcome{Verdict: Allowed, Reason: d.Reason, Rule: d.Rule, Until: until}
	case policy.VerdictAsk:
		if port && !filed {
			return Outcome{Verdict: Denied, Reason: d.Reason, Rule: d.Rule}
		}
		return b.file(r, d, p, session, text, now)
	}
	if !(port && !filed && d.Rule == policy.RulePortUnchecked) {
		b.dlog.decision(now, r, session, d, 0)
	}
	return Outcome{Verdict: Denied, Reason: d.Reason, Rule: d.Rule}
}

func onceGrant(gs []policy.Grant, id string) bool {
	for _, g := range gs {
		if g.ID == id {
			return g.Once
		}
	}
	return false
}

// consume decides r again on the grants as they are inside the lock,
// and removes the once grant that allows it, if one does: so a once
// grant allows exactly one crossing, whichever Broker or process decides
// it. One already gone means r is decided without it.
func (b *Broker) consume(r policy.Request, now time.Time) (policy.Decision, error) {
	var d policy.Decision
	err := b.txn(func(s *state) (bool, bool, error) {
		p, _ := b.snapshot(now) // b.st is s: its grants are the ones just read
		d = policy.Decide(p, r, now)
		if d.Verdict != policy.VerdictAllow || d.Grant == "" {
			return false, false, nil
		}
		i := slices.IndexFunc(s.grants.Grants, func(g grantRec) bool { return g.ID == d.Grant })
		if i < 0 || !s.grants.Grants[i].Once {
			return false, false, nil
		}
		s.grants.Grants = slices.Delete(s.grants.Grants, i, i+1)
		return true, false, nil
	})
	if err != nil {
		d = policy.Decision{Verdict: policy.VerdictDeny, Rule: "deny:grant-unusable", Reason: "the host could not use the grant that allows this: " + err.Error()}
	}
	return d, err
}

// file files r, which d says asks, as a request: merged into the pending
// one of the same kind, key, session and ExactKey (an alias's arguments,
// a connection's port), or new, within the limits. A request that
// differs is never merged into one that does not: a person allowing the
// one shown once would otherwise allow the other unseen.
func (b *Broker) file(r policy.Request, d policy.Decision, p policy.Policy, session string, text *AskText, now time.Time) Outcome {
	key := policy.GrantKey(r)
	exact := policy.ExactKey(r)
	if key == "" {
		d = policy.Decision{Verdict: policy.VerdictDeny, Rule: "deny:no-key", Reason: d.Reason}
		b.dlog.decision(now, r, session, d, 0)
		return Outcome{Verdict: Denied, Reason: d.Reason, Rule: d.Rule}
	}
	var (
		id      uint64
		isNew   bool
		refusal string
		expired []reqRec
	)
	err := b.txn(func(s *state) (bool, bool, error) {
		if s.reqsErr != nil {
			return false, false, newError(KindUnavailable, "%v", s.reqsErr)
		}
		expired = expireIn(&s.reqs, now)
		write := len(expired) > 0
		pending, recent := 0, 0
		for i := range s.reqs.Requests {
			q := &s.reqs.Requests[i]
			if q.State == StatePending {
				if q.Kind == r.Kind && q.Key == key && q.Session == session && policy.ExactKey(q.request()) == exact {
					id = q.ID
					return false, b.repeat(s, q, now) || write, nil
				}
				pending++
			}
			if now.Sub(q.Created) < time.Minute && !q.Created.After(now) {
				recent++
			}
		}
		switch {
		case pending >= b.maxPending:
			refusal = fmt.Sprintf("%s; and %d requests are already pending in this environment, the most there may be, so this one is not filed: a person answers some first (caboose requests)", d.Reason, pending)
			return false, write, nil
		case recent >= b.maxPerMinute:
			refusal = fmt.Sprintf("%s; and %d requests were filed in this environment in the last minute, the most there may be, so this one is not filed: try again shortly", d.Reason, recent)
			return false, write, nil
		}
		if s.reqs.NextID == 0 {
			s.reqs.NextID = 1
		}
		id, isNew = s.reqs.NextID, true
		s.reqs.NextID++
		s.reqs.Requests = append(s.reqs.Requests, record(p, r, key, session, text, id, now))
		return false, true, nil
	})
	for _, q := range expired {
		b.dlog.answer(now, &q, "")
	}
	if err != nil {
		d = policy.Decision{Verdict: policy.VerdictDeny, Rule: "deny:unfiled", Reason: d.Reason + "; and the host cannot file the request: " + err.Error()}
		b.dlog.decision(now, r, session, d, 0)
		return Outcome{Verdict: Denied, Reason: d.Reason, Rule: d.Rule}
	}
	if refusal != "" {
		d = policy.Decision{Verdict: policy.VerdictDeny, Rule: "deny:request-limit", Reason: refusal}
		b.dlog.decision(now, r, session, d, 0)
		return Outcome{Verdict: Denied, Reason: refusal, Rule: d.Rule}
	}
	if isNew {
		b.dlog.decision(now, r, session, d, id)
		if r.Kind == policy.KindOpenURL && b.opts.Dialog != nil {
			b.opts.Dialog.Ask(b.opts.Env, id, r.URL)
		}
	}
	return Outcome{Verdict: Pending, Reason: fmt.Sprintf("%s: request #%d waits for a person's answer", d.Reason, id), Request: id, Rule: d.Rule}
}

// heldAttempts are a pending request's repeats not yet written, and
// when this Broker last wrote its count.
type heldAttempts struct {
	n  int
	at time.Time
}

// repeat counts a repeat of the pending request q, in s read under the
// lock, and says whether requests.json is to be written for it: at most
// once an AttemptsEvery per request, the repeats in between held in
// memory and added with the next write of the file (flushAttempts). The
// request itself is in the file already; only its count waits. Under
// fileMu (txn).
func (b *Broker) repeat(s *state, q *reqRec, now time.Time) bool {
	if b.attempts == nil {
		b.attempts = map[uint64]*heldAttempts{}
	}
	h := b.attempts[q.ID]
	if h == nil {
		h = &heldAttempts{}
		b.attempts[q.ID] = h
	}
	h.n++
	if !h.at.IsZero() && now.Sub(h.at) < AttemptsEvery && !now.Before(h.at) {
		return false
	}
	h.at = now
	return true
}

// flushAttempts adds the repeats held in memory to the requests in d,
// about to be written, and forgets the ones of a request no longer
// pending; undo holds them again, for a write that failed. Under fileMu
// (txn).
func (b *Broker) flushAttempts(d *requestsDoc) (undo func()) {
	was := maps.Clone(b.attempts)
	held := map[uint64]int{}
	for id, h := range b.attempts {
		held[id] = h.n
		q := d.find(id)
		if q != nil {
			q.Attempts += h.n
		}
		h.n = 0
		if q == nil || q.State != StatePending {
			delete(b.attempts, id)
		}
	}
	return func() {
		b.attempts = was
		for id, h := range was {
			h.n = held[id]
		}
	}
}

// record is a new pending request for r.
func record(p policy.Policy, r policy.Request, key, session string, text *AskText, id uint64, now time.Time) reqRec {
	addrs := r.Addrs
	if len(addrs) > maxAddrs {
		addrs = addrs[:maxAddrs]
	}
	var args map[string]string
	if len(r.Args) > 0 {
		args = maps.Clone(r.Args)
	}
	return reqRec{
		ID: id, Kind: r.Kind, Key: key, Effect: policy.Effect(p, r),
		Created: now, Expires: now.Add(RequestTTL), State: StatePending,
		Attempts: 1, Session: session, Ask: text,
		Alias: r.Alias, Args: args, Host: r.Host, Literal: r.Literal, Port: r.Port,
		Addrs: slices.Clone(addrs), URL: r.URL, Origin: r.Origin,
	}
}

// expireIn expires every request in d pending past its time, and returns
// them as decided.
func expireIn(d *requestsDoc, now time.Time) []reqRec {
	var out []reqRec
	for i := range d.Requests {
		q := &d.Requests[i]
		if q.State == StatePending && !now.Before(q.Expires) {
			d.decide(q, StateExpired, ByExpiry, "", 0, fmt.Sprintf("no answer within %v", RequestTTL), now)
			out = append(out, *q)
		}
	}
	return out
}

// expireDue expires the requests pending past their time, if the state
// last read holds any.
func (b *Broker) expireDue(now time.Time) {
	b.mu.Lock()
	due := false
	for _, q := range b.st.reqs.Requests {
		if q.State == StatePending && !now.Before(q.Expires) {
			due = true
			break
		}
	}
	b.mu.Unlock()
	if !due {
		return
	}
	var expired []reqRec
	err := b.txn(func(s *state) (bool, bool, error) {
		expired = expireIn(&s.reqs, now)
		return false, len(expired) > 0, nil
	})
	if err != nil {
		b.logf("cannot expire requests: %v", err)
		return
	}
	for _, q := range expired {
		b.dlog.answer(now, &q, "")
	}
}

// expire expires what is due now.
func (b *Broker) expire() { b.expireDue(b.now()) }
