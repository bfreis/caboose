package broker

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
)

// Decider is what the link sees of the broker, for one link: everything
// the sandbox may ask of it, none of which changes policy. *Link is the
// broker's; tests fake it.
type Decider interface {
	// Decide is the policy's answer; an ask files (or finds) a pending
	// request.
	Decide(r policy.Request) (Outcome, error)
	// File is a request the agent makes ahead of time.
	File(a Ask) (Outcome, error)
	// List lists requests, newest first, and the cursor to Wait from.
	List(q Query) ([]RequestInfo, uint64, error)
	// Wait returns when a decision newer than since exists, or ctx ends.
	Wait(ctx context.Context, since uint64) (uint64, error)
	Aliases() ([]AliasInfo, error)
	Alias(name string) (policy.Alias, bool, error)
}

// RequestInfo is a request as it is shown: the host's effect, and the
// sandbox's words kept apart from it, made printable and clipped.
type RequestInfo struct {
	ID        uint64
	Kind      policy.Kind
	Key       string
	Effect    string
	State     string
	Scope     string
	Hours     int
	Created   time.Time
	Expires   time.Time
	DecidedAt time.Time
	DecidedBy string
	Reason    string
	Seq       uint64
	Attempts  int
	Session   string
	// Ask is what the sandbox says of it, nil when it said nothing.
	Ask *AskText
}

// clip cuts s to at most n bytes, on a character boundary.
func clip(s string, n int) string { return proposal.Clip(s, n) }

// shown is s, which the sandbox may have written, as a person is shown
// it: printable, and at most n bytes before that.
func shown(s string, n int) string { return proposal.Printable(clip(s, n)) }

func info(q *reqRec) RequestInfo {
	ri := RequestInfo{
		ID: q.ID, Kind: policy.Kind(shown(string(q.Kind), 32)), Key: shown(q.Key, 600), Effect: shown(q.Effect, 4000),
		State: q.State, Scope: q.Scope, Hours: q.Hours, Created: q.Created, Expires: q.Expires,
		DecidedAt: q.DecidedAt, DecidedBy: q.DecidedBy, Reason: shown(q.Reason, 4000), Seq: q.Seq,
		Attempts: q.Attempts, Session: shown(q.Session, MaxSession),
	}
	if a := q.Ask; a != nil {
		t := &AskText{Context: shown(a.Context, MaxContext), Recommendation: shown(a.Recommendation, MaxRecommendation), Meanwhile: shown(a.Meanwhile, MaxMeanwhile)}
		for i, o := range a.Options {
			if i == MaxOptions {
				break
			}
			t.Options = append(t.Options, shown(o, MaxOption))
		}
		ri.Ask = t
	}
	return ri
}

// Query says which requests List gives.
type Query struct {
	// Session keeps only the requests filed for this session; "" for all.
	Session string
	// All lists every request; otherwise only pending ones, and those
	// decided after Since.
	All bool
	// Since keeps only requests decided after this cursor, and pending
	// ones; 0 for no such filter.
	Since uint64
	// Limit is the most to list, at most MaxList; 0 for MaxList.
	Limit int
}

// List lists requests as q says, newest first, with the cursor a Wait
// would start from. It expires what is due first.
func (b *Broker) List(q Query) ([]RequestInfo, uint64, error) {
	if err := b.owns(); err != nil {
		return nil, 0, err
	}
	b.expireDue(b.now())
	return b.list(q)
}

// Requests is List for the host.
func (b *Broker) Requests(q Query) ([]RequestInfo, uint64, error) { return b.List(q) }

