package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/proposal"
)

// The schedule.
const (
	// Interval is how often a path is copied while its sandbox runs.
	Interval = 10 * time.Minute
	// TickEvery is how often a Scheduler looks.
	TickEvery = 30 * time.Second
	// StopMinAge is how old the newest checkpoint must be for the
	// sandbox's stop to take one more.
	StopMinAge = 30 * time.Second
	// PruneEvery is how often each key is pruned, running or not.
	PruneEvery = time.Hour
	// SlowTake is how long a checkpoint may run before it is logged and
	// shown as slow (it is not interrupted).
	SlowTake = 5 * time.Minute
	// NotifyEvery is the least time between two notifications of one
	// cause, for one environment and path.
	NotifyEvery = time.Hour
)

// Serving is how the daemon a Scheduler runs in serves its environment.
type Serving int

const (
	// ServeNone: another process serves the environment, or none should
	// (its link was stopped on purpose). Nothing is done.
	ServeNone Serving = iota
	// ServeLast: this daemon's link was the last to serve it and has
	// ended, and no other has taken it: the sandbox is not running. The
	// stop checkpoint and the prunes are made, never another.
	ServeLast
	// ServeHeld: this daemon holds the environment's link.lock.
	ServeHeld
)

func (s Serving) String() string {
	switch s {
	case ServeHeld:
		return "held"
	case ServeLast:
		return "last"
	}
	return "none"
}

// Unresolved is a configured path with nothing to copy, and why.
type Unresolved struct {
	Path   string
	Reason string
}

// View is a Scheduler's environment as it is at one tick: read afresh
// each time, since config.toml, the sandbox and its mounts change under
// it.
type View struct {
	Serving Serving
	// Running is whether the sandbox runs; only asked of a ServeHeld one
	// (one served otherwise is never taken as running).
	Running bool
	// Started is when the running sandbox last started, zero when it does
	// not run or that cannot be told. Two ticks that both run and see
	// different times saw a restart whose stop no tick saw.
	Started time.Time
	// Paths are config.toml's [checkpoints] paths.
	Paths []string
	// Open opens the environment's store, with its exposed-store check
	// set; nil, nil when there is none and no path is configured (nothing
	// to take or prune). The Scheduler closes it.
	Open func() (*Store, error)
	// Sources resolves the paths (only when a checkpoint is due).
	Sources func() ([]Source, []Unresolved)
}

// Env is what a Scheduler asks of its environment at every tick.
type Env interface {
	View() (View, error)
}

// TakeFunc takes one checkpoint with the store's lock held: TakeLocked.
type TakeFunc func(ctx context.Context, l *Lock, s *Store, src Source, reason string, o TakeOptions) (TakeResult, error)

// Gate lets one checkpoint run at a time across all of a daemon's
// Schedulers, so a slow copy never runs beside another. A Scheduler that
// finds it taken skips its tick.
type Gate struct{ ch chan struct{} }

// NewGate is a free Gate.
func NewGate() *Gate { return &Gate{ch: make(chan struct{}, 1)} }

