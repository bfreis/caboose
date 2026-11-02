package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
)

// volumeBrokerWorld is a jobsWorld whose environment's broker runs the
// resizes it allows as the daemon's jobs, as a link the daemon runs opens
// it, on a config.toml whose vm profile has volume cache, 64G.
func volumeBrokerWorld(t *testing.T) (*jobsWorld, *broker.Broker) {
	t.Helper()
	w := newJobsWorld(t, isolationContainer)
	envDir := w.b.Cfg.EnvDir
	body := "isolation = \"vm.default\"\n\n[link]\nforward_ports = []\nopen_urls = \"off\"\nssh_agent = \"none\"\n\n[vm.default]\n\n[vm.default.volume.cache]\npath = \"/cache\"\nsize = \"64G\"\n"
	if err := os.WriteFile(filepath.Join(envDir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, unlock, err := lockLinkFile(w.b.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	getenv := w.b.Cfg.Getenv
	b, err := broker.Open(broker.Options{Home: w.b.Cfg.CabooseHome, Env: w.b.Cfg.Env, Owner: broker.LockOwner(lock),
		Source: func() (broker.Settings, error) {
			c, err := config.Load(getenv, config.OSFS{}, w.b.Cfg.Env)
			if err != nil {
				return broker.Settings{}, err
			}
			return brokerSettings(c, envDir), nil
		},
		ConfigWriter: &configWriter{envDir: envDir}, Resizer: volumeJobs{l: w.l, env: w.b.Cfg.Env}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	w.l.brokerHook = func(env string) *brokerHandle {
		if env == w.b.Cfg.Env {
			return &brokerHandle{b: b}
		}
		return nil
	}
	return w, b
}

// filed is a resize of vol to size as a request filed against volumeEnv's
// volume cache passes it on: its disk and config.toml at 64G, of vm
// profile vm.default.
func filed(vol string, size int64) broker.Resize {
	return broker.Resize{ID: 1, Volume: vol, Size: size, From: 64 << 30, Configured: 64 << 30, Profile: "vm.default"}
}

func fileResize(t *testing.T, b *broker.Broker, size int64) uint64 {
	t.Helper()
	o, err := b.File(broker.Ask{Request: policy.Request{Kind: policy.KindVolumeResize, Volume: "cache", VolumeSize: size,
		VolumeFrom: 64 << 30, VolumeLive: true, VolumeFree: 1 << 40}})
	if err != nil || o.Verdict != broker.Pending {
		t.Fatalf("filed: %+v %v", o, err)
	}
	return o.Request
}

// An allowed resize runs as a job of the environment's, with the volume
// and size the request named; what came of it reaches the request (for
// the sandbox), jobs.log and a notification.
func TestVolumeRequestRunsAsJob(t *testing.T) {
	w, b := volumeBrokerWorld(t)
	type call struct {
		name string
		size int64
	}
	var calls []call
	w.l.volumeAction = func(a *App, r broker.Resize) (string, error) {
		calls = append(calls, call{r.Volume, r.Size})
		if r.From != 64<<30 || r.Configured != 64<<30 || r.Profile != "vm.default" {
			t.Errorf("filed against %+v", r)
		}
		a.Note("growing %s", r.Volume)
		return "volume cache is 128G now (it was 64G), and config.toml says so", nil
	}
	id := fileResize(t, b, 128<<30)
	res, err := b.Answer(id, broker.Reply{Verdict: broker.ReplyAllow, Scope: broker.ScopeOnce, By: broker.ByCLI})
	if err != nil || res.ResizeError != "" {
		t.Fatalf("%+v %v", res, err)
	}
	j := w.wait(t, "default")
	if j.Kind != daemonproto.JobVolumeResize || j.State != daemonproto.JobDone {
		t.Fatalf("job %+v", j)
	}
	if !slices.Equal(calls, []call{{"cache", 128 << 30}}) {
		t.Fatalf("resized %+v", calls)
	}
	ri, err := b.Request(id)
	if err != nil || !strings.Contains(ri.Reason, "then: volume cache is 128G now") {
		t.Fatalf("request %+v %v", ri, err)
	}
	log := w.jobsLog(t)
	for _, s := range []string{"volume_resize default: request #1: volume cache to 128G", "growing cache", "volume_resize default: done"} {
		if !strings.Contains(log, s) {
			t.Errorf("jobs.log lacks %q:\n%s", s, log)
		}
	}
	if n := w.notified(); len(n) != 1 || !strings.Contains(n[0], "request #1: volume cache is 128G now") {
		t.Errorf("notified %q", n)
	}
}

// A resize that fails says so on the request and in the notification; one
// that cannot start (another job runs) is the answer's ResizeError and the
// request's outcome, and runs nothing.
func TestVolumeRequestFailsOrDoesNotStart(t *testing.T) {
	w, b := volumeBrokerWorld(t)
	w.l.volumeAction = func(a *App, _ broker.Resize) (string, error) {
		return "", errors.New("volume cache is in use: pid 42 (make); a backup at " + filepath.Join(a.Cfg.DataDir, "vm", "cache.backup") + ": delete it")
	}
	id := fileResize(t, b, 32<<30)
	if _, err := b.Answer(id, broker.Reply{Verdict: broker.ReplyAllow, Scope: broker.ScopeOnce, By: broker.ByCLI}); err != nil {
		t.Fatal(err)
	}
	if j := w.wait(t, "default"); j.State != daemonproto.JobFailed || !strings.Contains(j.Error, "in use") {
		t.Fatalf("job %+v", j)
	}
	ri, _ := b.Request(id)
	// The sandbox reads the request: the host's path is left out there,
	// and kept in jobs.log and the notification, for the person.
	if !strings.Contains(ri.Reason, "then: the resize failed: volume cache is in use: pid 42 (make); a backup at (a path on the host): delete it") || strings.Contains(ri.Reason, w.b.Cfg.DataDir) {
		t.Fatalf("request %+v", ri)
	}
	hostPath := filepath.Join(w.b.Cfg.DataDir, "vm", "cache.backup")
	if n := w.notified(); len(n) != 1 || !strings.Contains(n[0], "resizing volume cache failed: volume cache is in use") || !strings.Contains(n[0], hostPath) {
		t.Errorf("notified %q", n)
	}
	if !strings.Contains(w.jobsLog(t), hostPath) {
		t.Errorf("jobs.log lacks the path:\n%s", w.jobsLog(t))
	}

	// Another job running: the resize does not start.
	block := make(chan struct{})
	w.l.jobAction = func(*App, string) error { <-block; return nil }
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); err != nil {
		t.Fatal(err)
	}
	ran := false
	w.l.volumeAction = func(*App, broker.Resize) (string, error) { ran = true; return "", nil }
	id = fileResize(t, b, 96<<30)
	res, err := b.Answer(id, broker.Reply{Verdict: broker.ReplyAllow, Scope: broker.ScopeOnce, By: broker.ByCLI})
	close(block)
	w.wait(t, "default")
	if err != nil || !strings.Contains(res.ResizeError, "already runs") {
		t.Fatalf("%+v %v", res, err)
	}
	if ri, _ := b.Request(id); !strings.Contains(ri.Reason, "the resize did not start") || ran {
		t.Fatalf("request %+v, ran %v", ri, ran)
	}
}

// A resize that does not start for a reason naming a host path (jobs.log
// cannot be opened; the environment cannot be loaded): the request's
// outcome, which the sandbox reads, leaves the path out; the answer, the
// person's, keeps it.
func TestVolumeRequestNotStartedHostPaths(t *testing.T) {
	w, b := volumeBrokerWorld(t)
	ran := false
	w.l.volumeAction = func(*App, broker.Resize) (string, error) { ran = true; return "", nil }
	jobsLog := filepath.Join(w.b.Cfg.EnvDir, jobsLogFile)
	if err := os.RemoveAll(jobsLog); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(jobsLog, 0o700); err != nil {
		t.Fatal(err)
	}
	id := fileResize(t, b, 96<<30)
	res, err := b.Answer(id, broker.Reply{Verdict: broker.ReplyAllow, Scope: broker.ScopeOnce, By: broker.ByCLI})
	if err != nil || !strings.Contains(res.ResizeError, jobsLog) {
		t.Fatalf("%+v %v", res, err)
	}
	ri, _ := b.Request(id)
	if !strings.Contains(ri.Reason, "the resize did not start: ") || !strings.Contains(ri.Reason, "(a path on the host)") || strings.Contains(ri.Reason, w.b.Cfg.EnvDir) || ran {
		t.Fatalf("jobs.log: request %+v, ran %v", ri, ran)
	}
	if err := os.Remove(jobsLog); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".caboose", "envs", "default", "config.toml")
	w.l.newApp = func(string) (*App, error) { return nil, errors.New("cannot read " + cfgPath + ": permission denied") }
	id = fileResize(t, b, 112<<30)
	res, err = b.Answer(id, broker.Reply{Verdict: broker.ReplyAllow, Scope: broker.ScopeOnce, By: broker.ByCLI})
	if err != nil || !strings.Contains(res.ResizeError, cfgPath) {
		t.Fatalf("%+v %v", res, err)
	}
	ri, _ = b.Request(id)
	if !strings.Contains(ri.Reason, "the resize did not start: cannot read (a path on the host): permission denied") || strings.Contains(ri.Reason, home) || ran {
		t.Fatalf("newApp: request %+v, ran %v", ri, ran)
	}
}

// The job's resize looks again at config.toml and the VM: what changed
// since the request refuses it, changing nothing; else it is caboose
// volume resize's live resize, its size written once the disk has it.
func TestRequestedResize(t *testing.T) {
	e, b := volumeEnv(t, liveVM, 64<<30)
	e.a.Cfg.VMVolumeLimit = config.DefaultVolumeLimit
	b.res = backend.VolumeResize{From: 64 << 30, Applied: true}
	out, err := e.a.requestedResize(t.Context(), filed("cache", 96<<30))
	if err != nil || out != "volume cache is 96G now (it was 64G), and config.toml says so" {
		t.Fatalf("%q %v", out, err)
	}
	if got := e.configSize(); got != "96G" || len(b.resized) != 1 {
		t.Fatalf("config %q, resized %v", got, b.resized)
	}

	for name, tc := range map[string]struct {
		state backend.VolumeState
		limit int64
		vol   string
		want  string
	}{
		"stopped":   {backend.VolumeState{Has: true, Live: true}, config.DefaultVolumeLimit, "cache", "no longer runs with volume cache"},
		"not live":  {backend.VolumeState{Running: true, Has: true}, config.DefaultVolumeLimit, "cache", "no longer runs with volume cache"},
		"not had":   {backend.VolumeState{Running: true, Live: true}, config.DefaultVolumeLimit, "cache", "no longer runs with volume cache"},
		"limit":     {liveVM, 80 << 30, "cache", "over volume_limit (80G)"},
		"gone":      {liveVM, config.DefaultVolumeLimit, "data", "has no volume \"data\""},
		"too small": {liveVM, config.DefaultVolumeLimit, "cache", ""},
	} {
		e, b := volumeEnv(t, tc.state, 64<<30)
		e.a.Cfg.VMVolumeLimit = tc.limit
		size := int64(96 << 30)
		if name == "too small" {
			size, tc.want = 2<<30, "outside 4G to 1024G"
		}
		_, err := e.a.requestedResize(t.Context(), filed(tc.vol, size))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if len(b.resized) != 0 || e.configSize() != "64G" {
			t.Errorf("%s: resized %v, config %q", name, b.resized, e.configSize())
		}
	}

	// Under another isolation now: refused.
	e, b = volumeEnv(t, liveVM, 64<<30)
	e.a.Cfg.Isolation = config.KindContainer
	if _, err := e.a.requestedResize(t.Context(), filed("cache", 96<<30)); err == nil || !strings.Contains(err.Error(), "isolation is container now") || len(b.resized) != 0 {
		t.Fatalf("container: %v, resized %v", err, b.resized)
	}

	// Applied with an error: recorded, and said.
	e, b = volumeEnv(t, liveVM, 64<<30)
	e.a.Cfg.VMVolumeLimit = config.DefaultVolumeLimit
	b.res, b.err = backend.VolumeResize{From: 64 << 30, Applied: true}, errors.New("its filesystem did not grow")
	if _, err := e.a.requestedResize(t.Context(), filed("cache", 96<<30)); err == nil || !strings.Contains(err.Error(), "the disk is 96G now, and config.toml says so") || e.configSize() != "96G" {
		t.Fatalf("%v, config %q", err, e.configSize())
	}
}

// A volume that changed since the request was filed -- config.toml's
// size, its disk's, the vm profile -- refuses the resize, changing
// nothing: the person approved it from the size the request showed.
func TestRequestedResizeVolumeChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		disk   int64
		change func(r *broker.Resize)
	}{
		// The user resized it to 200G meanwhile: 128G would shrink it.
		"config.toml": {64 << 30, func(r *broker.Resize) { r.Configured = 200 << 30 }},
		"disk":        {200 << 30, func(*broker.Resize) {}},
		"profile":     {64 << 30, func(r *broker.Resize) { r.Profile = "vm.other" }},
	} {
		e, b := volumeEnv(t, liveVM, tc.disk)
		e.a.Cfg.VMVolumeLimit = config.DefaultVolumeLimit
		r := filed("cache", 128<<30)
		tc.change(&r)
		_, err := e.a.requestedResize(t.Context(), r)
		if err == nil || err.Error() != broker.VolumeChanged {
			t.Errorf("%s: %v", name, err)
		}
		if len(b.resized) != 0 || e.configSize() != "64G" {
			t.Errorf("%s: resized %v, config %q", name, b.resized, e.configSize())
		}
	}
}

// Once config.toml has the new size, sessions are told what a proposal
// is made against again (state.toml); a resize refused tells nothing.
func TestRequestedResizeExports(t *testing.T) {
	e, b := volumeEnv(t, liveVM, 64<<30)
	e.a.Cfg.VMVolumeLimit = config.DefaultVolumeLimit
	state := filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml")
	if _, err := e.a.requestedResize(t.Context(), filed("cache", 300<<30)); err == nil {
		t.Fatal("past the limit, resized")
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused resize exported: %v", err)
	}
	b.res = backend.VolumeResize{From: 64 << 30, Applied: true}
	if _, err := e.a.requestedResize(t.Context(), filed("cache", 96<<30)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("not exported: %v", err)
	}
}

// What the sandbox reads of a failure is one line, without the host's
// paths.
func TestSandboxText(t *testing.T) {
	e, _ := volumeEnv(t, liveVM, 0)
	c := e.a.Cfg
	in := "cannot edit " + filepath.Join(c.EnvDir, "config.toml") + " safely (x);\n   nothing was changed: " + c.DataDir + ", '" + filepath.Join(c.Home, "a b") + "'."
	got := e.a.sandboxText(in)
	want := "cannot edit (a path on the host) safely (x); nothing was changed: (a path on the host), '(a path on the host) b'."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// What the link tells the policy of a volume is the host's own: its
// disk's size, whether the running VM has it live, the space free.
func TestLinkVolumeFacts(t *testing.T) {
	for _, tc := range []struct {
		state backend.VolumeState
		live  bool
	}{
		{liveVM, true},
		{backend.VolumeState{Has: true, Live: true}, false},
		{backend.VolumeState{Running: true, Live: true}, false},
		{backend.VolumeState{Running: true, Has: true}, false},
	} {
		e, _ := volumeEnv(t, tc.state, 64<<30)
		r := &linkRunner{a: e.a}
		size, live, free, err := r.VolumeFacts("cache")
		if err != nil || size != 64<<30 || live != tc.live || free != 1<<50 {
			t.Errorf("%+v: %d %v %d %v", tc.state, size, live, free, err)
		}
	}
	e, _ := volumeEnv(t, liveVM, 0)
	vmFreeSpace = func(string) (int64, error) { return 0, errors.New("statfs") }
	if size, _, free, err := (&linkRunner{a: e.a}).VolumeFacts("cache"); err != nil || size != 0 || free != -1 {
		t.Errorf("no disk, no statfs: %d %d %v", size, free, err)
	}
}

// The broker a link opens runs its allowed resizes with the link's
// resizer: the daemon's jobs, or none.
func TestLinkBrokerResizer(t *testing.T) {
	e, _ := volumeEnv(t, liveVM, 0)
	jobs := volumeJobs{env: "default"}
	if got := (&linkRunner{a: e.a, resizer: jobs}).brokerOptions(nil, nil).Resizer; got != broker.Resizer(jobs) {
		t.Errorf("resizer %+v", got)
	}
	if got := (&linkRunner{a: e.a}).brokerOptions(nil, nil).Resizer; got != nil {
		t.Errorf("a helper's link resizes: %+v", got)
	}
}
