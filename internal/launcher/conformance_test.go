package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/matrix"
)

// confPrintEnv makes the test binary print its environment and exit
// (TestMain): a child's view of what the suite's process holds.
const confPrintEnv = "CONF_TEST_PRINT_ENV"

const confTestID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// confUnderTemp fails the test unless p is inside os.TempDir(): every
// test whose code removes asserts it of its root first.
func confUnderTemp(t *testing.T, p string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(tmp, rp); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Fatalf("%s is not inside %s", rp, tmp)
	}
}

// confTestFacts are facts of a fake caller: a fake /tmp and a fake home,
// both temp dirs, and no sign of a sandbox.
func confTestFacts(t *testing.T) conformance.Facts {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	confUnderTemp(t, tmp)
	return conformance.Facts{
		TempRoot:    tmp,
		CabooseHome: filepath.Join(real, ".caboose"),
		UserHome:    real,
		RealRoots:   func() ([]string, error) { return []string{filepath.Join(real, "dev")}, nil },
		Detectors:   []conformance.Detector{},
		GOOS:        "darwin",
	}
}

func confTestGuard(t *testing.T, iso, trust string) *conformance.Guard {
	t.Helper()
	g, err := conformance.Setup(confTestFacts(t), iso, trust)
	if err != nil {
		t.Fatal(err)
	}
	confUnderTemp(t, g.Base())
	return g
}

// confInspection is what a sandbox the suite made looks like to the
// audit; the directories it mounts are made.
func confInspection(t *testing.T, g *conformance.Guard) conformance.Inspection {
	t.Helper()
	for _, d := range []string{g.DataRel() + "/home/.config", g.DataRel() + "/claude-code"} {
		if err := g.MakeDir(d); err != nil {
			t.Fatal(err)
		}
	}
	ro, dev := g.RootNames()[0], g.RootNames()[1]
	return conformance.Inspection{
		Isolation: g.Isolation(),
		Name:      config.ContainerFor(g.Env()),
		ID:        confTestID,
		Labels:    map[string]string{assets.LabelIsolation: g.Isolation()},
		Mounts: []conformance.Mount{
			{Source: g.RootPath(ro), Target: "/work/" + ro, ReadOnly: true},
			{Source: g.RootPath(dev), Target: "/work/" + dev, ReadOnly: true},
			{Source: filepath.Join(g.RootPath(dev), conformance.WritablePart), Target: "/work/" + dev + "/" + conformance.WritablePart},
			{Source: filepath.Join(g.DataDir(), "home", ".config"), Target: "/home/agent/.config"},
			{Source: filepath.Join(g.DataDir(), "claude-code"), Target: "/etc/claude-code", ReadOnly: true},
		},
		Hello:  &conformance.Hello{},
		Config: conformance.ConfigFacts{SSHAgent: "none", ForwardPorts: "none"},
	}
}

// fakeEnviron is a process environment of the test's own.
type fakeEnviron struct{ env []string }

func (f *fakeEnviron) get() []string { return slices.Clone(f.env) }
func (f *fakeEnviron) set(e []string) error {
	f.env = slices.Clone(e)
	return nil
}

// The suite's process environment is the throwaway environment's alone:
// what the caller had of CABOOSE_* or docker's endpoint reaches neither
// the environment the launcher loads nor a child.
func TestConfScrub(t *testing.T) {
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	fe := &fakeEnviron{env: []string{
		"PATH=/usr/bin", "HOME=/h", "CABOOSE_ENV=default", "CABOOSE_DATA_DIR=/h/.caboose/envs/default/data",
		"CABOOSE_HOME=/h/.caboose", "DOCKER_HOST=tcp://elsewhere", "DOCKER_CONTEXT=other", "CABOOSE_SESSION=s",
	}}
	got, err := confScrub(g, fe.get, fe.set)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "HOME=/h", "CABOOSE_HOME=" + g.CabooseHome(),
		"CABOOSE_ENV=" + g.Env(), "CABOOSE_NO_AUTO_UPDATE=1"}
	if !slices.Equal(got, want) || !slices.Equal(fe.env, want) {
		t.Fatalf("scrubbed to %q, want %q", got, want)
	}
	// The data dir is the environment's default: a CABOOSE_DATA_DIR, even
	// naming it, would keep the link from the daemon, and is refused.
	for _, kv := range []string{"CABOOSE_DATA_DIR=" + g.DataDir(), "CABOOSE_DATA_DIR=/h/.caboose/envs/default/data"} {
		if err := g.CheckLaunchEnv(append(slices.Clone(want), kv)); err == nil {
			t.Errorf("an environment with %s was taken", kv)
		}
	}
	// A set that does not take is refused.
	fe.env = append(fe.env, "CABOOSE_ENV=default")
	if _, err := confScrub(g, fe.get, func([]string) error { return nil }); err == nil {
		t.Fatal("an environment still holding the caller's CABOOSE_ENV was taken")
	}
}

// The real process environment is replaced, and a child inherits only
// it.
func TestConfScrubProcess(t *testing.T) {
	saved := os.Environ()
	t.Cleanup(func() { _ = setProcessEnv(saved) })
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	t.Setenv("CABOOSE_ENV", "default")
	t.Setenv("CABOOSE_DATA_DIR", "/elsewhere/data")
	t.Setenv("DOCKER_HOST", "tcp://elsewhere:2375")
	t.Setenv("CABOOSE_FORCE", "1")
	if _, err := confScrub(g, os.Environ, setProcessEnv); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), confPrintEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	child := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, kv := range child {
		k, _, _ := strings.Cut(kv, "=")
		if k == "DOCKER_HOST" || k == "DOCKER_CONTEXT" || strings.HasPrefix(k, "CABOOSE_") && !slices.Contains(
			[]string{"CABOOSE_HOME", "CABOOSE_ENV", "CABOOSE_NO_AUTO_UPDATE"}, k) {
			t.Errorf("the child has %s", kv)
		}
	}
	for _, kv := range []string{"CABOOSE_ENV=" + g.Env(), "CABOOSE_HOME=" + g.CabooseHome(), "CABOOSE_NO_AUTO_UPDATE=1"} {
		if !slices.Contains(child, kv) {
			t.Errorf("the child lacks %s", kv)
		}
	}
}

