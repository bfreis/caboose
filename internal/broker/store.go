package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/policy"
)

// fileVersion is the version grants.json and requests.json are written
// with; a file of another is refused, never rewritten.
const fileVersion = 1

// grantRec is a grant as grants.json keeps it.
type grantRec struct {
	ID      string       `json:"id"`
	Kind    policy.Kind  `json:"kind"`
	Key     string       `json:"key"`
	Once    bool         `json:"once"`
	Addrs   []netip.Addr `json:"addrs,omitempty"`
	Created time.Time    `json:"created"`
	Expires time.Time    `json:"expires"`
	Request uint64       `json:"request"`
}

type grantsDoc struct {
	Version int        `json:"version"`
	NextID  uint64     `json:"next_id"`
	Grants  []grantRec `json:"grants"`
}

// The states of a request.
const (
	StatePending = "pending"
	StateAllowed = "allowed"
	StateDenied  = "denied"
	StateExpired = "expired"
)

// Who decided a request.
const (
	ByCLI    = "cli"    // the caboose command
	ByWeb    = "web"    // the daemon's web page
	ByDialog = "dialog" // the URL dialog
	ByExpiry = "expiry" // nobody: it expired
)

// The scopes of an answer.
const (
	ScopeOnce   = "once"
	ScopeHours  = "hours"
	ScopeAlways = "always"
)

// AskText is what the sandbox said of a request it filed ahead of time:
// its own words, shown under "the sandbox says:", never as the effect.
type AskText struct {
	Context        string   `json:"context,omitempty"`
	Options        []string `json:"options,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Meanwhile      string   `json:"meanwhile,omitempty"`
}

// reqRec is a request as requests.json keeps it: its state, and the
// facts the host resolved for it, which an answer acts on.
type reqRec struct {
	ID        uint64      `json:"id"`
	Kind      policy.Kind `json:"kind"`
	Key       string      `json:"key"`
	Effect    string      `json:"effect"`
	Created   time.Time   `json:"created"`
	Expires   time.Time   `json:"expires"`
	State     string      `json:"state"`
	Scope     string      `json:"scope,omitempty"`
	Hours     int         `json:"hours,omitempty"`
	DecidedAt time.Time   `json:"decided_at,omitzero"`
	DecidedBy string      `json:"decided_by,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	Seq       uint64      `json:"seq"`
	Attempts  int         `json:"attempts"`
	Session   string      `json:"session,omitempty"`
	Ask       *AskText    `json:"ask,omitempty"`
	// InFlight is who shows the request's URL dialog right now, while it
	// is pending: ByDialog (this machine's own dialog). A broker that
	// opens to find it set denies the request (ReasonDialogGone): the
	// dialog is gone, and it is never asked again another way.
	InFlight string `json:"in_flight,omitempty"`

	// The facts.
	Alias   string            `json:"alias,omitempty"`
	Args    map[string]string `json:"args,omitempty"`
	Host    string            `json:"host,omitempty"`
	Literal bool              `json:"literal,omitempty"`
	Port    int               `json:"port,omitempty"`
	Addrs   []netip.Addr      `json:"addrs,omitempty"`
	URL     string            `json:"url,omitempty"`
	Origin  string            `json:"origin,omitempty"`
}

// request is the policy.Request r stands for, as far as an answer needs.
func (r *reqRec) request() policy.Request {
	return policy.Request{Kind: r.Kind, Alias: r.Alias, Args: r.Args, Host: r.Host, Literal: r.Literal,
		Port: r.Port, Addrs: r.Addrs, URL: r.URL, Origin: r.Origin}
}

type requestsDoc struct {
	Version  int      `json:"version"`
	NextID   uint64   `json:"next_id"`
	Seq      uint64   `json:"seq"`
	Requests []reqRec `json:"requests"`
}

// state is both files as last read. An Err is why that file cannot be
// used: its contents then count as empty, and it is never rewritten.
type state struct {
	grants    grantsDoc
	grantsErr error
	reqs      requestsDoc
	reqsErr   error
}

// readDoc reads name into v. A missing file is an empty one; anything
// else that cannot be read whole -- a symlink, a hard link, a corrupt or
// oversized file, another version -- is an error.
func (b *Broker) readDoc(name string, v any, version func() int) error {
	data, _, err := b.dir.ReadFileMax(name, maxStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s/%s cannot be read: %w", b.envDir, name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s/%s is not valid: %v", b.envDir, name, err)
	}
	if dec.More() {
		return fmt.Errorf("%s/%s is not valid: data after its object", b.envDir, name)
	}
	if n := version(); n != fileVersion {
		return fmt.Errorf("%s/%s is version %d, which this caboose does not know (it knows %d); a newer caboose wrote it", b.envDir, name, n, fileVersion)
	}
	return nil
}

