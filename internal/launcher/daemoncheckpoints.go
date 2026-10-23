package launcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/hostlink"
	"golang.org/x/sys/unix"
)

// CheckpointOptions turn on the daemon's checkpoint schedule
// (DaemonLinksOptions.Checkpoints): one checkpoint.Scheduler per
// environment whose link this daemon runs, or ran last.
type CheckpointOptions struct {
	// Notify shows the user a notification; nil shows none.
	Notify func(title, text string)
	// Every is how often each schedule looks; checkpoint.TickEvery when 0.
	Every time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Take takes one checkpoint; checkpoint.TakeLocked when nil.
	Take checkpoint.TakeFunc
	// TakeOptions carries the takes' test switches.
	TakeOptions checkpoint.TakeOptions
	// Env, when set, stands in for an environment's view (the tests').
	Env func(env string) checkpoint.Env
}

// SystemNotify is the daemon's notifications: the host's own.
func SystemNotify(title, text string) { _ = hostlink.Notify(hostlink.System{}, title, text) }

// schedRun is one environment's running schedule.
type schedRun struct {
	s      *checkpoint.Scheduler
	kick   chan struct{} // a tick now, not at the next TickEvery
	kicked bool          // under DaemonLinks.mu: keep running though the last tick found it unserved
}

// schedule has env's checkpoint schedule run, if checkpoints are on: a
// new one, or the one that runs looking at once (it may have just found
// the environment unserved, and would end: it keeps going). Called when
// a link starts, when it ends (so the stop checkpoint is not left for
// the next tick), and by recovery for the environments the daemon served.
func (l *DaemonLinks) schedule(e *envLink) {
	if l.cp == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if r := l.scheds[e.name]; r != nil {
		r.kicked = true
		select {
		case r.kick <- struct{}{}:
		default:
		}
		return
	}
	var env checkpoint.Env = schedEnv{l: l, e: e}
	if l.cp.Env != nil {
		env = l.cp.Env(e.name)
	}
	name := e.name
	r := &schedRun{kick: make(chan struct{}, 1), s: checkpoint.NewScheduler(checkpoint.SchedulerOptions{
		Name: name, Env: env, Gate: l.cpGate, Now: l.cp.Now, Take: l.cp.Take, Notify: l.cp.Notify,
		Logf: l.logf, TakeOptions: l.cp.TakeOptions,
		Command: func(args string) string { return envCommand(name, args) },
	})}
	l.scheds[name] = r
	every := l.cp.Every
	if every <= 0 {
		every = checkpoint.TickEvery
	}
	l.schedWG.Add(1)
	go l.runSchedule(name, r, every)
}

// runSchedule ticks r until the daemon's links close, or a tick finds the
// environment served by another (or by none) and nothing kicked it since.
func (l *DaemonLinks) runSchedule(name string, r *schedRun, every time.Duration) {
	defer l.schedWG.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		served := r.s.Tick(l.ctx)
		l.mu.Lock()
		if l.ctx.Err() != nil || !served && !r.kicked {
			if l.scheds[name] == r {
				delete(l.scheds, name)
			}
			l.mu.Unlock()
			return
		}
		r.kicked = false
		l.mu.Unlock()
		select {
		case <-l.ctx.Done():
		case <-t.C:
		case <-r.kick:
		}
	}
}

// checkpointing reports whether a schedule takes a checkpoint now.
func (l *DaemonLinks) checkpointing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.scheds {
		if r.s.Busy() {
			return true
		}
	}
	return false
}

// schedStatus is env's schedule's state, if one runs.
func (l *DaemonLinks) schedStatus(env string) *checkpoint.Status {
	l.mu.Lock()
	r := l.scheds[env]
	l.mu.Unlock()
	if r == nil {
		return nil
	}
	st := r.s.Status()
	return &st
}

// schedEnv is an environment as its schedule sees it at each tick: its
// config.toml and sandbox afresh, through its App.
type schedEnv struct {
	l *DaemonLinks
	e *envLink
}

