package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/saferm"
)

// manualClock is a clock that moves only when told.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *manualClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// schedT0 is a fixed instant, on the hour, the tests' clocks start at.
var schedT0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// fakeEnv is an environment for a Scheduler: its store under a temp
// CABOOSE_HOME, one source in a temp directory, and a state the test sets.
type fakeEnv struct {
	mu       sync.Mutex
	home     string
	envDir   string
	serving  Serving
	running  bool
	started  time.Time
	paths    []string
	srcs     []Source
	bad      []Unresolved
	exposed  error
	viewErr  error
	viewing  chan struct{} // when set, View tells it, then waits for unblock
	unblock  chan struct{}
	opened   int
	resolved int
}

func newFakeEnv(t *testing.T, name string) (*fakeEnv, string) {
	t.Helper()
	home := t.TempDir()
	envDir := config.EnvDirFor(home, name)
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeWritable(t, home)
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "proj", "a.txt"), "alpha", 0o644)
	src := rootSource(top, "proj")
	return &fakeEnv{home: home, envDir: envDir, serving: ServeHeld, running: true,
		paths: []string{src.Path}, srcs: []Source{src}}, filepath.Join(top, "proj")
}

func (f *fakeEnv) set(serving Serving, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serving, f.running = serving, running
}

func (f *fakeEnv) setStarted(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = at
}

// storeLog is what the store's checkpoints.log holds.
func (f *fakeEnv) storeLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(StoreDir(f.envDir), logName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fakeEnv) View() (View, error) {
	f.mu.Lock()
	viewing, unblock := f.viewing, f.unblock
	f.mu.Unlock()
	if viewing != nil {
		viewing <- struct{}{}
		<-unblock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.viewErr != nil {
		return View{}, f.viewErr
	}
	v := View{Serving: f.serving, Running: f.running, Started: f.started, Paths: slices.Clone(f.paths)}
	v.Open = func() (*Store, error) {
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
		s, err := Open(f.home, f.envDir)
		if err != nil {
			return nil, err
		}
		s.Exposed = func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.exposed
		}
		return s, nil
	}
	v.Sources = func() ([]Source, []Unresolved) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.resolved++
		return slices.Clone(f.srcs), slices.Clone(f.bad)
	}
	return v, nil
}

// store opens f's store, for the test to look at.
func (f *fakeEnv) store(t *testing.T) *Store {
	t.Helper()
	s, err := Open(f.home, f.envDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Exposed = func() error { return nil }
	return s
}

// list is the stamps and reasons of key's checkpoints, oldest first.
func (f *fakeEnv) list(t *testing.T, key string) []string {
	t.Helper()
	es, err := f.store(t).List(key)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(es) - 1; i >= 0; i-- {
		r := "?"
		if es[i].Meta != nil {
			r = es[i].Meta.Reason
		}
		out = append(out, FormatStamp(es[i].Stamp)+" "+r)
	}
	return out
}

// taker counts the takes it is asked for, and fails or blocks them as told.
type taker struct {
	mu      sync.Mutex
	reasons []string
	fail    error
	block   chan struct{} // when set, a take waits for it to close
	begun   chan struct{} // told when a take begins
}

func (k *taker) take(ctx context.Context, l *Lock, s *Store, src Source, reason string, o TakeOptions) (TakeResult, error) {
	k.mu.Lock()
	k.reasons = append(k.reasons, reason)
	fail, block, begun := k.fail, k.block, k.begun
	k.mu.Unlock()
	if begun != nil {
		begun <- struct{}{}
	}
	if block != nil {
		<-block
	}
	if fail != nil {
		return TakeResult{}, fail
	}
	return TakeLocked(ctx, l, s, src, reason, o)
}

func (k *taker) calls() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.reasons)
}

// notes records notifications.
type notes struct {
	mu   sync.Mutex
	list []string
}

func (n *notes) add(title, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, title+": "+text)
}

func (n *notes) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.list)
}

func (n *notes) count(sub string) int {
	c := 0
	for _, s := range n.got() {
		if strings.Contains(s, sub) {
			c++
		}
	}
	return c
}