// The suite's config.toml loads, with the launcher's own loader, as what
// the suite wrote, under every isolation and trust.
func TestConfConfigLoads(t *testing.T) {
	for _, iso := range config.Kinds {
		for _, trust := range []string{config.TrustFallible, config.TrustHostile} {
			t.Run(iso+"-"+trust, func(t *testing.T) {
				g := confTestGuard(t, iso, trust)
				s := confSettingsFor(g)
				als, err := conformance.SuiteAliases(filepath.Join(g.CabooseHome(), "scripts"), filepath.Join(g.Base(), "canary", "out"),
					filepath.Join(g.RootPath(g.RootNames()[1]), conformance.WritablePart))
				if err != nil {
					t.Fatal(err)
				}
				s.Aliases = als
				if err := writeConfConfig(g, s); err != nil {
					t.Fatal(err)
				}
				if err := writeConfConfig(g, s); err == nil {
					t.Fatal("the config.toml was written over")
				}
				environ := g.LaunchEnv([]string{"HOME=" + t.TempDir()})
				cfg, err := config.Load(envLookup(environ), config.OSFS{}, g.Env())
				if err != nil {
					t.Fatal(err)
				}
				if err := checkConfConfig(cfg, s); err != nil {
					t.Fatal(err)
				}
				if len(cfg.Aliases) != len(als) {
					t.Fatalf("%d aliases loaded, %d written", len(cfg.Aliases), len(als))
				}
				if iso == config.KindVM && trust == config.TrustHostile && len(cfg.EgressAllow) == 0 {
					t.Error("a hostile VM's egress_allow is empty")
				}
				if cfg.OpenURLs != "allow" {
					t.Errorf("open_urls is %q", cfg.OpenURLs)
				}
			})
		}
	}
}

func TestConfConfigRefusesOddValues(t *testing.T) {
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	s := confSettingsFor(g)
	s.Roots[0].Host = `/tmp/a"b`
	if _, err := confConfigText(s); err == nil {
		t.Error("a quote in a path was written")
	}
	s = confSettingsFor(g)
	s.OpenURLs = "ask"
	if _, err := confConfigText(s); err == nil {
		t.Error("open_urls ask was written")
	}
}