// TryAcquire takes the gate if it is free.
func (g *Gate) TryAcquire() bool {
	select {
	case g.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Acquire takes the gate, waiting for it until ctx ends; it reports
// whether it took it.
func (g *Gate) Acquire(ctx context.Context) bool { return acquire(ctx, g.ch) }

// acquire takes ch, a channel of capacity 1 used as a lock, waiting until
// ctx ends; a free one is taken even when ctx has ended.
func acquire(ctx context.Context, ch chan struct{}) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
	}
	select {
	case ch <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// Release frees the gate.
func (g *Gate) Release() { <-g.ch }

// SchedulerOptions configure a Scheduler.
type SchedulerOptions struct {
	Name string // the environment's
	Env  Env
	Gate *Gate // shared by every Scheduler of the daemon; required

	Now    func() time.Time          // the clock; time.Now when nil
	Take   TakeFunc                  // TakeLocked when nil
	Notify func(title, text string)  // the user's notifications; none when nil
	Logf   func(string, ...any)      // the daemon's log; none when nil
	Rules  func(now time.Time) Rules // retention; DefaultRules when nil
	Pinned func() []ID               // never pruned; none when nil
	// Command is how a caboose command for this environment is spelt in a
	// message ("caboose -e NAME " + args when nil).
	Command func(args string) string
	// TakeOptions carries the takes' test switches (Copy, Statfs, Denied);
	// the clock, the pins and the notices are the Scheduler's.
	TakeOptions TakeOptions
}

// PathStatus is one configured path, as the Scheduler last saw it.
type PathStatus struct {
	Path     string
	Key      string
	Resolved bool   // known only once it has been resolved (Checked)
	Checked  bool   // it has been resolved at least once
	Reason   string // why it is not resolved
	Newest   *ID    // the newest complete checkpoint, if any
	NewestAt time.Time
	Method   string
	// Shrunk is the earlier checkpoint to restore from while a sharp
	// shrink stands (ShrinkFlag).
	Shrunk  string
	NextAt  time.Time // when the next one is due, while the sandbox runs
	Failing string    // the last take's error, until one succeeds
}

// Status is a Scheduler's state, for the daemon's status.
type Status struct {
	Serving         Serving
	Running         bool
	Paths           []PathStatus
	InProgressSince time.Time // a checkpoint being taken, since when
	InProgressPath  string
	LastError       string
	LastErrorAt     time.Time
	LowSpace        bool // the last take found the disk below a floor
	// SpaceSkipped says why the last due checkpoint was skipped for want of
	// space (below the soft floor), until one is taken again.
	SpaceSkipped string
	Exposed      string // the store can be reached by a sandbox: why
	Slow         string // the last checkpoint over SlowTake: which, and how long
}

// Scheduler takes one environment's checkpoints on the schedule: every
// Interval while its sandbox runs (one at the start), one when it stops,
// and prunes each key every PruneEvery, running or not. Tick does one
// round, and is called every TickEvery by whatever runs the Scheduler
// (the daemon), until it reports the environment no longer served.
type Scheduler struct {
	o SchedulerOptions

	// tick is held by one Tick or TakeNow at a time (a channel, so that
	// TakeNow's wait for it ends with its context); it guards the fields
	// below it.
	tick     chan struct{}
	ran      bool      // the sandbox ran at the last tick that finished
	started  time.Time // View.Started at the last tick that finished; zero when unknown or not running
	cleaned  bool      // leftovers were looked for since the start
	failedAt map[string]time.Time
	prunedAt map[string]time.Time
	resolved map[string]string // path: "" when it resolved, else why not
	said     map[string]bool   // path: its not resolving was notified
	failing  map[string]string // path: the last take's error, until one succeeds
	notified map[string]time.Time
	skipped  string

	mu sync.Mutex // guards st
	st Status
}

// NewScheduler is a Scheduler for o.Env; nothing runs until Tick or Run.
func NewScheduler(o SchedulerOptions) *Scheduler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Take == nil {
		o.Take = TakeLocked
	}
	if o.Rules == nil {
		o.Rules = DefaultRules
	}
	if o.Command == nil {
		name := o.Name
		o.Command = func(args string) string { return "caboose -e " + name + " " + args }
	}
	return &Scheduler{o: o, tick: make(chan struct{}, 1), failedAt: map[string]time.Time{}, prunedAt: map[string]time.Time{},
		resolved: map[string]string{}, said: map[string]bool{}, failing: map[string]string{}, notified: map[string]time.Time{}}
}

// Status is the Scheduler's state as the last tick left it.
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Paths = append([]PathStatus(nil), s.st.Paths...)
	return st
}

// Busy reports whether a checkpoint is being taken.
func (s *Scheduler) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.st.InProgressSince.IsZero()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.o.Logf != nil {
		s.o.Logf("checkpoints of %s: "+format, append([]any{s.o.Name}, args...)...)
	}
}

// recent reports whether one of es started within window before now. One
// that started after now (the clock was set back) does not count: else a
// checkpoint stamped in the future would keep any other from being due
// until the clock caught up.
func recent(es []Entry, now time.Time, window time.Duration) bool {
	for _, e := range es {
		age := now.Sub(started(e))
		if age >= 0 && age < window {
			return true
		}
	}
	return false
}

// started is when e was started: its meta.json's, else its stamp.
func started(e Entry) time.Time {
	if e.Err == nil && e.Meta != nil && !e.Meta.Started.IsZero() {
		return e.Meta.Started
	}
	return e.Stamp
}

// lately reports whether t is at most d before now, and not after it.
func lately(t, now time.Time, d time.Duration) bool {
	if t.IsZero() {
		return false
	}
	age := now.Sub(t)
	return age >= 0 && age < d
}

