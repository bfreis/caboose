package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// ckClock is a clock the test moves.
type ckClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ckClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ckClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ckTakes records the daemon's takes and makes them for real.
type ckTakes struct {
	mu      sync.Mutex
	reasons []string
}

func (k *ckTakes) take(ctx context.Context, l *checkpoint.Lock, s *checkpoint.Store, src checkpoint.Source, reason string, o checkpoint.TakeOptions) (checkpoint.TakeResult, error) {
	k.mu.Lock()
	k.reasons = append(k.reasons, reason)
	k.mu.Unlock()
	return checkpoint.TakeLocked(ctx, l, s, src, reason, o)
}

func (k *ckTakes) got() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.reasons)
}

// ckBox is an agentBox the test can stop while the link and the
// schedule look at it.
type ckBox struct {
	*agentBox
	gone atomic.Bool
}

func (b *ckBox) State() string {
	if b.gone.Load() {
		return "exited"
	}
	return b.agentBox.State()
}

// ckWorld is a linksWorld whose environments have a root at /work and a
// checkpoint path in it, and daemon links with checkpoints on.
type ckWorld struct {
	*linksWorld
	clock *ckClock
	takes *ckTakes
	notes []string
	mu    sync.Mutex
	boxes map[string]*ckBox
}