// The App the suite drives is built from the scrubbed environment and B
// alone, and pinned to the engine it was given.
func TestConfApp(t *testing.T) {
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	if err := writeConfConfig(g, confSettingsFor(g)); err != nil {
		t.Fatal(err)
	}
	environ := g.LaunchEnv([]string{"HOME=" + t.TempDir(), "CABOOSE_ENV=default", "DOCKER_HOST=tcp://x", "CABOOSE_DATA_DIR=/x"})
	var out bytes.Buffer
	a, err := confApp(g, environ, "unix:///engine.sock", "", &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if a.Cfg.Env != g.Env() || a.Cfg.DataDir != g.DataDir() || a.Cfg.CabooseHome != g.CabooseHome() || a.Cfg.Container != config.ContainerFor(g.Env()) {
		t.Fatalf("the App is for %s, %s, %s", a.Cfg.Env, a.Cfg.DataDir, a.Cfg.CabooseHome)
	}
	if a.Docker.Host != "unix:///engine.sock" {
		t.Fatalf("the engine is %q", a.Docker.Host)
	}
	for _, extra := range []string{"CABOOSE_ENV=default", "DOCKER_HOST=tcp://x", "DOCKER_CONTEXT=c", "CABOOSE_SESSION=s"} {
		if _, err := confApp(g, append(slices.Clone(environ), extra), "unix:///engine.sock", "", &out, &out); err == nil {
			t.Errorf("an environment with %s was taken", extra)
		}
	}
	if _, err := confApp(g, environ, "", "", &out, &out); err == nil {
		t.Error("a container App with no pinned engine was built")
	}
}

// The canaries are made where the checks look: the managed directory's
// after the launch, the checkpoint store's once the store is.
func TestConfLayout(t *testing.T) {
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	before, store, after, err := confLayout(g)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 || len(store) != 1 || len(after) == 0 {
		t.Fatalf("%d canaries before the launch, %d in the store, %d after", len(before), len(store), len(after))
	}
	for _, s := range after {
		if s.Parent != g.DataRel()+"/claude-code" {
			t.Errorf("%s after the launch, in %s", s.Role, s.Parent)
		}
	}
	if store[0].Parent != g.StoreRel() {
		t.Errorf("%s in %s", store[0].Role, store[0].Parent)
	}
	h := newConfHost(g, nil)
	for _, d := range confLayoutDirs(g) {
		if err := g.MakeDir(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := makeCanaries(h, before); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{g.DataRel() + "/claude-code", g.StoreRel()} {
		if err := g.MakeDir(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := makeCanaries(h, after); err != nil {
		t.Fatal(err)
	}
	if err := makeCanaries(h, store); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"outside", "daemon", "grants", "cfg", "cred"} {
		c, err := h.Canary(role)
		if err != nil || c.Rel != "canary/"+g.CanaryName(role) {
			t.Errorf("watched canary %s: %+v, %v", role, c, err)
		}
	}
	if c, err := h.Canary("ckpt"); err != nil || c.Rel != g.StoreRel()+"/"+g.CanaryName("ckpt") || c.Kind != conformance.File {
		t.Errorf("the store's canary: %+v, %v", c, err)
	}
	if c, err := h.Canary("alias-out"); err != nil || filepath.Join(g.Base(), c.Rel) != confAliasOut(g) {
		t.Errorf("the alias-out canary is %+v (%v), the aliases write into %s", c, err, confAliasOut(g))
	}
}

// The full check list: every group's, the file checks last and the wipe
// last of all; under trust hostile with no VM, T1 alone.
func TestConfChecks(t *testing.T) {
	ids := func(cs []conformance.Check) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	got := ids(confChecks(config.KindContainer, config.TrustFallible))
	for _, want := range []string{"R1", "R13", "R14", "H1", "C1", "E8"} {
		if !slices.Contains(got, want) {
			t.Errorf("no %s in a container run: %v", want, got)
		}
	}
	if got[len(got)-1] != "R14" {
		t.Errorf("the last check is %s", got[len(got)-1])
	}
	if i, j := slices.Index(got, "R13"), slices.Index(got, "C1"); i < j {
		t.Errorf("the file checks run before the checkpoint checks: %v", got)
	}
	// As the runner orders them: every destructive check after every other
	// one, whatever its group, and the file checks' destructive ones (which
	// unmount, and the wipe) last.
	for _, run := range [][2]string{
		{config.KindContainer, config.TrustFallible}, {config.KindGVisor, config.TrustFallible},
		{config.KindVM, config.TrustFallible}, {config.KindVM, config.TrustHostile},
	} {
		ordered, err := conformance.RunOrder(confChecks(run[0], run[1]))
		if err != nil {
			t.Fatalf("%s %s: %v", run[0], run[1], err)
		}
		seenDestructive := false
		for _, c := range ordered {
			if c.Destructive() {
				seenDestructive = true
			} else if seenDestructive {
				t.Errorf("%s %s: %s runs after a destructive check: %v", run[0], run[1], c.ID, ids(ordered))
			}
		}
		if last := ordered[len(ordered)-1]; !seenDestructive || last.Group != "files" {
			t.Errorf("%s %s: the last check is %s, of group %q", run[0], run[1], last.ID, last.Group)
		}
	}
	for _, iso := range []string{config.KindContainer, config.KindGVisor} {
		if got := ids(confChecks(iso, config.TrustHostile)); !slices.Equal(got, []string{"T1"}) {
			t.Errorf("%s hostile runs %v", iso, got)
		}
	}
	vm := ids(confChecks(config.KindVM, config.TrustHostile))
	if len(vm) < 10 || !slices.Contains(vm, "R1") || !slices.Contains(vm, "E1") {
		t.Errorf("vm hostile runs %v", vm)
	}
	// The daemon checks, in group daemon, in every run that makes a
	// sandbox, before the file checks (whose last ones unmount and wipe).
	daemonIDs := []string{"D0", "D1", "D2", "D3", "D4", "X2", "D5", "D6", "D7", "D8", "D9", "X1"}
	for _, run := range [][2]string{
		{config.KindContainer, config.TrustFallible}, {config.KindGVisor, config.TrustFallible},
		{config.KindVM, config.TrustFallible}, {config.KindVM, config.TrustHostile},
	} {
		cs := confChecks(run[0], run[1])
		got := ids(cs)
		r1 := slices.Index(got, "R1")
		last := -1
		for _, id := range daemonIDs {
			i := slices.Index(got, id)
			switch {
			case i < 0:
				t.Errorf("%s %s: no %s", run[0], run[1], id)
				continue
			case cs[i].Group != "daemon":
				t.Errorf("%s is in group %q", id, cs[i].Group)
			case i < last:
				t.Errorf("%s %s: %s out of order: %v", run[0], run[1], id, got)
			case r1 >= 0 && i > r1:
				t.Errorf("%s %s: %s runs after the file checks: %v", run[0], run[1], id, got)
			}
			last = i
		}
	}
	seen := map[string]bool{}
	for _, id := range ids(confEvery()) {
		if seen[id] {
			t.Errorf("%s twice", id)
		}
		seen[id] = true
	}
}

func TestConfList(t *testing.T) {
	var out bytes.Buffer
	if code := Conformance([]string{"list"}, ConformanceIO{Stdout: &out, Stderr: &out}); code != 0 {
		t.Fatal(code)
	}
	for _, c := range confEvery() {
		if !strings.Contains(out.String(), c.ID+" ") {
			t.Errorf("list lacks %s", c.ID)
		}
	}
}

func TestConfDocCompare(t *testing.T) {
	all := []conformance.Check{{ID: "R1"}, {ID: "R2"}, {ID: "R3"}}
	unknown, uncited := confDocCompare("see `R1` and `X9`, not R2", nil, all)
	if !slices.Equal(unknown, []string{"X9"}) || !slices.Equal(uncited, []string{"R2", "R3"}) {
		t.Fatalf("unknown %v, uncited %v", unknown, uncited)
	}
	// A matrix row's ids count as cited.
	unknown, uncited = confDocCompare("see `R1`", []string{"R2", "R3"}, all)
	if len(unknown) != 0 || len(uncited) != 0 {
		t.Fatalf("with rows: unknown %v, uncited %v", unknown, uncited)
	}
}

func TestConfExitCode(t *testing.T) {
	res := func(ss ...conformance.Status) []conformance.Result {
		var out []conformance.Result
		for i, s := range ss {
			out = append(out, conformance.Result{ID: string(rune('A' + i)), Outcome: conformance.Outcome{Status: s}})
		}
		return out
	}
	ref := &conformance.RefusalError{Item: "x", Err: errors.New("x")}
	for _, c := range []struct {
		r    []conformance.Result
		err  error
		want int
	}{
		{res(conformance.Pass, conformance.Gap, conformance.Skip, conformance.Info), nil, 0},
		{res(conformance.Pass, conformance.Fail), nil, 1},
		{res(conformance.Pass, conformance.Error), nil, 2},
		{res(conformance.Fail, conformance.Error), nil, 1},
		{res(conformance.Pass), errors.New("interrupted"), 2},
		{res(conformance.Fail), ref, 2},
		{nil, ref, 2},
	} {
		if got := confExitCode(c.r, c.err); got != c.want {
			t.Errorf("%v %v: %d, want %d", c.r, c.err, got, c.want)
		}
	}
}

// fakeDriver is a driver whose sandbox is a recording fake that answers
// every command with success.
type fakeDriver struct {
	t       *testing.T
	g       *conformance.Guard
	ups     int
	onUp    func(ctx context.Context) error
	removed []string
	onRm    func()
	sb      *fakeConfSandbox
}

func (d *fakeDriver) Prepare(context.Context) error { return nil }
func (d *fakeDriver) Up(ctx context.Context) (conformance.Inputs, error) {
	d.ups++
	if d.onUp != nil {
		if err := d.onUp(ctx); err != nil {
			return conformance.Inputs{}, err
		}
	}
	in := confInspection(d.t, d.g)
	return conformance.Inputs{
		Inspect: func(context.Context) (conformance.Inspection, error) { return in, nil },
		Pin: func(context.Context) (conformance.Pins, error) {
			return conformance.Pins{EngineID: "E", ContainerID: confTestID}, nil
		},
	}, nil
}
func (d *fakeDriver) Sandbox() conformance.Sandbox  { return d.sb }
func (d *fakeDriver) Host() conformance.Host        { return &conformance.GuardHost{G: d.g} }
func (d *fakeDriver) Restart(context.Context) error { return nil }
func (d *fakeDriver) Remover() conformance.Remover  { return d }
func (d *fakeDriver) Engine() string                { return "fake" }
func (d *fakeDriver) RemoveImage(_ context.Context, n string) error {
	d.removed = append(d.removed, n)
	return nil
}
func (d *fakeDriver) RemoveContainer(_ context.Context, n string) error {
	if d.onRm != nil {
		d.onRm()
	}
	d.removed = append(d.removed, n)
	return nil
}

type fakeConfSandbox struct{ argv [][]string }

func (s *fakeConfSandbox) Exec(_ context.Context, _ string, argv []string, _ []byte) (int, string, string, error) {
	s.argv = append(s.argv, argv)
	return 0, "\nrc=0\n", "", nil
}
func (s *fakeConfSandbox) Mounts() []conformance.Mount { return nil }
func (s *fakeConfSandbox) Labels() map[string]string   { return nil }
func (s *fakeConfSandbox) State() string               { return conformance.StateRunning }

func passCheck(id string, class conformance.Class) conformance.Check {
	return conformance.Check{ID: id, Title: "check " + id, Control: "c", Step: "s", Group: "g", Class: class,
		Run: func(context.Context, conformance.Env) conformance.CheckResult {
			return conformance.CheckResult{Status: conformance.Pass}
		}}
}

func wipeCheck() conformance.Check {
	c := passCheck("W1", conformance.Destructive)
	c.Wipe = true
	c.Run = func(ctx context.Context, env conformance.Env) conformance.CheckResult {
		if _, err := env.Wipe(ctx); err != nil {
			return conformance.CheckResult{Status: conformance.Error, Reason: err.Error()}
		}
		return conformance.CheckResult{Status: conformance.Pass}
	}
	return c
}

// confWorld is one command run's fakes.
type confWorld struct {
	t      *testing.T
	cio    ConformanceIO
	out    bytes.Buffer
	errb   bytes.Buffer
	env    *fakeEnviron
	drv    *fakeDriver
	sigs   chan os.Signal
	exited chan int
	outF   string
}

func newConfWorld(t *testing.T, checks ...conformance.Check) *confWorld {
	t.Helper()
	w := &confWorld{t: t, env: &fakeEnviron{env: []string{"PATH=/usr/bin", "HOME=/h", "CABOOSE_ENV=default"}},
		sigs: make(chan os.Signal, 4), exited: make(chan int, 1)}
	w.outF = filepath.Join(t.TempDir(), "results.txt")
	w.drv = &fakeDriver{t: t, sb: &fakeConfSandbox{}}
	w.cio = ConformanceIO{
		Stdout: &w.out, Stderr: &w.errb,
		Environ: w.env.get, SetEnv: w.env.set,
		Facts: confTestFacts(t),
		Driver: func(g *conformance.Guard, _ []string, _ string) confDriver {
			w.drv.g = g
			return w.drv
		},
		Checks:  func(string, string) []conformance.Check { return checks },
		Signals: func() (<-chan os.Signal, func()) { return w.sigs, func() {} },
		Exit:    func(code int) { w.exited <- code },
		Now:     func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) },
	}
	return w
}

func (w *confWorld) run(args ...string) int {
	w.t.Helper()
	return Conformance(append([]string{"run", "--out", w.outF}, args...), w.cio)
}

// bases are the run directories left in the fake /tmp.
func (w *confWorld) bases() []string {
	w.t.Helper()
	m, err := filepath.Glob(filepath.Join(w.cio.Facts.TempRoot, conformance.BasePrefix+"*"))
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *confWorld) results() string {
	w.t.Helper()
	b, err := os.ReadFile(w.outF)
	if err != nil {
		w.t.Fatalf("%v (stderr: %s)", err, w.errb.String())
	}
	return string(b)
}

func TestConformanceRun(t *testing.T) {
	w := newConfWorld(t, passCheck("P1", conformance.Observe), wipeCheck())
	asked := 0
	w.cio.OSVersion = func() string { asked++; return "15.3.1" }
	w.cio.GOOS = "darwin"
	if code := w.run("--isolation", "container"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	res := w.results()
	if asked != 1 || !strings.Contains(res, "host: darwin/") || !strings.Contains(res, "os: macOS 15.3.1\n") {
		t.Errorf("os version asked %d times; results:\n%s", asked, res)
	}
	for _, want := range []string{"PASS P1 check P1", "PASS W1 check W1", "isolation: container", "summary: 2 checks: 2 PASS"} {
		if !strings.Contains(res, want) {
			t.Errorf("the results lack %q:\n%s", want, res)
		}
	}
	if strings.Contains(res, w.drv.g.Base()) {
		t.Error("the results name the base")
	}
	container, images := w.drv.g.ExactNames()
	if want := append([]string{container}, images...); !slices.Equal(w.drv.removed, want) {
		t.Errorf("removed %v, want %v", w.drv.removed, want)
	}
	if b := w.bases(); len(b) != 0 {
		t.Errorf("left %v", b)
	}
	if !slices.Contains(w.env.env, "CABOOSE_ENV="+w.drv.g.Env()) || slices.Contains(w.env.env, "CABOOSE_ENV=default") {
		t.Errorf("the process environment is %v", w.env.env)
	}
	roots := w.drv.g.RootNames()
	if len(w.drv.sb.argv) != 1 || !strings.Contains(strings.Join(w.drv.sb.argv[0], " "), "rm -rf /work/"+roots[0]+" /work/"+roots[1]) {
		t.Errorf("the sandbox ran %q", w.drv.sb.argv)
	}
}

func TestConformanceSkipWipe(t *testing.T) {
	w := newConfWorld(t, passCheck("P1", conformance.Observe), wipeCheck())
	if code := w.run("--isolation", "container", "--skip-wipe"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	if res := w.results(); !strings.Contains(res, "SKIP W1") || !strings.Contains(res, "PASS P1") {
		t.Fatalf("results:\n%s", res)
	}
	if len(w.drv.sb.argv) != 0 {
		t.Errorf("the sandbox ran %q", w.drv.sb.argv)
	}
	if out := w.out.String(); !strings.Contains(out, "checks, in the order they run: P1\n") || !strings.Contains(out, "skipped: W1 (--skip-wipe)") {
		t.Errorf("the plan lists the skipped wipe under checks:\n%s", out)
	}
}

// Under trust hostile with a container, the run is T1 alone, and no
// sandbox is brought up.
func TestConformanceHostileContainer(t *testing.T) {
	w := newConfWorld(t)
	w.cio.Checks = confChecks
	code := w.run("--isolation", "container", "--trust", "hostile")
	if w.drv.ups != 0 {
		t.Error("a sandbox was brought up")
	}
	res := w.results()
	n := 0
	for _, l := range strings.Split(res, "\n") {
		if strings.HasPrefix(l, "PASS ") || strings.HasPrefix(l, "FAIL ") || strings.HasPrefix(l, "ERROR ") || strings.HasPrefix(l, "SKIP ") || strings.HasPrefix(l, "GAP ") {
			n++
			if !strings.Contains(l, " T1 ") {
				t.Errorf("ran %s", l)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d results:\n%s", n, res)
	}
	// The host calls T1 needs are not built yet: an ERROR, never a pass.
	if !strings.Contains(res, "ERROR T1") || code != 2 {
		t.Errorf("exit %d:\n%s", code, res)
	}
	if b := w.bases(); len(b) != 0 {
		t.Errorf("left %v", b)
	}
}

func TestConformanceRefusesToStart(t *testing.T) {
	cases := map[string]func(w *confWorld) []string{
		"the results file exists": func(w *confWorld) []string {
			if err := os.WriteFile(w.outF, []byte("mine"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"--isolation", "container"}
		},
		"in a sandbox": func(w *confWorld) []string {
			w.cio.Facts.Detectors = []conformance.Detector{{Name: "test sign", Present: func() (bool, error) { return true, nil }}}
			return []string{"--isolation", "container"}
		},
		"an unknown isolation": func(w *confWorld) []string { return []string{"--isolation", "chroot"} },
		"no isolation":         func(w *confWorld) []string { return nil },
		"an unknown check":     func(w *confWorld) []string { return []string{"--isolation", "vm", "--only", "Z9"} },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := newConfWorld(t, passCheck("P1", conformance.Observe))
			args := setup(w)
			if code := w.run(args...); code != 2 {
				t.Fatalf("exit %d", code)
			}
			if b := w.bases(); len(b) != 0 {
				t.Errorf("created %v", b)
			}
			if w.drv.g != nil {
				t.Error("a driver was made")
			}
			if b, _ := os.ReadFile(w.outF); len(b) != 0 && string(b) != "mine" {
				t.Errorf("results written: %s", b)
			}
		})
	}
}

// An interrupt during the run cancels it, and the teardown runs; another
// during the teardown says what is left.
func TestConformanceInterrupt(t *testing.T) {
	w := newConfWorld(t, passCheck("P1", conformance.Observe))
	w.drv.onUp = func(ctx context.Context) error {
		w.sigs <- os.Interrupt
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("the interrupt did not cancel the run")
		}
	}
	w.drv.onRm = func() {
		w.sigs <- os.Interrupt
		select {
		case <-w.exited:
		case <-time.After(10 * time.Second):
			t.Error("a second interrupt during the teardown did not end the run")
		}
	}
	if code := w.run("--isolation", "container"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if len(w.drv.removed) == 0 {
		t.Fatal("the teardown did not run")
	}
	if !strings.Contains(w.errb.String(), "interrupted; what may be left") || !strings.Contains(w.errb.String(), "docker rm -f "+config.ContainerFor(w.drv.g.Env())) {
		t.Errorf("stderr: %s", w.errb.String())
	}
}

func TestConformanceKeep(t *testing.T) {
	w := newConfWorld(t, passCheck("P1", conformance.Observe))
	if code := w.run("--isolation", "container", "--keep"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	if len(w.drv.removed) != 0 {
		t.Errorf("removed %v", w.drv.removed)
	}
	if b := w.bases(); len(b) != 1 || !strings.Contains(w.out.String(), "rm -rf "+b[0]) {
		t.Errorf("bases %v, said %s", b, w.out.String())
	}
}

// A teardown whose guard no longer verifies deletes nothing.
func TestConformanceTeardownRefuses(t *testing.T) {
	spoil := passCheck("P1", conformance.Observe)
	var base string
	spoil.Run = func(context.Context, conformance.Env) conformance.CheckResult {
		return conformance.CheckResult{Status: conformance.Pass}
	}
	w := newConfWorld(t, spoil)
	w.drv.onUp = func(context.Context) error {
		base = w.drv.g.Base()
		return nil
	}
	// The marker is changed once the checks are done, as the results are
	// written.
	w.cio.Now = func() time.Time {
		if base != "" {
			_ = os.WriteFile(filepath.Join(base, conformance.MarkerFile), []byte("{}"), 0o600)
		}
		return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	}
	if code := w.run("--isolation", "container"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if len(w.drv.removed) != 0 {
		t.Errorf("removed %v", w.drv.removed)
	}
	if b := w.bases(); len(b) != 1 {
		t.Errorf("bases %v", b)
	}
	if !strings.Contains(w.errb.String(), "removed by hand") {
		t.Errorf("stderr: %s", w.errb.String())
	}
}

// leftovers lists, and removes nothing.
func TestConfLeftovers(t *testing.T) {
	w := newConfWorld(t)
	g, err := conformance.Setup(w.cio.Facts, config.KindContainer, config.TrustFallible)
	if err != nil {
		t.Fatal(err)
	}
	// A base an earlier version of the suite left, its environment named
	// conformance-<8 hex>, under vm.
	old := filepath.Join(w.cio.Facts.TempRoot, conformance.BasePrefix+"4294967295")
	if err := os.MkdirAll(filepath.Join(old, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, filepath.FromSlash(conformance.MarkerFile)),
		[]byte(`{"suite_version":"1","env":"conformance-0011aabb","isolation":"vm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The caller's own environments: cf-44556677 has a container and an
	// image of the suite's shape, which are listed with no command; so is
	// the one the run's marker names, which is listed with its command.
	for _, e := range []string{"cf-44556677", g.Env()} {
		if err := os.MkdirAll(filepath.Join(w.cio.Facts.CabooseHome, "envs", e), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	script := "#!/bin/sh\ncase \"$1\" in\nps) printf 'caboose-cf-44556677\\ncaboose-%[1]s\\ncaboose-default\\ncaboose-cf-2233ccdd\\ncaboose-conformance-0011aabb\\ncaboose-conformance-x\\ncaboose-cf-x\\n' ;;\n" +
		"image) printf 'caboose-base:cf-44556677\\ncaboose:default\\ncaboose:cf-2233ccdd\\ncaboose:conformance-0011aabb\\ncaboose-base:conformance-0011aabb\\n' ;;\nesac\n"
	script = fmt.Sprintf(script, g.Env())
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w.cio.DockerPath = fake
	if code := Conformance([]string{"leftovers"}, w.cio); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	out := w.out.String()
	for _, want := range []string{
		"rm -rf " + g.Base(), "environment " + g.Env(), "the suite's daemon, if one runs", "docker rm -f caboose-conformance-0011aabb",
		"docker rm -f caboose-cf-2233ccdd", "docker image rm caboose:cf-2233ccdd",
		"rm -rf " + old, "environment conformance-0011aabb, vm",
		"docker rm -f caboose-" + g.Env(),
		"container caboose-cf-44556677\n  not the suite's: cf-44556677 is an environment of yours",
		"image caboose-base:cf-44556677\n  not the suite's: cf-44556677 is an environment of yours",
		filepath.Join(old, "home/envs/conformance-0011aabb/data/vm/caboose-conformance-0011aabb/vmm.pid"),
		"docker image rm caboose:conformance-0011aabb", "docker image rm caboose-base:conformance-0011aabb",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("leftovers lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"caboose-default", "caboose:default", "conformance-x", "cf-x", "rm -f caboose-cf-44556677", "rm caboose-base:cf-44556677"} {
		if strings.Contains(out, not) {
			t.Errorf("leftovers names %s", not)
		}
	}
	if _, err := os.Stat(g.Base()); err != nil {
		t.Errorf("the base is gone: %v", err)
	}
}

// A name of the suite's shape is not said to be removable when whether
// it is the caller's cannot be told.
func TestConfLeftoversUnsure(t *testing.T) {
	w := newConfWorld(t)
	if err := os.WriteFile(w.cio.Facts.CabooseHome, nil, 0o600); err != nil { // envs/ cannot be read
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncase \"$1\" in\nps) echo caboose-cf-2233ccdd ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.cio.DockerPath = fake
	if code := Conformance([]string{"leftovers"}, w.cio); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	if out := w.out.String(); !strings.Contains(out, "maybe not the suite's") || strings.Contains(out, "docker rm") {
		t.Errorf("leftovers:\n%s", out)
	}
}

// The suite's layout fits a VM's socket on a Mac, where B is in
// /private/tmp and its random part is os.MkdirTemp's longest (a uint32 in
// decimal), with room to spare: the socket is B/home/envs/<env>/data/vm/
// caboose-<env>/vm.sock, and checkVM refuses one over maxSocketPath.
func TestConfLayoutFitsVMSocket(t *testing.T) {
	g := confTestGuard(t, config.KindVM, config.TrustHostile)
	if r := strings.TrimPrefix(filepath.Base(g.Base()), conformance.BasePrefix); len(r) > 10 {
		t.Fatalf("B's random part %q is longer than a uint32's", r)
	}
	worst := "/private/tmp/" + conformance.BasePrefix + "4294967295"
	if !conformance.ValidEnvName(g.Env()) || len(g.Env()) != len(conformance.EnvPrefix)+8 {
		t.Fatalf("environment %q", g.Env())
	}
	a := &App{Cfg: &config.Config{
		DataDir:   filepath.Join(worst, filepath.FromSlash(g.DataRel())),
		Container: config.ContainerFor(g.Env()),
	}}
	const margin = 8
	for _, name := range []string{a.Cfg.Container, builderName} {
		s := a.vmDir(name).Socket()
		if len(s) > maxSocketPath-margin {
			t.Errorf("%s is %d bytes: over %d, macOS's %d less a margin of %d", s, len(s), maxSocketPath-margin, maxSocketPath, margin)
		}
	}
}

// The suite's daemon is asked nothing unless its directory is inside the
// throwaway CABOOSE_HOME: a daemon directory elsewhere is the person's
// daemon's, which is never dialled.
func TestConfDaemonCheckDirInside(t *testing.T) {
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	d := newConfDaemon(g, nil, "")
	dialled := 0
	app := func(home string) *App {
		return &App{Cfg: &config.Config{CabooseHome: home, Getenv: func(string) string { return "" }},
			Stdout: io.Discard, Stderr: io.Discard,
			DaemonDial: func(context.Context, bool) (DaemonCaller, error) {
				dialled++
				return nil, errors.New("no daemon here")
			}}
	}
	err := d.check(context.Background(), app(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "is not in") || dialled != 0 {
		t.Errorf("a daemon directory outside the throwaway CABOOSE_HOME: %v, dialled %d", err, dialled)
	}
	err = d.check(context.Background(), app(g.CabooseHome()))
	if err == nil || !strings.Contains(err.Error(), "does not answer") || dialled != 1 {
		t.Errorf("the suite's daemon directory: %v, dialled %d, want it dialled", err, dialled)
	}
}

// verify-doc fails on a check that neither the prose nor a matrix row cites.
func TestConfVerifyDocUncited(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"security.md", "security-matrix.json"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", f))
		if err != nil {
			t.Fatal(err)
		}
		b = bytes.ReplaceAll(b, []byte("R12"), []byte("R1"))
		if err := os.WriteFile(filepath.Join(root, "docs", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	cio := ConformanceIO{Stdout: &out, Stderr: &errb, Checkout: root}
	if code := Conformance([]string{"verify-doc"}, cio); code != 1 || !strings.Contains(out.String(), "not cited: R12") || !strings.Contains(out.String(), "is in no row") {
		t.Fatalf("verify-doc %d: %s", code, out.String())
	}
}

func TestConfMatrixCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"security.md", "security-matrix.json"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", f))
		if err != nil {
			t.Fatal(err)
		}
		// The repo's matrix holds real runs' results; the test starts
		// from none, so what it stores is its own.
		if f == "security-matrix.json" {
			d, err := matrix.ParseData(b)
			if err != nil {
				t.Fatal(err)
			}
			d.Results = nil
			if b, err = d.Marshal(); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "docs", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep := conformance.Report{
		Header: conformance.Header{SuiteVersion: "1", CabooseVersion: "v0.4.0", OS: "darwin", Arch: "arm64", OSVersion: "15.3.1", Engine: "e",
			Isolation: "vm", Trust: "hostile", Date: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)},
		Entries: []conformance.Entry{{ID: "R12", Title: "t", Status: conformance.Pass}},
	}
	txt, err := rep.Format("/b", nil)
	if err != nil {
		t.Fatal(err)
	}
	rf := filepath.Join(root, "results.txt")
	if err := os.WriteFile(rf, []byte(txt), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	cio := ConformanceIO{Stdout: &out, Stderr: &errb, Checkout: root}
	if code := Conformance([]string{"matrix", rf}, cio); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "vm/hostile: 1 results stored") || !strings.Contains(out.String(), "container: 0 results stored") {
		t.Fatalf("%s", out.String())
	}
	data, _ := os.ReadFile(filepath.Join(root, "docs", "security-matrix.json"))
	if !strings.Contains(string(data), `"R12"`) || !strings.Contains(string(data), `"2026-10-18"`) {
		t.Fatalf("data: %s", data)
	}
	doc, _ := os.ReadFile(filepath.Join(root, "docs", "security.md"))
	if !strings.Contains(string(doc), "(tested 2026-10-18 on macOS 15.3.1 (e), caboose v0.4.0: `R12`)") {
		t.Fatal("table not rewritten")
	}
	// verify-doc agrees with the written files, and the repo's own docs
	// are untouched.
	out.Reset()
	if code := Conformance([]string{"verify-doc"}, cio); code != 0 {
		t.Fatalf("verify-doc %d: %s", code, out.String())
	}
	// A hand edit of the table is caught.
	if err := os.WriteFile(filepath.Join(root, "docs", "security.md"), []byte(strings.Replace(string(doc), "R12", "R13", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Conformance([]string{"verify-doc"}, cio); code != 1 {
		t.Fatalf("verify-doc after an edit: %d", code)
	}
	// Re-render with no file repairs it.
	if code := Conformance([]string{"matrix"}, cio); code != 0 {
		t.Fatal(code)
	}
	// A refused file leaves everything as it was and exits 2.
	docBefore, _ := os.ReadFile(filepath.Join(root, "docs", "security.md"))
	dataBefore, _ := os.ReadFile(filepath.Join(root, "docs", "security-matrix.json"))
	defer func() {
		a, _ := os.ReadFile(filepath.Join(root, "docs", "security.md"))
		b, _ := os.ReadFile(filepath.Join(root, "docs", "security-matrix.json"))
		if !bytes.Equal(a, docBefore) || !bytes.Equal(b, dataBefore) {
			t.Error("a refused file changed the docs")
		}
	}()
	bad := filepath.Join(root, "bad.txt")
	if err := os.WriteFile(bad, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Conformance([]string{"matrix", bad}, cio); code != 2 {
		t.Fatalf("bad file: %d", code)
	}
	if code := Conformance([]string{"matrix", root}, cio); code != 2 {
		t.Fatalf("directory: %d", code)
	}
	if code := Conformance([]string{"matrix"}, ConformanceIO{Stdout: &out, Stderr: &errb}); code != 2 {
		t.Fatalf("no checkout: %d", code)
	}
}

func TestConfMatrixRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "security-matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "security-matrix.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "security.md"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "elsewhere.md")
	if err := os.WriteFile(target, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "docs", "security.md")); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Conformance([]string{"matrix"}, ConformanceIO{Stdout: &out, Stderr: &errb, Checkout: root}); code != 2 {
		t.Fatalf("code %d", code)
	}
	after, _ := os.ReadFile(target)
	if !bytes.Equal(after, doc) {
		t.Fatal("the link's target changed")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "docs", "security-matrix.json")); !bytes.Equal(got, b) {
		t.Fatal("the data file changed")
	}
}

// The OS version comes from sw_vers on darwin alone, by absolute path,
// trimmed; anything that fails, or another OS, gives "".
func TestConfOSVersion(t *testing.T) {
	var calls []string
	run := func(out string, err error) func(context.Context, string, ...string) ([]byte, error) {
		return func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("no deadline")
			}
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte(out), err
		}
	}
	if got := confOSVersion("darwin", run("15.3.1\n", nil)); got != "15.3.1" {
		t.Errorf("got %q", got)
	}
	if !slices.Equal(calls, []string{"/usr/bin/sw_vers -productVersion"}) {
		t.Errorf("ran %v", calls)
	}
	calls = nil
	if got := confOSVersion("linux", run("6.8", nil)); got != "" || len(calls) != 0 {
		t.Errorf("linux: %q, ran %v", got, calls)
	}
	if got := confOSVersion("darwin", run("15.3.1", errors.New("no"))); got != "" {
		t.Errorf("failed run: %q", got)
	}
	if got := confOSVersion("darwin", run(strings.Repeat("1", 65), nil)); got != "" {
		t.Errorf("long output: %q", got)
	}
	if got := confOSVersion("darwin", nil); got != "" {
		t.Errorf("no runner: %q", got)
	}
}

// On linux the header has no os line, whatever the version says.
func TestConformanceRunLinuxHasNoOSLine(t *testing.T) {
	w := newConfWorld(t, passCheck("P1", conformance.Observe))
	w.cio.OSVersion = func() string { return "15.3.1" }
	w.cio.GOOS = "linux"
	if code := w.run("--isolation", "container"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	if res := w.results(); strings.Contains(res, "os: ") || !strings.Contains(res, "host: linux/") {
		t.Errorf("results:\n%s", res)
	}
}

// The plan lists the checks in the order the runner runs them: every
// destructive check after every other one, whatever its group.
func TestConfPlanOrder(t *testing.T) {
	ck := func(id, group string, class conformance.Class) conformance.Check {
		return conformance.Check{ID: id, Group: group, Class: class}
	}
	run := []conformance.Check{
		ck("A1", "a", conformance.Observe), ck("AD1", "a", conformance.Destructive), ck("AW1", "a", conformance.Write),
		ck("AD2", "a", conformance.Destructive), ck("A2", "a", conformance.Observe),
		ck("BW1", "b", conformance.Write), ck("BD1", "b", conformance.Destructive), ck("B1", "b", conformance.Observe),
		ck("BW2", "b", conformance.Write),
	}
	var out bytes.Buffer
	confPlan(&out, confOptions{isolation: "container", trust: "fallible", out: "/r"}, run, nil)
	for _, want := range []string{
		"checks, in the order they run: A1 A2 AW1 B1 BW1 BW2 AD1 AD2 BD1\n",
		"destructive, after every other check: AD1 AD2 BD1\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan lacks %q:\n%s", want, out.String())
		}
	}
}