// testHookBeforeLock, when set (by tests only), runs in Tick between
// what looked due and the store's lock.
var testHookBeforeLock func(*Scheduler)

// due is one checkpoint a tick takes.
type due struct {
	path, key, reason string
}

// Tick does one round of the schedule, and reports whether the
// environment is still served by this daemon (Run stops when not).
func (s *Scheduler) Tick(ctx context.Context) bool {
	s.tick <- struct{}{}
	defer func() { <-s.tick }()
	now := s.o.Now()
	v, err := s.o.Env.View()
	if err != nil {
		s.fail(now, "", fmt.Sprintf("cannot read the environment: %v", err))
		s.skip("the environment cannot be read")
		return true
	}
	if v.Serving == ServeNone {
		s.setServing(v.Serving, false)
		s.setRan(false, v)
		return false
	}
	running := v.Serving == ServeHeld && v.Running
	stopping := s.ran && !running
	// A restart between two ticks: the sandbox runs at both, but started
	// again in between. It stands in for the stop no tick saw.
	restarted := running && s.ran && !v.Started.IsZero() && !s.started.IsZero() && !v.Started.Equal(s.started)
	s.setServing(v.Serving, running)

	store, err := v.Open()
	if err != nil {
		s.fail(now, "", fmt.Sprintf("cannot open the checkpoint store: %v", err))
		s.skip("the store cannot be opened")
		return true
	}
	if store == nil {
		s.mu.Lock()
		s.st.Paths = nil
		s.mu.Unlock()
		s.setRan(running, v)
		return true
	}
	defer store.Close()

	keys, _ := store.Keys()
	pruneDue := !s.cleaned
	for _, k := range keys {
		if !lately(s.prunedAt[k.Key], now, PruneEvery) {
			pruneDue = true
		}
	}
	// What looks due, without the lock; it is decided again under it.
	work := pruneDue || stopping || restarted
	if running && !work {
		for _, p := range v.Paths {
			if s.wants(store, p, now, running) {
				work = true
				break
			}
		}
	}
	if !work {
		s.refresh(store, v.Paths, now, running)
		s.setRan(running, v)
		return true
	}

	if !s.o.Gate.TryAcquire() {
		s.skipLogged(store, "another checkpoint is being taken")
		s.refresh(store, v.Paths, now, running)
		return true
	}
	defer s.o.Gate.Release()
	if testHookBeforeLock != nil {
		testHookBeforeLock(s)
	}
	l, err := store.TryLock()
	if err != nil {
		if errors.Is(err, ErrLocked) {
			s.skipLogged(store, "the store's lock is held (a checkpoint, a prune or a restore)")
		} else {
			s.fail(now, "", err.Error())
			s.skipLogged(store, "the store's lock cannot be taken")
		}
		s.refresh(store, v.Paths, now, running)
		return true
	}
	defer l.Unlock()
	s.skipped = ""

	if !s.cleaned {
		s.cleaned = true
		s.clean(store, l, now)
	}

	if restarted {
		msg := fmt.Sprintf("the sandbox restarted between two ticks (started %s, before %s)",
			v.Started.UTC().Format(time.RFC3339), s.started.UTC().Format(time.RFC3339))
		s.logf("%s", msg)
		store.Logf("start: %s", msg)
	}
	// Under the lock: what is due now.
	var todo []due
	for _, p := range v.Paths {
		if !s.wants(store, p, now, running) {
			if !stopping && !restarted {
				continue
			}
			if newest, ok := s.tooRecent(store, p, now); ok {
				// Once per transition: the next tick sees it done.
				what := ReasonStop
				if restarted {
					what = ReasonStart
				}
				store.Logf("%s: %s not taken: %s started %s ago", what, printable(p), newest.ID, now.Sub(started(newest)).Round(time.Second))
				continue
			}
			if !s.stopWants(store, p, now) {
				continue
			}
		}
		reason := ReasonInterval
		switch {
		case !running:
			reason = ReasonStop
		case !s.ran || restarted:
			reason = ReasonStart
		}
		todo = append(todo, due{p, Key(p), reason})
	}
	if len(todo) > 0 {
		s.takeAll(ctx, store, l, v, todo, now)
	}

	if ctx.Err() != nil {
		// The daemon is stopping: this tick did not happen.
		return true
	}
	if pruneDue {
		s.pruneAll(store, l, now)
	}
	s.refresh(store, v.Paths, s.o.Now(), running)
	s.setRan(running, v)
	return true
}