func (b *Broker) list(q Query) ([]RequestInfo, uint64, error) {
	limit := q.Limit
	if limit <= 0 || limit > MaxList {
		limit = MaxList
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []RequestInfo
	reqs := b.st.reqs.Requests
	for i := len(reqs) - 1; i >= 0; i-- {
		r := &reqs[i]
		if q.Session != "" && r.Session != q.Session {
			continue
		}
		if r.State == StatePending || q.All && q.Since == 0 || q.Since > 0 && r.Seq > q.Since {
			out = append(out, info(r))
		}
	}
	out, cur := Fit(out, q.Since, b.st.reqs.Seq, func(RequestInfo) bool { return true }, limit)
	return out, cur, nil
}

// Fit keeps of reqs what room takes, newest first: the decisions made
// after since first, oldest decision first, then the rest, newest first;
// room is asked of each in that order with what is kept so far counted by
// the caller, and at most limit are kept. When a decision after since is
// left out, the cursor cur goes back to just before the earliest one left
// out, so a caller waiting from it is told of it next, and of nothing it
// was already told: every decision it got is older.
func Fit(reqs []RequestInfo, since, cur uint64, room func(RequestInfo) bool, limit int) ([]RequestInfo, uint64) {
	var decided, rest []RequestInfo
	for _, r := range reqs {
		if since > 0 && r.State != StatePending && r.Seq > since {
			decided = append(decided, r)
		} else {
			rest = append(rest, r)
		}
	}
	slices.SortStableFunc(decided, func(a, b RequestInfo) int { return cmpID(a.Seq, b.Seq) })
	slices.SortStableFunc(rest, func(a, b RequestInfo) int { return cmpID(b.ID, a.ID) })
	var kept []RequestInfo
	for i, r := range append(decided, rest...) {
		if len(kept) == limit || !room(r) {
			if i < len(decided) {
				cur = max(decided[i].Seq-1, since)
			}
			if i < len(decided) || len(kept) == limit {
				break
			}
			continue
		}
		kept = append(kept, r)
	}
	slices.SortStableFunc(kept, func(a, b RequestInfo) int { return cmpID(b.ID, a.ID) })
	return kept, cur
}

// Request is request id, in full.
func (b *Broker) Request(id uint64) (RequestInfo, error) {
	if err := b.owns(); err != nil {
		return RequestInfo{}, err
	}
	b.expireDue(b.now())
	b.mu.Lock()
	defer b.mu.Unlock()
	if q := b.st.reqs.find(id); q != nil {
		return info(q), nil
	}
	if b.st.reqsErr != nil {
		return RequestInfo{}, newError(KindUnavailable, "%v", b.st.reqsErr)
	}
	return RequestInfo{}, newError(KindNotFound, "no request #%d in environment %s", id, b.opts.Env)
}

// wait returns when the seq passes since, or ctx ends, with the seq.
func (b *Broker) wait(ctx context.Context, since uint64) uint64 {
	for {
		b.mu.Lock()
		seq, ch := b.st.reqs.Seq, b.cond
		b.mu.Unlock()
		if seq > since {
			return seq
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return seq
		}
	}
}

// Link is the broker as one link to a sandbox sees it: a Decider whose
// Waits are capped, at MaxWaitsPerLink at once whatever sessions they
// claim.
type Link struct {
	b     *Broker
	waits atomic.Int32
}

// Link is a new handle for one link.
func (b *Broker) Link() *Link { return &Link{b: b} }

var _ Decider = (*Link)(nil)

func (l *Link) Decide(r policy.Request) (Outcome, error)      { return l.b.Decide(r) }
func (l *Link) File(a Ask) (Outcome, error)                   { return l.b.File(a) }
func (l *Link) List(q Query) ([]RequestInfo, uint64, error)   { return l.b.List(q) }
func (l *Link) Aliases() ([]AliasInfo, error)                 { return l.b.Aliases() }
func (l *Link) Alias(name string) (policy.Alias, bool, error) { return l.b.Alias(name) }

// Wait returns when a decision newer than since exists, or ctx ends,
// with the cursor. A wait past the link's MaxWaitsPerLink is refused at
// once with ErrTooManyWaits.
func (l *Link) Wait(ctx context.Context, since uint64) (uint64, error) {
	if err := l.b.owns(); err != nil {
		return 0, err
	}
	if l.waits.Add(1) > MaxWaitsPerLink {
		l.waits.Add(-1)
		return 0, newError(KindTooManyWaits, "too many waits: at most %d at once on one link", MaxWaitsPerLink)
	}
	defer l.waits.Add(-1)
	return l.b.wait(ctx, since), nil
}

// GrantInfo is a grant in force.
type GrantInfo struct {
	ID      string
	Kind    policy.Kind
	Key     string
	Once    bool
	Addrs   []string
	Created time.Time
	Expires time.Time
	Left    time.Duration
	Request uint64
}

// Grants lists the grants in force, with the time each has left.
func (b *Broker) Grants() ([]GrantInfo, error) {
	if err := b.owns(); err != nil {
		return nil, err
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.grantsErr != nil {
		return nil, newError(KindUnavailable, "%v", b.st.grantsErr)
	}
	var out []GrantInfo
	for _, g := range b.st.grants.Grants {
		if !now.Before(g.Expires) {
			continue
		}
		gi := GrantInfo{ID: g.ID, Kind: g.Kind, Key: shown(g.Key, 600), Once: g.Once, Created: g.Created, Expires: g.Expires, Left: g.Expires.Sub(now), Request: g.Request}
		for _, a := range g.Addrs {
			gi.Addrs = append(gi.Addrs, a.String())
		}
		out = append(out, gi)
	}
	return out, nil
}

// Revoke removes grant id.
func (b *Broker) Revoke(id string) error {
	if err := b.owns(); err != nil {
		return err
	}
	now := b.now()
	var gone grantRec
	err := b.txn(func(s *state) (bool, bool, error) {
		if s.grantsErr != nil {
			return false, false, newError(KindUnavailable, "%v: fix or remove it first", s.grantsErr)
		}
		i := slices.IndexFunc(s.grants.Grants, func(g grantRec) bool { return g.ID == id && now.Before(g.Expires) })
		if i < 0 {
			return false, false, newError(KindNotFound, "no grant %q in force in environment %s", clip(id, 32), b.opts.Env)
		}
		gone = s.grants.Grants[i]
		s.grants.Grants = slices.Delete(s.grants.Grants, i, i+1)
		return true, false, nil
	})
	if err == nil {
		b.dlog.revoke(now, gone)
	}
	return err
}

// Pending is how many requests wait for an answer, as last read. It reads
// nothing from disk and expires nothing, so status can ask it freely; a
// request past its time still counts until the next minute's expiry.
func (b *Broker) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for i := range b.st.reqs.Requests {
		if b.st.reqs.Requests[i].State == StatePending {
			n++
		}
	}
	return n
}