type schedWorld struct {
	env   *fakeEnv
	src   string // the source directory
	clock *manualClock
	take  *taker
	notes *notes
	s     *Scheduler
}

func newSchedWorld(t *testing.T, gate *Gate) *schedWorld {
	t.Helper()
	skipRoot(t)
	env, src := newFakeEnv(t, "alpha")
	return newSchedWorldOn(t, env, src, gate)
}

func newSchedWorldOn(t *testing.T, env *fakeEnv, src string, gate *Gate) *schedWorld {
	t.Helper()
	if gate == nil {
		gate = NewGate()
	}
	w := &schedWorld{env: env, src: src, clock: &manualClock{t: schedT0}, take: &taker{}, notes: &notes{}}
	w.s = NewScheduler(SchedulerOptions{Name: "alpha", Env: env, Gate: gate, Now: w.clock.now, Take: w.take.take,
		Notify: w.notes.add, TakeOptions: TakeOptions{Copy: walkerOnly, Statfs: plenty}})
	return w
}

func (w *schedWorld) tick(t *testing.T) {
	t.Helper()
	w.s.Tick(t.Context())
}

// ticks ticks every 30 s for d, starting one step after now.
func (w *schedWorld) ticks(t *testing.T, d time.Duration) {
	t.Helper()
	for step := time.Duration(0); step < d; step += TickEvery {
		w.clock.add(TickEvery)
		w.tick(t)
	}
}

func (w *schedWorld) key() string { return Key(w.env.paths[0]) }

// While the sandbox runs: one at the first tick (start), then one every
// 10 minutes (interval), never in between.
func TestScheduleInterval(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.tick(t)
	if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
		t.Fatalf("first tick took %v", got)
	}
	w.ticks(t, Interval-TickEvery)
	if got := w.take.calls(); len(got) != 1 {
		t.Fatalf("took %v before 10 minutes", got)
	}
	w.clock.add(TickEvery)
	w.tick(t)
	if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart, ReasonInterval}) {
		t.Fatalf("at 10 minutes took %v", got)
	}
	if got := w.env.list(t, w.key()); len(got) != 2 || !strings.HasSuffix(got[0], "start") || !strings.HasSuffix(got[1], "interval") {
		t.Fatalf("store holds %v", got)
	}
}

// Nothing is taken while the sandbox is not running, held or not.
func TestScheduleNoneWhileStopped(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.env.set(ServeHeld, false)
	w.tick(t)
	w.ticks(t, 30*time.Minute)
	w.env.set(ServeLast, true) // only a held environment is taken as running
	w.ticks(t, 30*time.Minute)
	if got := w.take.calls(); len(got) != 0 {
		t.Fatalf("took %v while stopped", got)
	}
}

// The sandbox's stop takes one more, when the newest is older than 30 s;
// whether the link still holds link.lock or has just let go of it.
func TestScheduleStop(t *testing.T) {
	for _, after := range []Serving{ServeHeld, ServeLast} {
		t.Run(after.String(), func(t *testing.T) {
			w := newSchedWorld(t, nil)
			w.tick(t)
			w.ticks(t, 5*time.Minute)
			w.env.set(after, false)
			w.clock.add(TickEvery)
			w.tick(t)
			if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart, ReasonStop}) {
				t.Fatalf("took %v", got)
			}
			w.ticks(t, 30*time.Minute)
			if got := w.take.calls(); len(got) != 2 {
				t.Fatalf("took %v after the stop", got)
			}
		})
	}
	t.Run("recent", func(t *testing.T) {
		w := newSchedWorld(t, nil)
		w.tick(t)
		w.clock.add(10 * time.Second)
		w.env.set(ServeLast, false)
		w.tick(t)
		if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
			t.Fatalf("a stop 10 s after the last checkpoint took %v", got)
		}
	})
}

// A stop within StopMinAge of the newest takes none, and the store's log
// says so, once.
func TestScheduleStopRecentLogged(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.tick(t)
	w.clock.add(25 * time.Second)
	w.env.set(ServeLast, false)
	w.tick(t)
	w.ticks(t, 5*time.Minute)
	if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
		t.Fatalf("took %v", got)
	}
	log := w.env.storeLog(t)
	want := "stop: " + w.env.paths[0] + " not taken: " + w.key() + "@" + FormatStamp(schedT0) + " started 25s ago"
	if strings.Count(log, want) != 1 || strings.Count(log, "not taken") != 1 {
		t.Fatalf("log %q, want %q once", log, want)
	}
}