func (x schedEnv) View() (checkpoint.View, error) {
	app, err := x.l.newApp(x.e.name)
	if err != nil {
		if daemonproto.Kind(err) == daemonproto.KindUnknownEnv {
			return checkpoint.View{Serving: checkpoint.ServeNone}, nil
		}
		return checkpoint.View{}, err
	}
	v := checkpoint.View{Paths: app.Cfg.Checkpoints, Serving: x.l.serving(x.e, app.Cfg.DataDir)}
	if v.Serving == checkpoint.ServeHeld {
		box := app.box()
		v.Running = box.State() == "running"
		// When it started, to tell a restart between two ticks; asked
		// only when there is something to take.
		if st, ok := box.(backend.Starter); ok && v.Running && len(v.Paths) > 0 {
			if at, ok := st.Started(); ok {
				v.Started = at
			}
		}
	}
	v.Open = func() (*checkpoint.Store, error) {
		if len(app.Cfg.Checkpoints) == 0 {
			if _, err := os.Lstat(checkpoint.StoreDir(app.Cfg.EnvDir)); errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
		}
		return app.openCheckpointStore(nil)
	}
	v.Sources = func() ([]checkpoint.Source, []checkpoint.Unresolved) {
		srcs, bad := app.checkpointSources()
		out := make([]checkpoint.Unresolved, 0, len(bad))
		for _, b := range bad {
			out = append(out, checkpoint.Unresolved{Path: b.Path, Reason: b.Reason})
		}
		return srcs, out
	}
	return v, nil
}

// serving is how this daemon serves e, whose data dir is dataDir:
// ServeHeld while its link here holds link.lock; ServeLast once that link
// has ended, as long as link.json still names this daemon and its link was
// not stopped on purpose (any other process that takes link.lock writes
// link.json, and a link stopped with stop_link is marked so); else
// ServeNone.
func (l *DaemonLinks) serving(e *envLink, dataDir string) checkpoint.Serving {
	e.mu.Lock()
	in := e.cur
	e.mu.Unlock()
	if in != nil && in.holdsLock() {
		return checkpoint.ServeHeld
	}
	st, ok := readLinkState(dataDir)
	if ok && !st.Stopped && st.Daemon != "" && filepath.Clean(st.Daemon) == l.dir {
		return checkpoint.ServeLast
	}
	return checkpoint.ServeNone
}

// holdsLock reports whether in's link still holds its link.lock. Under
// in.lockMu, so the look never happens as the link lets go of it: a flock
// made between its LOCK_UN and its close would take the lock back, and
// keep another process from taking it.
func (in *linkInstance) holdsLock() bool {
	in.lockMu.Lock()
	defer in.lockMu.Unlock()
	return !in.released && ownsLock(in.lock)
}