// setRan records what a finished tick saw: whether the sandbox ran, and
// when it started.
func (s *Scheduler) setRan(running bool, v View) {
	s.ran = running
	s.started = time.Time{}
	if running {
		s.started = v.Started
	}
}

// wants reports whether path p is due while the sandbox runs: no
// checkpoint of it started in the last Interval, and no take of it failed
// in that time.
func (s *Scheduler) wants(store *Store, p string, now time.Time, running bool) bool {
	if !running {
		return false
	}
	k := Key(p)
	if lately(s.failedAt[k], now, Interval) {
		return false
	}
	es, err := store.List(k)
	if err != nil {
		return false
	}
	return !recent(es, now, Interval)
}

// stopWants reports whether the sandbox's stop takes one more of p: none
// started in the last StopMinAge.
func (s *Scheduler) stopWants(store *Store, p string, now time.Time) bool {
	es, err := store.List(Key(p))
	if err != nil {
		return false
	}
	return !recent(es, now, StopMinAge)
}

// tooRecent is p's checkpoint that started in the last StopMinAge, which
// keeps a stop (or a restart) from taking another.
func (s *Scheduler) tooRecent(store *Store, p string, now time.Time) (Entry, bool) {
	es, err := store.List(Key(p))
	if err != nil {
		return Entry{}, false
	}
	for _, e := range es {
		if age := now.Sub(started(e)); age >= 0 && age < StopMinAge {
			return e, true
		}
	}
	return Entry{}, false
}

// takeAll takes the checkpoints of todo, one after another, under the lock.
func (s *Scheduler) takeAll(ctx context.Context, store *Store, l *Lock, v View, todo []due, now time.Time) {
	var srcs []Source
	var bad []Unresolved
	if v.Sources != nil {
		srcs, bad = v.Sources()
	}
	s.noteResolution(v.Paths, srcs, bad, now)
	for i, d := range todo {
		if ctx.Err() != nil {
			return
		}
		src, reason, ok := pick(d.path, srcs, bad)
		if !ok {
			s.failedAt[d.key] = now
			s.pathFailing(d.path, "not resolved: "+reason)
			continue
		}
		_, err := s.takeOne(ctx, store, l, src, d.reason)
		if errors.Is(err, ErrExposed) {
			// Nothing else can be taken either: the next try is at the
			// next due time.
			for _, r := range todo[i+1:] {
				s.failedAt[r.key] = now
			}
			return
		}
	}
}

// pick is path's source, or why it has none.
func pick(path string, srcs []Source, bad []Unresolved) (Source, string, bool) {
	for _, s := range srcs {
		if s.Path == path {
			return s, "", true
		}
	}
	for _, b := range bad {
		if b.Path == path {
			return Source{}, b.Reason, false
		}
	}
	return Source{}, "it did not resolve", false
}