// A restart quick enough that no tick saw the sandbox stopped is told by
// its start time: one checkpoint, reason start, though the interval has
// not passed, standing in for the stop no tick saw.
func TestScheduleRestartMissed(t *testing.T) {
	t.Run("seen", func(t *testing.T) {
		w := newSchedWorld(t, nil)
		w.env.setStarted(schedT0.Add(-time.Hour))
		w.tick(t)
		w.ticks(t, 3*time.Minute)
		w.env.setStarted(w.clock.now().Add(10 * time.Second))
		w.clock.add(TickEvery)
		w.tick(t)
		if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart, ReasonStart}) {
			t.Fatalf("took %v", got)
		}
		if got := w.env.list(t, w.key()); len(got) != 2 || !strings.HasSuffix(got[1], "start") {
			t.Fatalf("store holds %v", got)
		}
		if log := w.env.storeLog(t); strings.Count(log, "restarted between two ticks") != 1 {
			t.Fatalf("log %q", log)
		}
		// Then on the schedule again: the next is an interval, 10 minutes on.
		w.ticks(t, Interval-TickEvery)
		if got := w.take.calls(); len(got) != 2 {
			t.Fatalf("took %v", got)
		}
		w.clock.add(TickEvery)
		w.tick(t)
		if got := w.take.calls(); len(got) != 3 || got[2] != ReasonInterval {
			t.Fatalf("took %v", got)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		for _, c := range []struct{ before, after time.Time }{
			{time.Time{}, schedT0.Add(time.Minute)},
			{schedT0.Add(-time.Hour), time.Time{}},
		} {
			w := newSchedWorld(t, nil)
			w.env.setStarted(c.before)
			w.tick(t)
			w.ticks(t, 3*time.Minute)
			w.env.setStarted(c.after)
			w.ticks(t, 3*time.Minute)
			if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
				t.Fatalf("%v -> %v: took %v", c.before, c.after, got)
			}
		}
	})
	t.Run("recent", func(t *testing.T) {
		w := newSchedWorld(t, nil)
		w.env.setStarted(schedT0.Add(-time.Hour))
		w.tick(t)
		w.clock.add(20 * time.Second)
		w.env.setStarted(w.clock.now().Add(-5 * time.Second))
		w.tick(t)
		w.ticks(t, 3*time.Minute)
		if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
			t.Fatalf("took %v", got)
		}
		if log := w.env.storeLog(t); !strings.Contains(log, "start: "+w.env.paths[0]+" not taken: ") || !strings.Contains(log, "started 20s ago") {
			t.Fatalf("log %q", log)
		}
	})
}

// After the sandbox starts again, the first one is a start.
func TestScheduleRestart(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.tick(t)
	w.env.set(ServeLast, false)
	w.ticks(t, time.Hour)
	w.env.set(ServeHeld, true)
	w.clock.add(TickEvery)
	w.tick(t)
	if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart, ReasonStop, ReasonStart}) {
		t.Fatalf("took %v", got)
	}
}

// An environment this daemon does not serve gets nothing done, and the
// tick says so.
func TestScheduleNotServed(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.env.set(ServeNone, true)
	if w.s.Tick(t.Context()) {
		t.Fatal("an unserved environment's tick reports it served")
	}
	if got := w.take.calls(); len(got) != 0 || w.env.opened != 0 {
		t.Fatalf("took %v, opened the store %d times", got, w.env.opened)
	}
	// It stops being served while running: no stop checkpoint either.
	w.env.set(ServeHeld, true)
	w.tick(t)
	w.env.set(ServeNone, false)
	w.clock.add(time.Minute)
	w.tick(t)
	w.env.set(ServeLast, false)
	w.clock.add(time.Minute)
	w.tick(t)
	if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
		t.Fatalf("took %v", got)
	}
}