func newCkWorld(t *testing.T) *ckWorld {
	w := &ckWorld{linksWorld: newLinksWorld(t), clock: &ckClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}, takes: &ckTakes{}, boxes: map[string]*ckBox{}}
	if err := os.MkdirAll(filepath.Join(w.root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.root, "proj", "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	return w
}

// ckEnvOf makes environment name with the checkpoint path /work/proj, its
// sandbox in status.
func (w *ckWorld) ckEnvOf(name, status string) *ckBox {
	w.t.Helper()
	b := &ckBox{agentBox: w.env(name, status)}
	w.boxes[name] = b
	body := fmt.Sprintf("[roots.work]\nhost = %q\npath = \"/work\"\n\n[checkpoints]\npaths = [\"/work/proj\"]\n\n"+
		"[link]\nforward_ports = [\"3000-3999\"]\nhost_exec = true\nssh_agent = \"none\"\n", w.root)
	if err := os.WriteFile(filepath.Join(config.EnvDirFor(w.home, name), config.FileName), []byte(body), 0o600); err != nil {
		w.t.Fatal(err)
	}
	return b
}

func (w *ckWorld) newApp(env string) (*App, error) {
	app, err := w.linksWorld.newApp(env)
	if err == nil && w.boxes[env] != nil {
		app.Backend = w.boxes[env]
	}
	return app, err
}

func (w *ckWorld) ckLinks(dir string) *DaemonLinks {
	l := NewDaemonLinks(DaemonLinksOptions{Dir: dir, CabooseHome: w.home, NewApp: w.newApp, Checkpoints: &CheckpointOptions{
		Every: time.Hour, Now: w.clock.now, Take: w.takes.take,
		TakeOptions: checkpoint.TakeOptions{Copy: checkpoint.Options{NoWhole: true},
			Statfs: func(string) (uint64, uint64, error) { return 900 << 30, 1000 << 30, nil }},
		Notify: func(title, text string) {
			w.mu.Lock()
			w.notes = append(w.notes, title+": "+text)
			w.mu.Unlock()
		}}})
	w.t.Cleanup(l.Close)
	return l
}

// stored is the reasons of env's checkpoints of /work/proj, oldest first.
func (w *ckWorld) stored(env string) []string {
	w.t.Helper()
	envDir := config.EnvDirFor(w.home, env)
	if _, err := os.Lstat(checkpoint.StoreDir(envDir)); err != nil {
		return nil
	}
	s, err := checkpoint.Open(w.home, envDir)
	if err != nil {
		w.t.Fatal(err)
	}
	defer s.Close()
	es, err := s.List(checkpoint.Key("/work/proj"))
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for i := len(es) - 1; i >= 0; i-- {
		out = append(out, es[i].Meta.Reason)
	}
	return out
}

func ckWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The daemon's link starting starts the environment's schedule, which
// takes the start checkpoint at once; the sandbox stopping (the link
// ending, link.lock let go) takes the stop checkpoint; status and the
// checkpoints method show them; Close ends it all.
func TestDaemonCheckpointSchedule(t *testing.T) {
	w := newCkWorld(t)
	box := w.ckEnvOf("alpha", "running")
	l := w.ckLinks(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	sess := box.session(t)
	ckWait(t, "the start checkpoint", func() bool { return slices.Equal(w.stored("alpha"), []string{checkpoint.ReasonStart}) })

	var st *daemonproto.CheckpointStatus
	ckWait(t, "a status with the checkpoint", func() bool {
		envs := l.Envs()
		if len(envs) != 1 || envs[0].Checkpoints == nil || len(envs[0].Checkpoints.Paths) != 1 {
			return false
		}
		st = envs[0].Checkpoints
		return st.Paths[0].Newest != ""
	})
	p := st.Paths[0]
	if p.Path != "/work/proj" || !p.Resolved || !p.Checked || st.Served != "held" || p.Method == "" || p.NextAt.IsZero() {
		t.Fatalf("status %+v / %+v", st, p)
	}
	envs := l.Envs()
	if err := (&daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{Version: "v1"}, Envs: envs}).Validate(); err != nil {
		t.Fatalf("the status does not validate: %v", err)
	}
	r, err := l.Checkpoints(t.Context(), "alpha")
	if err != nil || len(r.Paths) != 1 || len(r.Paths[0].Checkpoints) != 1 || r.Paths[0].Checkpoints[0].Reason != checkpoint.ReasonStart ||
		!r.Paths[0].Resolved || r.Validate() != nil {
		t.Fatalf("listing %+v (%v)", r, err)
	}

	// The sandbox stops: the link sees it at its next connection.
	w.clock.add(time.Minute)
	box.gone.Store(true)
	sess.Close()
	ckWait(t, "the link to end", func() bool { return !held(t, w.dataDir("alpha")) })
	ckWait(t, "the stop checkpoint", func() bool {
		return slices.Equal(w.stored("alpha"), []string{checkpoint.ReasonStart, checkpoint.ReasonStop})
	})
	if got := w.takes.got(); !slices.Equal(got, []string{checkpoint.ReasonStart, checkpoint.ReasonStop}) {
		t.Fatalf("took %v", got)
	}
	if st := l.schedStatus("alpha"); st == nil || st.Serving != checkpoint.ServeLast {
		t.Fatalf("schedule after the stop: %+v", st)
	}
	l.Close()
	if l.Busy() || l.schedStatus("alpha") != nil {
		t.Fatal("a schedule outlived Close")
	}
}

// How the daemon serves an environment's schedule: held only while its
// link here holds link.lock; last once that link ended, while link.json
// names this daemon and was not stopped on purpose; else none.
func TestDaemonCheckpointServing(t *testing.T) {
	w := newCkWorld(t)
	box := w.ckEnvOf("alpha", "running")
	l := w.ckLinks(w.daemonDir())
	e := l.env("alpha")
	data := w.dataDir("alpha")
	if got := l.serving(e, data); got != checkpoint.ServeNone {
		t.Fatalf("before any link: %v", got)
	}
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	sess := box.session(t)
	if got := l.serving(e, data); got != checkpoint.ServeHeld {
		t.Fatalf("with its link up: %v", got)
	}
	box.gone.Store(true)
	sess.Close()
	ckWait(t, "the link to end", func() bool { return !held(t, data) })
	ckWait(t, "the link to be gone", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.cur == nil
	})
	if got := l.serving(e, data); got != checkpoint.ServeLast {
		t.Fatalf("after its link ended: %v", got)
	}
	// Another daemon took it.
	st, _ := readLinkState(data)
	st.Daemon = filepath.Join(w.home, "daemon-dev")
	if err := writeLinkState(data, st); err != nil {
		t.Fatal(err)
	}
	if got := l.serving(e, data); got != checkpoint.ServeNone {
		t.Fatalf("named another daemon: %v", got)
	}
	// A detached helper took it.
	st.Daemon, st.PID = "", 12345
	if err := writeLinkState(data, st); err != nil {
		t.Fatal(err)
	}
	if got := l.serving(e, data); got != checkpoint.ServeNone {
		t.Fatalf("named a helper: %v", got)
	}
	// Stopped on purpose.
	st.Daemon, st.PID, st.Stopped = w.daemonDir(), 0, true
	if err := writeLinkState(data, st); err != nil {
		t.Fatal(err)
	}
	if got := l.serving(e, data); got != checkpoint.ServeNone {
		t.Fatalf("stopped with stop_link: %v", got)
	}
}