// takeOne takes one checkpoint of src, and records and tells of what came
// of it.
func (s *Scheduler) takeOne(ctx context.Context, store *Store, l *Lock, src Source, reason string) (TakeResult, error) {
	key := Key(src.Path)
	begun := s.o.Now()
	s.mu.Lock()
	s.st.InProgressSince, s.st.InProgressPath = begun, src.Path
	s.mu.Unlock()
	o := s.o.TakeOptions
	o.Now = s.o.Now
	o.NoPrune = false
	o.SkipBelowSoft = true
	if s.o.Pinned != nil {
		o.Pinned = s.o.Pinned()
	}
	var notices []Notice
	o.Notify = func(n Notice) { notices = append(notices, n) }
	res, err := s.o.Take(ctx, l, store, src, reason, o)
	end := s.o.Now()
	s.mu.Lock()
	s.st.InProgressSince, s.st.InProgressPath = time.Time{}, ""
	s.mu.Unlock()
	if took := end.Sub(begun); took > SlowTake {
		msg := fmt.Sprintf("the checkpoint of %s took %s", src.Path, took.Round(time.Second))
		s.logf("%s", msg)
		store.Logf("slow: %s", msg)
		s.mu.Lock()
		s.st.Slow = msg
		s.mu.Unlock()
	}
	if err != nil && ctx.Err() != nil {
		// The daemon is stopping: not a failure of the checkpoint.
		s.logf("%s (%s) stopped: %s", src.Path, reason, printable(err.Error()))
		return res, err
	}
	if errors.Is(err, ErrLowSpace) {
		// Skipped, not failed: tried again at the next due time. Nothing is
		// pruned for it: the prunes go on by the rules alone.
		s.failedAt[key] = end
		text := printable(err.Error())
		s.logf("%s (%s) skipped: %s", src.Path, reason, text)
		s.mu.Lock()
		s.st.LowSpace, s.st.SpaceSkipped = true, text
		s.mu.Unlock()
		detail := strings.TrimPrefix(text, ErrLowSpace.Error()+": ")
		s.notify(end, "low-space", "", fmt.Sprintf("the disk holding %s's checkpoints is low on space (%s): no checkpoint is taken until more is free", s.o.Name, detail))
		return res, err
	}
	if err != nil {
		s.failedAt[key] = end
		text := printable(err.Error())
		s.fail(end, src.Path, text)
		s.pathFailing(src.Path, text)
		s.logf("%s (%s) failed: %s", src.Path, reason, text)
		switch {
		case errors.Is(err, ErrExposed):
			s.mu.Lock()
			s.st.Exposed = text
			s.mu.Unlock()
			s.notify(end, "exposed", "", fmt.Sprintf("%s's checkpoints are not being taken: %s", s.o.Name, text))
		case errors.Is(err, ErrDiskFull):
			s.mu.Lock()
			s.st.LowSpace = true
			s.mu.Unlock()
			s.notify(end, "disk-full", "", fmt.Sprintf("the disk holding %s's checkpoints is nearly full: the checkpoint of %s was not taken", s.o.Name, src.Path))
		default:
			s.notify(end, "failed", src.Path, fmt.Sprintf("%s's checkpoint of %s failed: %s. It is tried again in %d minutes.", s.o.Name, src.Path, text, int(Interval/time.Minute)))
		}
		return res, err
	}
	delete(s.failedAt, key)
	s.pathFailing(src.Path, "")
	s.mu.Lock()
	s.st.Exposed, s.st.LowSpace, s.st.SpaceSkipped = "", res.LowSpace, ""
	s.mu.Unlock()
	if res.PruneErr == nil {
		s.prunedAt[key] = end
	}
	for _, n := range notices {
		switch n.Kind {
		case NoticeShrunk:
			kept := "before it ages out"
			if n.Pinned {
				kept = "(it is kept until " + s.o.Command("checkpoint release "+n.Prev.String()) + ")"
			}
			s.notify(end, "shrunk", src.Path, fmt.Sprintf("%s's checkpoint of %s holds half or less of what %s held; if files were lost, restore from %s %s: %s",
				s.o.Name, src.Path, n.Prev, n.Prev, kept, s.o.Command("checkpoint restore "+n.Prev.String()+" --to DIR")))
		case NoticeLowSpace:
			s.notify(end, "low-space", "", fmt.Sprintf("the disk holding %s's checkpoints is low on space; the checkpoint of %s was still taken", s.o.Name, src.Path))
		}
	}
	return res, nil
}

// pruneAll prunes every key of the store (configured or not) that was not
// pruned in the last PruneEvery, and removes leftovers.
func (s *Scheduler) pruneAll(store *Store, l *Lock, now time.Time) {
	keys, err := store.Keys()
	if err != nil {
		s.fail(now, "", err.Error())
		return
	}
	var pinned []ID
	if s.o.Pinned != nil {
		pinned = s.o.Pinned()
	}
	pruned := false
	for _, k := range keys {
		if lately(s.prunedAt[k.Key], now, PruneEvery) {
			continue
		}
		pruned = true
		dropped, err := store.Prune(l, k.Key, s.o.Rules(now), pinned)
		s.prunedAt[k.Key] = now
		if err != nil {
			text := printable(err.Error())
			s.fail(now, k.Path, "prune: "+text)
			s.logf("prune of %s: %s", k.Path, text)
			if errors.Is(err, ErrExposed) {
				s.mu.Lock()
				s.st.Exposed = text
				s.mu.Unlock()
				s.notify(now, "exposed", "", fmt.Sprintf("%s's checkpoints are not being taken: %s", s.o.Name, text))
				return
			}
			continue
		}
		if len(dropped) > 0 {
			s.logf("pruned %d of %s", len(dropped), k.Path)
		}
	}
	if pruned {
		s.clean(store, l, now)
	}
}