// A failed take is tried again at the next due time, not at every tick,
// and notified once an hour.
func TestScheduleFailureRetry(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.take.fail = errors.New("boom")
	w.tick(t)
	w.ticks(t, Interval-TickEvery)
	if got := w.take.calls(); len(got) != 1 {
		t.Fatalf("a failure was tried again within 10 minutes: %v", got)
	}
	w.clock.add(TickEvery)
	w.tick(t)
	if got := w.take.calls(); len(got) != 2 {
		t.Fatalf("not tried again at 10 minutes: %v", got)
	}
	if n := w.notes.count("failed: boom"); n != 1 {
		t.Fatalf("%d failure notifications within the hour: %v", n, w.notes.got())
	}
	st := w.s.Status()
	if !strings.Contains(st.LastError, "boom") || st.LastErrorAt.IsZero() || !strings.Contains(st.Paths[0].Failing, "boom") {
		t.Fatalf("status %+v", st)
	}
	w.ticks(t, time.Hour)
	if n := w.notes.count("failed: boom"); n != 2 {
		t.Fatalf("%d failure notifications after an hour: %v", n, w.notes.got())
	}
	w.take.mu.Lock()
	w.take.fail = nil
	w.take.mu.Unlock()
	w.ticks(t, Interval)
	if st := w.s.Status(); st.Paths[0].Failing != "" || st.Paths[0].Newest == nil {
		t.Fatalf("status after a success %+v", st.Paths[0])
	}
}

// One checkpoint at a time across a daemon's environments: a schedule that
// finds the gate taken skips its tick, and takes at the next.
func TestScheduleOneAtATime(t *testing.T) {
	gate := NewGate()
	a, b := newSchedWorld(t, gate), newSchedWorld(t, gate)
	a.take.block, a.take.begun = make(chan struct{}), make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.tick(t)
	}()
	<-a.take.begun
	if !a.s.Busy() || a.s.Status().InProgressSince.IsZero() || a.s.Status().InProgressPath == "" {
		t.Error("a take in progress is not shown")
	}
	b.tick(t)
	if got := b.take.calls(); len(got) != 0 {
		t.Fatalf("a second checkpoint ran beside the first: %v", got)
	}
	close(a.take.block)
	<-done
	if a.s.Busy() {
		t.Error("busy after the take")
	}
	b.clock.add(TickEvery)
	b.tick(t)
	if got := b.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
		t.Fatalf("the skipped schedule then took %v", got)
	}
}

// A tick finding the store's lock held (a restore, a caboose checkpoint
// create, another daemon) is skipped.
func TestScheduleLockHeld(t *testing.T) {
	w := newSchedWorld(t, nil)
	l, err := w.env.store(t).TryLock()
	if err != nil {
		t.Fatal(err)
	}
	w.tick(t)
	if got := w.take.calls(); len(got) != 0 {
		t.Fatalf("took %v with the lock held", got)
	}
	l.Unlock()
	w.clock.add(TickEvery)
	w.tick(t)
	if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart}) {
		t.Fatalf("took %v after", got)
	}
}

// What is due is decided again under the store's lock: two daemons
// sharing a CABOOSE_HOME both find a path due, and one checkpoint is
// taken.
func TestScheduleDueUnderLock(t *testing.T) {
	a := newSchedWorld(t, nil)
	b := newSchedWorldOn(t, a.env, a.src, NewGate())
	b.clock = a.clock
	b.s.o.Now = a.clock.now
	t.Cleanup(func() { testHookBeforeLock = nil })
	testHookBeforeLock = func(s *Scheduler) {
		if s == b.s {
			testHookBeforeLock = nil
			a.tick(t)
		}
	}
	b.tick(t)
	if got := a.env.list(t, a.key()); len(got) != 1 {
		t.Fatalf("two daemons took %v", got)
	}
	if got := b.take.calls(); len(got) != 0 {
		t.Fatalf("the second took %v", got)
	}
}