// readState reads both files, logging one that cannot be used.
func (b *Broker) readState() state {
	var s state
	if err := b.readDoc(grantsFile, &s.grants, func() int { return s.grants.Version }); err != nil {
		s.grants, s.grantsErr = grantsDoc{}, err
		b.logf("%v; no grant applies, and it is not rewritten, until it is fixed or removed", err)
	}
	if err := b.readDoc(requestsFile, &s.reqs, func() int { return s.reqs.Version }); err != nil {
		s.reqs, s.reqsErr = requestsDoc{}, err
		b.logf("%v; no request is filed or answered, and it is not rewritten, until it is fixed or removed", err)
	}
	if s.grants.Version == 0 {
		s.grants.Version = fileVersion
	}
	if s.reqs.Version == 0 {
		s.reqs.Version = fileVersion
	}
	// The cursor starts at 1, so that 0 can mean "no cursor" and a
	// decision always comes after any cursor handed out.
	if s.reqs.Seq == 0 {
		s.reqs.Seq = 1
	}
	return s
}

// writeDoc writes v to name whole: a new file beside it, renamed over
// it, mode 0600; a symlink at name is replaced, never written through.
func (b *Broker) writeDoc(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if writeFault != nil {
		if err := writeFault(name); err != nil {
			return err
		}
	}
	return b.dir.WriteFile(name, append(data, '\n'), 0o600)
}

// writeFault, when set (by the tests), can fail a write of the file named.
var writeFault func(name string) error

// txn is a read-modify-write of the state, under fileMu and the flock on
// broker.lock, with the files read again inside them. fn changes s and
// says which files to write; a file that cannot be used is never written.
// The state read (and written, if it was) is kept as b.st.
func (b *Broker) txn(fn func(s *state) (grants, reqs bool, err error)) error {
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	if err := flock(int(b.lock.Fd()), unix.LOCK_EX); err != nil {
		return newError(KindUnavailable, "cannot lock %s/%s: %v", b.envDir, lockFile, err)
	}
	defer func() { _ = flock(int(b.lock.Fd()), unix.LOCK_UN) }()
	s := b.readState()
	b.setState(s)
	wg, wr, err := fn(&s)
	if err != nil {
		return err
	}
	now := b.now()
	if wg {
		if s.grantsErr != nil {
			return newError(KindUnavailable, "%v: fix or remove it first", s.grantsErr)
		}
		s.grants.Grants = slices.DeleteFunc(s.grants.Grants, func(g grantRec) bool { return !now.Before(g.Expires) })
		if err := b.writeDoc(grantsFile, &s.grants); err != nil {
			return newError(KindUnavailable, "cannot write %s/%s: %v", b.envDir, grantsFile, err)
		}
	}
	if wr {
		if s.reqsErr != nil {
			return newError(KindUnavailable, "%v: fix or remove it first", s.reqsErr)
		}
		undo := b.flushAttempts(&s.reqs)
		pruneDecided(&s.reqs)
		if err := b.writeDoc(requestsFile, &s.reqs); err != nil {
			undo()
			return newError(KindUnavailable, "cannot write %s/%s: %v", b.envDir, requestsFile, err)
		}
	}
	b.setState(s)
	return nil
}

// setState keeps s as the state last read, waking waiters when its seq
// moved. It keeps its own copy of the lists: a txn goes on changing s's
// in place, while readers look at b.st.
func (b *Broker) setState(s state) {
	s.grants.Grants = slices.Clone(s.grants.Grants)
	s.reqs.Requests = slices.Clone(s.reqs.Requests)
	b.mu.Lock()
	defer b.mu.Unlock()
	moved := s.reqs.Seq != b.st.reqs.Seq
	b.st = s
	if moved {
		close(b.cond)
		b.cond = make(chan struct{})
	}
}

// pruneDecided keeps every pending request and the KeepDecided most
// recently decided, in the order of their numbers.
func pruneDecided(d *requestsDoc) {
	var decided []reqRec
	for _, r := range d.Requests {
		if r.State != StatePending {
			decided = append(decided, r)
		}
	}
	if len(decided) <= KeepDecided {
		return
	}
	slices.SortFunc(decided, func(a, b reqRec) int {
		if c := a.DecidedAt.Compare(b.DecidedAt); c != 0 {
			return -c
		}
		return cmpID(b.ID, a.ID)
	})
	drop := map[uint64]bool{}
	for _, r := range decided[KeepDecided:] {
		drop[r.ID] = true
	}
	d.Requests = slices.DeleteFunc(d.Requests, func(r reqRec) bool { return drop[r.ID] })
}

func cmpID(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// find is the request numbered id in d, nil if there is none.
func (d *requestsDoc) find(id uint64) *reqRec {
	for i := range d.Requests {
		if d.Requests[i].ID == id {
			return &d.Requests[i]
		}
	}
	return nil
}

// decide records r as decided: state, by whom, why, and a new seq.
func (d *requestsDoc) decide(r *reqRec, st, by, scope string, hours int, reason string, now time.Time) {
	d.Seq++
	r.State, r.DecidedBy, r.Scope, r.Hours, r.Reason, r.DecidedAt, r.Seq = st, by, scope, hours, reason, now, d.Seq
	r.InFlight = ""
}

// policyGrants are the grants in g that may still match at now.
func policyGrants(g []grantRec, now time.Time) []policy.Grant {
	var out []policy.Grant
	for _, r := range g {
		if now.Before(r.Expires) {
			out = append(out, policy.Grant{ID: r.ID, Kind: r.Kind, Key: r.Key, Once: r.Once, Expires: r.Expires, Addrs: r.Addrs})
		}
	}
	return out
}