// clean removes the store's leftovers older than LeftoverAge.
func (s *Scheduler) clean(store *Store, l *Lock, now time.Time) {
	removed, err := store.CleanLeftovers(l, now)
	if err != nil {
		s.logf("leftovers: %s", printable(err.Error()))
	}
	if len(removed) > 0 {
		s.logf("removed %d leftover(s)", len(removed))
	}
}

// noteResolution records which paths resolved, and tells of one that
// stopped (or never did), once until it resolves again.
func (s *Scheduler) noteResolution(paths []string, srcs []Source, bad []Unresolved, now time.Time) {
	for _, p := range paths {
		_, reason, ok := pick(p, srcs, bad)
		if ok {
			s.resolved[p], s.said[p] = "", false
			continue
		}
		reason = printable(reason)
		if reason == "" {
			reason = "it did not resolve"
		}
		s.resolved[p] = reason
		if !s.said[p] {
			s.said[p] = s.notify(now, "unresolved", p, fmt.Sprintf("%s's checkpoint path %s is not being taken: %s", s.o.Name, p, reason))
		}
	}
}

// refresh recomputes each path's status from the store.
func (s *Scheduler) refresh(store *Store, paths []string, now time.Time, running bool) {
	out := make([]PathStatus, 0, len(paths))
	for _, p := range paths {
		ps := PathStatus{Path: p, Key: Key(p), Failing: s.failing[p]}
		if why, ok := s.resolved[p]; ok {
			ps.Checked, ps.Resolved, ps.Reason = true, why == "", why
		}
		es, err := store.List(ps.Key)
		if err == nil {
			for _, e := range es {
				if e.Err == nil && e.Meta != nil {
					id := e.ID
					ps.Newest, ps.NewestAt, ps.Method = &id, started(e), e.Meta.Method
					break
				}
			}
			if _, prev := ShrinkFlag(es); prev != "" {
				ps.Shrunk = prev
			}
		}
		if running {
			next := now
			for _, e := range es {
				if t := started(e); now.Sub(t) >= 0 && t.Add(Interval).After(next) {
					next = t.Add(Interval)
				}
			}
			if f := s.failedAt[ps.Key]; lately(f, now, Interval) && f.Add(Interval).After(next) {
				next = f.Add(Interval)
			}
			ps.NextAt = next
		}
		out = append(out, ps)
	}
	s.mu.Lock()
	s.st.Paths = out
	s.mu.Unlock()
}

// ShrinkFlag is the checkpoint of es (a key's, newest first) that shrank
// sharply and the earlier one it names, while that stands: until a later
// checkpoint holds about as much as the earlier one did again, or the
// earlier one is gone. "" when none stands.
func ShrinkFlag(es []Entry) (shrunk, prev string) {
	have := map[string]bool{}
	for _, e := range es {
		have[e.ID.String()] = true
	}
	// Oldest first, so a later recovery clears an earlier flag.
	sorted := append([]Entry(nil), es...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Stamp.Before(sorted[j].Stamp) })
	var flag *Entry
	for i := range sorted {
		e := &sorted[i]
		if e.Err != nil || e.Meta == nil {
			continue
		}
		if flag != nil && !shrank(&Meta{Entries: flag.Meta.Shrunk.Entries, Bytes: flag.Meta.Shrunk.Bytes}, e.Meta.Entries, e.Meta.Bytes) {
			flag = nil
		}
		if e.Meta.Shrunk != nil && have[e.Meta.Shrunk.ID] {
			flag = e
		}
	}
	if flag == nil {
		return "", ""
	}
	return flag.ID.String(), flag.Meta.Shrunk.ID
}

// notify tells the user text, at most once per cause and path every
// NotifyEvery, and reports whether it did.
func (s *Scheduler) notify(now time.Time, cause, path, text string) bool {
	k := cause + "\x00" + path
	if lately(s.notified[k], now, NotifyEvery) {
		return false
	}
	s.notified[k] = now
	if s.o.Notify != nil {
		s.o.Notify("caboose ("+s.o.Name+")", text)
	}
	return true
}