// takeAt has the store hold a checkpoint of w's source taken at at.
func (w *schedWorld) takeAt(t *testing.T, at time.Time) ID {
	t.Helper()
	s := w.env.store(t)
	l, err := s.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	res, err := TakeLocked(t.Context(), l, s, w.env.srcs[0], ReasonInterval,
		TakeOptions{NoPrune: true, Now: func() time.Time { return at }, Copy: walkerOnly, Statfs: plenty})
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

// Every hour, running or not, each key is pruned by the retention rules:
// what they keep stays (and a pinned checkpoint), what they drop goes.
func TestSchedulePruneHourly(t *testing.T) {
	for _, pin := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned=%v", pin), func(t *testing.T) {
			w := newSchedWorld(t, nil)
			w.env.set(ServeLast, false)
			at := func(h, m int) time.Time {
				return schedT0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
			}
			older := w.takeAt(t, at(-3, 10))    // 09:10, same UTC hour as the next: dropped
			newer := w.takeAt(t, at(-3, 40))    // 09:40: kept
			lone := w.takeAt(t, at(-4, 20))     // 08:20: kept
			ancient := w.takeAt(t, at(-240, 0)) // 10 days: dropped
			recentID := w.takeAt(t, at(0, -20)) // 20 min: kept
			if pin {
				w.s.o.Pinned = func() []ID { return []ID{older} }
			}
			w.tick(t)
			want := []ID{lone, newer, recentID}
			if pin {
				want = []ID{lone, older, newer, recentID}
			}
			if got := ids(t, w.env.store(t), w.key()); !slices.Equal(got, idStrings(want)) {
				t.Fatalf("kept %v, want %v (dropped %v)", got, idStrings(want), ancient)
			}
			// Not again within the hour.
			more := w.takeAt(t, at(-300, 0))
			w.ticks(t, 30*time.Minute)
			if got := ids(t, w.env.store(t), w.key()); !slices.Contains(got, more.String()) {
				t.Fatalf("pruned again within the hour: %v", got)
			}
			w.ticks(t, 31*time.Minute)
			if got := ids(t, w.env.store(t), w.key()); slices.Contains(got, more.String()) {
				t.Fatalf("not pruned after an hour: %v", got)
			}
			if got := w.take.calls(); len(got) != 0 {
				t.Fatalf("took %v while stopped", got)
			}
		})
	}
}

func ids(t *testing.T, s *Store, key string) []string {
	t.Helper()
	es, err := s.List(key)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(es) - 1; i >= 0; i-- {
		out = append(out, es[i].ID.String())
	}
	return out
}

func idStrings(l []ID) []string {
	var out []string
	for _, id := range l {
		out = append(out, id.String())
	}
	return out
}

// Keys whose path is no longer configured are pruned by the same rules.
func TestSchedulePrunesUnconfigured(t *testing.T) {
	w := newSchedWorld(t, nil)
	ancient := w.takeAt(t, schedT0.Add(-18*24*time.Hour))
	newest := w.takeAt(t, schedT0.Add(-10*24*time.Hour))
	w.env.paths = nil
	w.env.set(ServeLast, false)
	w.tick(t)
	if got := ids(t, w.env.store(t), ancient.Key); len(got) != 1 || got[0] != newest.String() {
		t.Fatalf("an unconfigured key kept %v, want only its newest %s", got, newest)
	}
}

// A clock set back: the newest checkpoint started in the future, which is
// due (not "not for another day"); the one taken then is the one that
// counts, so the next is 10 minutes later, not at every tick.
func TestScheduleClockSetBack(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.takeAt(t, schedT0.Add(24*time.Hour))
	w.tick(t)
	if got := w.take.calls(); len(got) != 1 {
		t.Fatalf("a checkpoint stamped in the future kept any other from being due: %v", got)
	}
	w.ticks(t, Interval-TickEvery)
	if got := w.take.calls(); len(got) != 1 {
		t.Fatalf("took %v within 10 minutes", got)
	}
	w.clock.add(TickEvery)
	w.tick(t)
	if got := w.take.calls(); len(got) != 2 {
		t.Fatalf("took %v at 10 minutes", got)
	}
	// A failure stamped in the future does not hold the next try back.
	w.take.fail = errors.New("boom")
	w.ticks(t, Interval)
	w.clock.add(-48 * time.Hour)
	w.take.fail = nil
	n := len(w.take.calls())
	w.tick(t)
	if got := w.take.calls(); len(got) != n+1 {
		t.Fatalf("a failure in the future held the next one back: %v", got)
	}
}