// ownsLock reports whether f, a link.lock this process opened, still
// holds its lock. The fd is used through SyscallConn, which keeps it open
// meanwhile: a link ending closes f, and a closed fd's number may already
// name another file.
func ownsLock(f *os.File) bool {
	if f == nil {
		return false
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) {
		for {
			ferr = unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB)
			if !errors.Is(ferr, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return false
	}
	return ferr == nil
}

// checkpointStatus is a schedule's state as the daemon's status reports
// it, at now.
func checkpointStatus(st *checkpoint.Status, now time.Time) *daemonproto.CheckpointStatus {
	if st == nil {
		return nil
	}
	text := func(s string) string { return daemonproto.Text(s, 1000) }
	out := &daemonproto.CheckpointStatus{Paths: []daemonproto.CheckpointPathStatus{},
		InProgressSince: st.InProgressSince, InProgressPath: text(st.InProgressPath),
		LastError: text(st.LastError), LastErrorAt: st.LastErrorAt, LowSpace: st.LowSpace, SpaceSkipped: text(st.SpaceSkipped),
		Exposed: text(st.Exposed), Slow: text(st.Slow)}
	if st.Serving != checkpoint.ServeNone {
		out.Served = st.Serving.String()
	}
	for i, p := range st.Paths {
		if i == daemonproto.MaxCheckpointPaths {
			break
		}
		ps := daemonproto.CheckpointPathStatus{Path: text(p.Path), Key: p.Key, Checked: p.Checked, Resolved: p.Resolved,
			Reason: text(p.Reason), Method: daemonproto.Text(p.Method, 100), Shrunk: daemonproto.Text(p.Shrunk, 100),
			NextAt: p.NextAt, Failing: text(p.Failing)}
		if p.Newest != nil {
			ps.Newest, ps.NewestAt = p.Newest.String(), p.NewestAt
			ps.NewestAgeS = int64(now.Sub(p.NewestAt) / time.Second)
		}
		out.Paths = append(out.Paths, ps)
	}
	return out
}

// Checkpoints is the checkpoints method (daemon.Checkpoints): env's
// checkpoints, read from its store.
func (l *DaemonLinks) Checkpoints(_ context.Context, env string) (*daemonproto.CheckpointsResult, error) {
	app, err := l.newApp(env)
	if err != nil {
		return nil, err
	}
	return app.checkpointListing(l.schedStatus(env))
}

// checkpointListing is this environment's checkpoints: each configured
// path's, newest first, with whether it resolves (as st, a schedule's
// state, last found, or resolved now when st has not), then the keys of
// paths no longer configured. It reads the store, takes no lock (a
// checkpoint appears complete, by a rename), and makes no store that is
// not there.
func (a *App) checkpointListing(st *checkpoint.Status) (*daemonproto.CheckpointsResult, error) {
	res := &daemonproto.CheckpointsResult{Env: a.Cfg.Env, Paths: []daemonproto.CheckpointListed{}}
	text := func(s string) string { return daemonproto.Text(s, 1000) }
	known := map[string]checkpoint.PathStatus{}
	if st != nil {
		for _, p := range st.Paths {
			known[p.Path] = p
		}
	}
	var resolvedNow map[string]string
	resolve := func(p string) (bool, string) {
		if ps, ok := known[p]; ok && ps.Checked {
			return ps.Resolved, ps.Reason
		}
		if resolvedNow == nil {
			resolvedNow = map[string]string{}
			srcs, bad := a.checkpointSources()
			for _, s := range srcs {
				resolvedNow[s.Path] = ""
			}
			for _, b := range bad {
				resolvedNow[b.Path] = b.Reason
			}
		}
		why, ok := resolvedNow[p]
		if !ok {
			return false, "it did not resolve"
		}
		return why == "", why
	}
	var store *checkpoint.Store
	if _, err := os.Lstat(checkpoint.StoreDir(a.Cfg.EnvDir)); err == nil {
		s, err := checkpoint.Open(a.Cfg.CabooseHome, a.Cfg.EnvDir)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		store = s
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	pins := map[string]checkpoint.Pin{}
	pinsErr := ""
	if store != nil {
		ps, err := store.Pins()
		if err != nil {
			pinsErr = text(err.Error())
		}
		for _, p := range ps {
			pins[p.ID] = p
		}
	}
	listed := func(path, key string) daemonproto.CheckpointListed {
		out := daemonproto.CheckpointListed{Path: text(path), Key: key, Checkpoints: []daemonproto.CheckpointView{}, PinsError: pinsErr}
		if store == nil {
			return out
		}
		es, err := store.List(key)
		if err != nil {
			out.Reason = text(err.Error())
			return out
		}
		if _, prev := checkpoint.ShrinkFlag(es); prev != "" {
			out.Shrunk = prev
		}
		for i, e := range es {
			if i == daemonproto.MaxCheckpointList {
				res.Truncated = true
				break
			}
			v := daemonproto.CheckpointView{ID: e.ID.String()}
			if p, ok := pins[v.ID]; ok {
				v.Pinned, v.PinnedBy, v.PinnedAt = true, daemonproto.Text(p.By, 100), p.Since
			}
			if e.Err != nil || e.Meta == nil {
				v.Error = text("unreadable: " + errString(e.Err))
			} else {
				m := e.Meta
				v.Started, v.Finished = m.Started, m.Finished
				v.Reason, v.Method = daemonproto.Text(m.Reason, 100), daemonproto.Text(m.Method, 100)
				v.Entries, v.Bytes, v.Linked, v.Skipped = max(m.Entries, 0), max(m.Bytes, 0), max(m.Linked, 0), max(m.Skipped, 0)
				if m.Shrunk != nil {
					v.Shrunk = daemonproto.Text(m.Shrunk.ID, 100)
				}
			}
			out.Checkpoints = append(out.Checkpoints, v)
		}
		return out
	}
	var keys []string
	for _, p := range a.Cfg.Checkpoints {
		k := checkpoint.Key(p)
		keys = append(keys, k)
		l := listed(p, k)
		l.Checked = true
		if l.Resolved, l.Reason = resolve(p); l.Resolved {
			l.Reason = ""
		} else {
			l.Reason = text(l.Reason)
		}
		res.Paths = append(res.Paths, l)
	}
	if store != nil {
		ks, err := store.Keys()
		if err != nil {
			return nil, err
		}
		for _, k := range ks {
			if slices.Contains(keys, k.Key) || len(res.Orphans) == daemonproto.MaxCheckpointPaths {
				continue
			}
			res.Orphans = append(res.Orphans, listed(k.Path, k.Key))
		}
	}
	return res, nil
}

func errString(err error) string {
	if err == nil {
		return "no meta.json"
	}
	return err.Error()
}

// agentCheckpoints is what e's link takes the checkpoints its sandbox asks
// for with (OpCheckpoint), and the environment's limits on them; nil, nil
// when this daemon runs no checkpoint schedules.
func (l *DaemonLinks) agentCheckpoints(e *envLink) (hostlink.Checkpointer, *hostlink.CheckpointLimits) {
	if l.cp == nil {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cpLimits == nil {
		e.cpLimits = hostlink.NewCheckpointLimits(l.cp.Now)
	}
	return envCheckpoints{l: l, e: e}, e.cpLimits
}

// envCheckpoints takes an environment's checkpoints for its sandbox,
// through its schedule (checkpoint.Scheduler.TakeNow): the same gate and
// store lock, and the paths resolved as the schedule resolves them, from
// config.toml and the roots the sandbox mounts.
type envCheckpoints struct {
	l *DaemonLinks
	e *envLink
}

func (x envCheckpoints) CheckpointPaths() ([]string, error) {
	app, err := x.l.newApp(x.e.name)
	if err != nil {
		return nil, err
	}
	return slices.Clone(app.Cfg.Checkpoints), nil
}

func (x envCheckpoints) TakeCheckpoints(ctx context.Context, paths []string) []agentproto.CheckpointTaken {
	r := x.l.schedRunOf(x.e.name)
	if r == nil {
		// A schedule runs while the link does; one that just ended
		// starts again.
		x.l.schedule(x.e)
		r = x.l.schedRunOf(x.e.name)
	}
	out := make([]agentproto.CheckpointTaken, 0, len(paths))
	if r == nil {
		for _, p := range paths {
			out = append(out, agentproto.CheckpointTaken{Path: p, Reason: "the daemon runs no checkpoint schedule for this environment now"})
		}
		return out
	}
	for _, n := range r.s.TakeNow(ctx, paths, checkpoint.ReasonAgent) {
		t := agentproto.CheckpointTaken{Path: n.Path, Reason: n.Reason}
		if n.Taken {
			t.ID, t.Reason = n.ID.String(), ""
		}
		out = append(out, t)
	}
	return out
}

// schedRunOf is env's running schedule, nil when none runs.
func (l *DaemonLinks) schedRunOf(env string) *schedRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.scheds[env]
}