func (s *Scheduler) fail(now time.Time, path, text string) {
	if path != "" {
		text = path + ": " + text
	}
	s.mu.Lock()
	s.st.LastError, s.st.LastErrorAt = printable(text), now
	s.mu.Unlock()
}

func (s *Scheduler) pathFailing(path, text string) {
	if text == "" {
		delete(s.failing, path)
		return
	}
	s.failing[path] = text
}

func (s *Scheduler) setServing(sv Serving, running bool) {
	s.mu.Lock()
	s.st.Serving, s.st.Running = sv, running
	s.mu.Unlock()
}

// skip records why a tick did nothing, logged when the reason changes.
func (s *Scheduler) skip(why string) {
	if why != s.skipped {
		s.logf("skipped: %s", why)
	}
	s.skipped = why
}

// skipLogged is skip, also in the store's log.
func (s *Scheduler) skipLogged(store *Store, why string) {
	if why != s.skipped {
		store.Logf("tick skipped: %s", why)
	}
	s.skip(why)
}

// printable is s safe to show, short.
func printable(s string) string { return proposal.Clip(proposal.Printable(s), 300) }

// NowResult is what TakeNow made of one path: the checkpoint taken, or
// why none was.
type NowResult struct {
	Path   string
	ID     ID
	Taken  bool
	Reason string
}

// TakeNow takes a checkpoint of each of paths now, for reason (one the
// sandbox asked for: ReasonAgent), as a tick takes its own: through the
// daemon's Gate, waiting for it until ctx ends, under the store's lock,
// with the environment's view read afresh, so it never runs beside a
// scheduled checkpoint, a prune or a restore. A path the view does not
// list is not taken; nor is any while the environment is not served here
// or its store cannot be opened or locked. Each path's result is in
// paths' order.
func (s *Scheduler) TakeNow(ctx context.Context, paths []string, reason string) []NowResult {
	out := make([]NowResult, len(paths))
	for i, p := range paths {
		out[i].Path = p
	}
	all := func(why string) []NowResult {
		for i := range out {
			out[i].Reason = why
		}
		return out
	}
	// The environment's own lock first, then its view and its sources,
	// and only then the daemon's gate: nothing that may wait on this
	// environment (its engine, a Tick of its own) is waited on while the
	// gate is held, so a hung environment holds up only itself. A Tick
	// holding the lock never waits for the gate (TryAcquire), so the two
	// cannot deadlock.
	if !acquire(ctx, s.tick) {
		return all("this environment's checkpoints are busy, and did not finish in time; try again shortly")
	}
	defer func() { <-s.tick }()
	now := s.o.Now()
	v, err := s.o.Env.View()
	switch {
	case err != nil:
		return all(fmt.Sprintf("cannot read the environment: %s", printable(err.Error())))
	case v.Serving == ServeNone:
		return all("this daemon does not serve the environment now")
	}
	var srcs []Source
	var bad []Unresolved
	if v.Sources != nil {
		srcs, bad = v.Sources()
	}
	store, err := v.Open()
	if err != nil {
		return all(fmt.Sprintf("cannot open the checkpoint store: %s", printable(err.Error())))
	}
	if store == nil {
		return all("no [checkpoints] paths are configured")
	}
	defer store.Close()
	if !s.o.Gate.Acquire(ctx) {
		return all("another checkpoint is being taken, and did not finish in time; try again shortly")
	}
	defer s.o.Gate.Release()
	l, err := store.TryLock()
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return all("the store's lock is held (a checkpoint, a prune or a restore); try again shortly")
		}
		return all(fmt.Sprintf("the store's lock cannot be taken: %s", printable(err.Error())))
	}
	defer l.Unlock()
	s.noteResolution(v.Paths, srcs, bad, now)
	for i, p := range paths {
		if ctx.Err() != nil {
			out[i].Reason = "not taken in time"
			continue
		}
		if !slices.Contains(v.Paths, p) {
			out[i].Reason = "not one of [checkpoints] paths"
			continue
		}
		src, why, ok := pick(p, srcs, bad)
		if !ok {
			out[i].Reason = "not resolved: " + printable(why)
			continue
		}
		res, err := s.takeOne(ctx, store, l, src, reason)
		if err != nil {
			out[i].Reason = printable(err.Error())
			continue
		}
		out[i].ID, out[i].Taken = res.ID, true
	}
	s.refresh(store, v.Paths, s.o.Now(), v.Serving == ServeHeld && v.Running)
	return out
}