// The first tick removes the leftovers older than an hour, younger ones
// stay; while stopped, too.
func TestScheduleLeftovers(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.env.set(ServeLast, false)
	s := w.env.store(t)
	old := filepath.Join(s.Dir(), tmpPrefix+w.key()+"-20261018T100000Z-abc")
	young := filepath.Join(s.Dir(), trashPrefix+w.key()+"-20261018T115900Z-def")
	for _, d := range []string{old, young} {
		if err := os.MkdirAll(filepath.Join(d, "tree"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// CleanLeftovers judges by the real clock's mtime.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	w.clock.set(time.Now())
	w.tick(t)
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an old leftover stayed: %v", err)
	}
	if _, err := os.Lstat(young); err != nil {
		t.Fatalf("a young leftover went: %v", err)
	}
}

// A sharp shrink notifies at once, naming the earlier checkpoint and the
// command to restore it, once per hour; the status flags it while it
// stands.
func TestScheduleShrink(t *testing.T) {
	w := newSchedWorld(t, nil)
	for i := range 1200 {
		mustWrite(t, filepath.Join(w.src, "many", fmt.Sprintf("f%04d", i)), "x", 0o644)
	}
	w.tick(t)
	first := w.s.Status().Paths[0].Newest
	if first == nil {
		t.Fatal("no first checkpoint")
	}
	if err := saferm.RemoveAll(w.src, filepath.Join(w.src, "many")); err != nil {
		t.Fatal(err)
	}
	w.ticks(t, Interval)
	if n := w.notes.count("holds half or less of what " + first.String()); n != 1 {
		t.Fatalf("%d shrink notifications: %v", n, w.notes.got())
	}
	if n := w.notes.count("checkpoint restore " + first.String() + " --to DIR"); n != 1 {
		t.Fatalf("the notification does not say how to restore: %v", w.notes.got())
	}
	if n := w.notes.count("kept until caboose -e alpha checkpoint release " + first.String()); n != 1 {
		t.Fatalf("the notification does not say it is kept: %v", w.notes.got())
	}
	if ps, err := w.env.store(t).Pins(); err != nil || len(ps) != 1 || ps[0].ID != first.String() {
		t.Fatalf("pins %+v %v", ps, err)
	}
	if st := w.s.Status(); st.Paths[0].Shrunk != first.String() {
		t.Fatalf("status does not flag the shrink: %+v", st.Paths[0])
	}
	w.ticks(t, Interval)
	if n := w.notes.count("holds half or less"); n != 1 {
		t.Fatalf("notified again: %v", w.notes.got())
	}
	if st := w.s.Status(); st.Paths[0].Shrunk != first.String() {
		t.Fatalf("the flag went while the shrink stands: %+v", st.Paths[0])
	}
	// Back to about what it held: the flag goes.
	for i := range 1200 {
		mustWrite(t, filepath.Join(w.src, "many", fmt.Sprintf("f%04d", i)), "x", 0o644)
	}
	w.ticks(t, Interval)
	if st := w.s.Status(); st.Paths[0].Shrunk != "" {
		t.Fatalf("the flag stays after a recovery: %+v", st.Paths[0])
	}
	if ps, err := w.env.store(t).Pins(); err != nil || len(ps) != 0 {
		t.Fatalf("pins after a recovery %+v %v", ps, err)
	}
}

// Below the soft floor: nothing taken, shown, notified once an hour, and
// nothing pruned early; taken again once there is room.
func TestScheduleLowSpace(t *testing.T) {
	w := newSchedWorld(t, nil)
	// Kept by the rules: one a day old, and the newest.
	day := w.takeAt(t, schedT0.Add(-24*time.Hour))
	newest := w.takeAt(t, schedT0.Add(-20*time.Minute))
	w.s.o.TakeOptions.Statfs = func(string) (uint64, uint64, error) { return 20 << 30, 1000 << 30, nil }
	w.tick(t)
	if got := w.take.calls(); len(got) != 1 {
		t.Fatalf("tried %v", got)
	}
	if got := w.env.list(t, w.key()); len(got) != 2 {
		t.Fatalf("store holds %v", got)
	}
	st := w.s.Status()
	if !st.LowSpace || !strings.Contains(st.SpaceSkipped, "20480 MiB free") || st.Paths[0].Failing != "" || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	if n := w.notes.count("low on space (20480 MiB free, under the 51200 MiB the floor asks): no checkpoint is taken"); n != 1 {
		t.Fatalf("notifications %v", w.notes.got())
	}
	// Retried at each due time, notified at most hourly, nothing pruned.
	w.ticks(t, 50*time.Minute)
	if got := w.take.calls(); len(got) != 6 {
		t.Fatalf("tried %d times in 50 min: %v", len(got), got)
	}
	if n := w.notes.count("low on space"); n != 1 {
		t.Fatalf("notified again within the hour: %v", w.notes.got())
	}
	w.ticks(t, 15*time.Minute)
	if n := w.notes.count("low on space"); n != 2 {
		t.Fatalf("not notified after the hour: %v", w.notes.got())
	}
	if got := ids(t, w.env.store(t), w.key()); len(got) != 2 || got[0] != day.String() || got[1] != newest.String() {
		t.Fatalf("store holds %v, want %s and %s", got, newest, day)
	}
	// Room again: taken at the next due time, and the skip cleared.
	w.s.o.TakeOptions.Statfs = plenty
	w.ticks(t, Interval)
	if got := w.env.list(t, w.key()); len(got) != 3 {
		t.Fatalf("store holds %v", got)
	}
	if st := w.s.Status(); st.LowSpace || st.SpaceSkipped != "" {
		t.Fatalf("status %+v", st)
	}
	// Below the hard floor: not taken, and said.
	w.s.o.TakeOptions.Statfs = func(string) (uint64, uint64, error) { return 1 << 20, 1000 << 30, nil }
	w.ticks(t, Interval)
	if n := w.notes.count("nearly full"); n != 1 {
		t.Fatalf("notifications %v", w.notes.got())
	}
	if got := w.env.list(t, w.key()); len(got) != 3 {
		t.Fatalf("store holds %v", got)
	}
	// The hard floor is the path's failure; the soft one never is.
	if st := w.s.Status(); !st.LowSpace || st.Paths[0].Failing == "" {
		t.Fatalf("status %+v", st)
	}
}

// A store a sandbox can reach: nothing taken, shown and notified once,
// tried again at the next due time.
func TestScheduleExposed(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.env.exposed = fmt.Errorf("the store is inside root /work of environment beta: %w", ErrExposed)
	w.tick(t)
	w.ticks(t, 3*Interval)
	if got := w.env.list(t, w.key()); len(got) != 0 {
		t.Fatalf("took %v while exposed", got)
	}
	if got := w.take.calls(); len(got) != 4 {
		t.Fatalf("tried %d times in 30 minutes, want 4 (every 10 minutes)", len(got))
	}
	if st := w.s.Status(); !strings.Contains(st.Exposed, "inside root /work") {
		t.Fatalf("status %+v", st)
	}
	if n := w.notes.count("not being taken"); n != 1 {
		t.Fatalf("notifications %v", w.notes.got())
	}
}

// A path that does not resolve is not taken, notified once until it
// resolves again, and tried at the next due time.
func TestScheduleUnresolved(t *testing.T) {
	w := newSchedWorld(t, nil)
	src := w.env.srcs[0]
	w.env.srcs, w.env.bad = nil, []Unresolved{{Path: src.Path, Reason: "it is read-only"}}
	w.tick(t)
	w.ticks(t, Interval)
	if n := w.notes.count("is not being taken: it is read-only"); n != 1 {
		t.Fatalf("notifications %v", w.notes.got())
	}
	if w.env.resolved != 2 {
		t.Fatalf("resolved %d times in 10 minutes, want 2", w.env.resolved)
	}
	if st := w.s.Status(); st.Paths[0].Resolved || !st.Paths[0].Checked || st.Paths[0].Reason != "it is read-only" {
		t.Fatalf("status %+v", st.Paths[0])
	}
	w.env.srcs, w.env.bad = []Source{src}, nil
	w.ticks(t, Interval)
	if got := w.take.calls(); len(got) != 1 {
		t.Fatalf("took %v once it resolved", got)
	}
	if st := w.s.Status(); !st.Paths[0].Resolved {
		t.Fatalf("status %+v", st.Paths[0])
	}
	w.env.srcs, w.env.bad = nil, []Unresolved{{Path: src.Path, Reason: "it is gone"}}
	w.ticks(t, 2*time.Hour)
	if n := w.notes.count("is not being taken: it is gone"); n != 1 {
		t.Fatalf("notifications %v", w.notes.got())
	}
}

// The status: each path's newest, its method and the next due time, and
// how the environment is served.
func TestScheduleStatus(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.tick(t)
	st := w.s.Status()
	if st.Serving != ServeHeld || !st.Running || len(st.Paths) != 1 {
		t.Fatalf("status %+v", st)
	}
	p := st.Paths[0]
	if p.Path != w.env.paths[0] || p.Key != w.key() || p.Newest == nil || !p.NewestAt.Equal(schedT0) ||
		(p.Method != MethodCopy && p.Method != MethodReflink) || !p.NextAt.Equal(schedT0.Add(Interval)) || !p.Resolved {
		t.Fatalf("path status %+v", p)
	}
	w.env.set(ServeLast, false)
	w.ticks(t, time.Minute)
	if st := w.s.Status(); st.Serving != ServeLast || st.Running || !st.Paths[0].NextAt.IsZero() {
		t.Fatalf("status once stopped %+v", st)
	}
}

// An environment that cannot be read is an error of the tick, which goes
// on being served.
func TestScheduleViewError(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.env.viewErr = errors.New("config.toml: bad")
	if !w.s.Tick(t.Context()) {
		t.Fatal("an unreadable environment ended its schedule")
	}
	if st := w.s.Status(); !strings.Contains(st.LastError, "config.toml: bad") {
		t.Fatalf("status %+v", st)
	}
}

func TestShrinkFlag(t *testing.T) {
	at := func(m int) time.Time { return schedT0.Add(time.Duration(m) * time.Minute) }
	e := func(m int, entries int64, shrunkFrom string, prevEntries int64) Entry {
		id := ID{Key: "k-00000000", Stamp: at(m)}
		meta := &Meta{ID: id.String(), Entries: entries}
		if shrunkFrom != "" {
			meta.Shrunk = &Shrunk{ID: shrunkFrom, Entries: prevEntries}
		}
		return Entry{ID: id, Stamp: at(m), Meta: meta}
	}
	first := e(0, 5000, "", 0)
	shrunk := e(10, 100, first.ID.String(), 5000)
	still := e(20, 120, "", 0)
	back := e(30, 4900, "", 0)
	newestFirst := func(l ...Entry) []Entry { slices.Reverse(l); return l }
	if s, p := ShrinkFlag(newestFirst(first, shrunk, still)); s != shrunk.ID.String() || p != first.ID.String() {
		t.Fatalf("flag %q %q", s, p)
	}
	if s, _ := ShrinkFlag(newestFirst(first, shrunk, still, back)); s != "" {
		t.Fatalf("flag after a recovery %q", s)
	}
	if s, _ := ShrinkFlag(newestFirst(shrunk, still)); s != "" {
		t.Fatalf("flag after the earlier one was pruned %q", s)
	}
}

// A take cut short by the daemon stopping is no failure: nothing is
// notified, and the next start takes one at once, as a start.
func TestScheduleCancelled(t *testing.T) {
	w := newSchedWorld(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	w.take.fail = context.Canceled
	w.take.block, w.take.begun = make(chan struct{}), make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.s.Tick(ctx)
	}()
	<-w.take.begun
	cancel()
	close(w.take.block)
	<-done
	if len(w.notes.got()) != 0 || w.s.Status().LastError != "" {
		t.Fatalf("a cancelled take notified %v, status %+v", w.notes.got(), w.s.Status())
	}
	w.take.mu.Lock()
	w.take.fail, w.take.block, w.take.begun = nil, nil, nil
	w.take.mu.Unlock()
	w.clock.add(TickEvery)
	w.tick(t)
	if got := w.take.calls(); !slices.Equal(got, []string{ReasonStart, ReasonStart}) {
		t.Fatalf("took %v", got)
	}
}