// An environment whose link.lock another process holds gets no
// checkpoint from this daemon, though its sandbox runs and a schedule is
// asked to look; the schedule ends.
func TestDaemonCheckpointNotHeld(t *testing.T) {
	w := newCkWorld(t)
	w.ckEnvOf("alpha", "running")
	f, unlock, err := lockLinkFile(w.dataDir("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_ = f
	if err := writeLinkState(w.dataDir("alpha"), linkState{Daemon: filepath.Join(w.home, "daemon-dev")}); err != nil {
		t.Fatal(err)
	}
	l := w.ckLinks(w.daemonDir())
	if _, err := l.Serve(t.Context(), daemonproto.MethodEnsureLink, "alpha"); err == nil {
		t.Fatal("a link started on another daemon's lock")
	}
	l.schedule(l.env("alpha"))
	ckWait(t, "the schedule to end", func() bool { return l.schedStatus("alpha") == nil })
	if got := w.takes.got(); len(got) != 0 {
		t.Fatalf("took %v", got)
	}
	if got := w.stored("alpha"); len(got) != 0 {
		t.Fatalf("stored %v", got)
	}
}

// Without checkpoint options the daemon runs no schedule.
func TestDaemonCheckpointsOff(t *testing.T) {
	w := newCkWorld(t)
	box := w.ckEnvOf("alpha", "running")
	l := w.links(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	box.session(t)
	time.Sleep(50 * time.Millisecond)
	if l.schedStatus("alpha") != nil || len(w.stored("alpha")) != 0 {
		t.Fatal("a schedule ran with checkpoints off")
	}
	if envs := l.Envs(); len(envs) != 1 || envs[0].Checkpoints != nil {
		t.Fatalf("status %+v", envs)
	}
}

// Recovery starts the schedule of every environment the daemon served,
// whether its sandbox runs or not: a stopped one is pruned.
func TestDaemonCheckpointRecovery(t *testing.T) {
	w := newCkWorld(t)
	w.ckEnvOf("alpha", "exited")
	if err := writeLinkState(w.dataDir("alpha"), linkState{Daemon: w.daemonDir()}); err != nil {
		t.Fatal(err)
	}
	// A leftover of a daemon that went mid-checkpoint.
	envDir := config.EnvDirFor(w.home, "alpha")
	s, err := checkpoint.Open(w.home, envDir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	left := filepath.Join(checkpoint.StoreDir(envDir), ".tmp-proj-00000000-20261018T100000Z-abc")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(left, old, old); err != nil {
		t.Fatal(err)
	}
	w.clock.t = time.Now()
	l := w.ckLinks(w.daemonDir())
	l.Recover(nil)
	ckWait(t, "the leftover to go", func() bool {
		_, err := os.Lstat(left)
		return os.IsNotExist(err)
	})
	st := l.schedStatus("alpha")
	if st == nil || st.Serving != checkpoint.ServeLast || st.Running {
		t.Fatalf("schedule %+v", st)
	}
	if got := w.takes.got(); len(got) != 0 {
		t.Fatalf("took %v of a stopped sandbox", got)
	}
}

// The status of a schedule, as the daemon reports it.
func TestCheckpointStatusConversion(t *testing.T) {
	if checkpointStatus(nil, time.Now()) != nil {
		t.Fatal("no schedule, yet a status")
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	id := checkpoint.ID{Key: "proj-00000000", Stamp: now.Add(-4 * time.Minute)}
	st := checkpointStatus(&checkpoint.Status{Serving: checkpoint.ServeHeld, LastError: "x\x1by", Exposed: "inside root",
		Paths: []checkpoint.PathStatus{{Path: "/work/proj", Key: id.Key, Checked: true, Resolved: true, Newest: &id,
			NewestAt: id.Stamp, Method: "clonefile", Shrunk: "proj-00000000@20261018T110000Z"}}}, now)
	p := st.Paths[0]
	if st.Served != "held" || p.Newest != id.String() || p.NewestAgeS != 240 || p.Method != "clonefile" || p.Shrunk == "" ||
		strings.ContainsRune(st.LastError, 0x1b) || st.Exposed != "inside root" {
		t.Fatalf("status %+v / %+v", st, p)
	}
	if err := (&daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "alpha", Checkpoints: st}}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// An environment with no checkpoint paths and no store gets none made.
func TestDaemonCheckpointNoPathsNoStore(t *testing.T) {
	w := newCkWorld(t)
	box := w.env("alpha", "running")
	l := w.ckLinks(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	box.session(t)
	ckWait(t, "a tick", func() bool { st := l.schedStatus("alpha"); return st != nil && st.Serving == checkpoint.ServeHeld })
	if _, err := os.Lstat(checkpoint.StoreDir(config.EnvDirFor(w.home, "alpha"))); !os.IsNotExist(err) {
		t.Fatalf("a store was made: %v", err)
	}
}

// ownsLock and holdsLock: true only for a link.lock still held; false once
// it is let go of, closed, or marked released, and never taking it back.
func TestOwnsLock(t *testing.T) {
	dir := t.TempDir()
	f, unlock, err := lockLinkFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ownsLock(f) {
		t.Fatal("a held lock is not owned")
	}
	in := &linkInstance{lock: f}
	if !in.holdsLock() {
		t.Fatal("a held lock is not held")
	}
	in.released = true
	if in.holdsLock() {
		t.Fatal("a released link holds its lock")
	}
	// Another process may take it now: holdsLock above did not take it back.
	unlock()
	g, unlock2, err := lockLinkFile(dir)
	if err != nil {
		t.Fatalf("the lock was not free after its link let go: %v", err)
	}
	defer unlock2()
	if ownsLock(f) || ownsLock(nil) {
		t.Fatal("a closed or missing file owns the lock")
	}
	_ = g
}

// The schedule's view carries when the running sandbox started, which is
// how it tells a restart no tick saw stopped; none while it is stopped.
func TestDaemonCheckpointViewStarted(t *testing.T) {
	w := newCkWorld(t)
	box := w.ckEnvOf("alpha", "running")
	at := time.Date(2026, 10, 18, 11, 59, 0, 0, time.UTC)
	box.Fake.StartedAt = at
	l := w.ckLinks(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	box.session(t)
	x := schedEnv{l: l, e: l.env("alpha")}
	v, err := x.View()
	if err != nil || v.Serving != checkpoint.ServeHeld || !v.Running || !v.Started.Equal(at) {
		t.Fatalf("view %+v (%v), want started %v", v, err, at)
	}
	box.gone.Store(true)
	if v, err := x.View(); err != nil || v.Running || !v.Started.IsZero() {
		t.Fatalf("stopped: view %+v (%v)", v, err)
	}
}

// A checkpoint the sandbox asks for is taken through the environment's
// schedule, reason agent, and named by its ID; the daemon's link offers
// it, with the environment's limits, which outlive the link.
func TestDaemonAgentCheckpoint(t *testing.T) {
	w := newCkWorld(t)
	w.ckEnvOf("alpha", "running")
	l := w.ckLinks(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	ckWait(t, "the start checkpoint", func() bool { return slices.Equal(w.stored("alpha"), []string{checkpoint.ReasonStart}) })
	l.mu.Lock()
	e := l.envs["alpha"]
	l.mu.Unlock()
	e.mu.Lock()
	in := e.cur
	e.mu.Unlock()
	if in == nil || in.runner.cfg.Checkpoints == nil || in.runner.cfg.CheckpointLimits == nil || in.runner.cfg.CheckpointRequests != config.CheckpointRequestsAllow {
		t.Fatalf("the link offers no checkpoints: %+v", in)
	}
	cp, lim := l.agentCheckpoints(e)
	if lim != in.runner.cfg.CheckpointLimits {
		t.Error("the limits are not the environment's")
	}
	if paths, err := cp.CheckpointPaths(); err != nil || !slices.Equal(paths, []string{"/work/proj"}) {
		t.Fatalf("paths %q, %v", paths, err)
	}
	w.clock.add(time.Minute)
	got := cp.TakeCheckpoints(t.Context(), []string{"/work/proj"})
	if len(got) != 1 || got[0].Path != "/work/proj" || got[0].ID == "" || got[0].Reason != "" {
		t.Fatalf("took %+v", got)
	}
	if s := w.stored("alpha"); !slices.Equal(s, []string{checkpoint.ReasonStart, checkpoint.ReasonAgent}) {
		t.Fatalf("stored %v", s)
	}
	if !strings.HasPrefix(got[0].ID, checkpoint.Key("/work/proj")) {
		t.Errorf("ID %q", got[0].ID)
	}

	// With checkpoints off in the daemon, a link offers none.
	l2 := NewDaemonLinks(DaemonLinksOptions{Dir: t.TempDir(), CabooseHome: w.home, NewApp: w.newApp})
	t.Cleanup(l2.Close)
	if cp, lim := l2.agentCheckpoints(&envLink{name: "alpha"}); cp != nil || lim != nil {
		t.Fatal("offered without a schedule")
	}
}
