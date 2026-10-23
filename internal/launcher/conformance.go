package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/conformance/matrix"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/version"
)

// caboose conformance: the conformance suite's command. It is in no
// release binary (cmd/caboose registers it only under the conformance
// build tag); everything it does is here, untagged, so that the tests and
// go vet cover it.

// ConformanceIO is what the command reaches outside itself.
type ConformanceIO struct {
	Stdout, Stderr io.Writer
	// Environ and SetEnv read and replace the process environment.
	Environ func() []string
	SetEnv  func([]string) error
	// Facts are the guard's facts; TempRoot "" is the physical /tmp.
	Facts conformance.Facts
	// Driver makes the run's driver once the guard exists and the
	// environment is scrubbed.
	Driver func(g *conformance.Guard, environ []string, probeDir string) confDriver
	// Checks are the checks a run under isolation and trust has.
	Checks func(isolation, trust string) []conformance.Check
	// Signals delivers SIGINT and SIGTERM; stop ends the delivery.
	Signals func() (ch <-chan os.Signal, stop func())
	// Exit ends the process on a second interrupt, during the teardown.
	Exit func(code int)
	Now  func() time.Time
	// Checkout is the caboose checkout, for verify-doc; "" for none.
	Checkout string
	// Redact are strings no results line may hold (the host name, the
	// user's name and home).
	Redact []string
	// DockerPath is the docker leftovers lists with; "" is docker on PATH.
	DockerPath string
	// OSVersion is the host's OS version for the results header (macOS's
	// sw_vers on darwin); nil, or "", leaves the header's os line out.
	OSVersion func() string
	// GOOS is the host's OS for the results header; "" is runtime.GOOS.
	GOOS string
}

// DefaultConformanceIO is the real one.
func DefaultConformanceIO(stdout, stderr io.Writer, checkout string) ConformanceIO {
	facts := confFacts(os.Getenv)
	var redact []string
	if h, err := os.Hostname(); err == nil {
		redact = append(redact, h)
	}
	for _, k := range []string{"USER", "LOGNAME", "HOME"} {
		if v := os.Getenv(k); v != "" {
			redact = append(redact, v)
		}
	}
	return ConformanceIO{
		Stdout: stdout, Stderr: stderr,
		Environ: os.Environ, SetEnv: setProcessEnv,
		Facts: facts,
		Driver: func(g *conformance.Guard, environ []string, probeDir string) confDriver {
			return newConfReal(g, environ, checkout, probeDir, stdout, stderr)
		},
		Checks: confChecks,
		Signals: func() (<-chan os.Signal, func()) {
			ch := make(chan os.Signal, 2)
			signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
			return ch, func() { signal.Stop(ch) }
		},
		Exit:      os.Exit,
		Now:       time.Now,
		Checkout:  checkout,
		Redact:    redact,
		OSVersion: func() string { return confOSVersion(runtime.GOOS, runOutput) },
		GOOS:      runtime.GOOS,
	}
}

const conformanceUsage = `usage: caboose conformance run --isolation container|gvisor|vm [--trust fallible|hostile]
                               [--out FILE] [--skip-wipe] [--only ID,ID...] [--keep] [--probe-dir DIR]
       caboose conformance list | leftovers | verify-doc
       caboose conformance matrix [FILE...]`

// Conformance is `caboose conformance ARGS`; it returns the exit status:
// 0 when every check passed (or was a gap or skipped), 1 when one failed
// or was an error, 2 when the suite refused to start or to go on.
func Conformance(args []string, cio ConformanceIO) int {
	if len(args) == 0 {
		fmt.Fprintln(cio.Stderr, conformanceUsage)
		return 2
	}
	switch args[0] {
	case "run":
		return confRunCmd(args[1:], cio)
	case "list":
		if len(args) != 1 {
			break
		}
		return confList(cio.Stdout)
	case "leftovers":
		if len(args) != 1 {
			break
		}
		return confLeftovers(cio)
	case "verify-doc":
		if len(args) != 1 {
			break
		}
		return confVerifyDoc(cio)
	case "matrix":
		return confMatrix(args[1:], cio)
	}
	fmt.Fprintln(cio.Stderr, conformanceUsage)
	return 2
}

// confOptions are run's flags.
type confOptions struct {
	isolation, trust string
	out              string
	skipWipe, keep   bool
	only             []string
	probeDir         string
}

func parseConfRun(args []string, stderr io.Writer, now time.Time) (confOptions, error) {
	var o confOptions
	var only string
	fs := flag.NewFlagSet("caboose conformance run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.isolation, "isolation", "", "container, gvisor or vm")
	fs.StringVar(&o.trust, "trust", config.TrustFallible, "fallible or hostile")
	fs.StringVar(&o.out, "out", "", "the results file, which must not exist")
	fs.BoolVar(&o.skipWipe, "skip-wipe", false, "do not run the wipe (R14)")
	fs.BoolVar(&o.keep, "keep", false, "leave the environment for inspection")
	fs.StringVar(&only, "only", "", "run only these check ids")
	fs.StringVar(&o.probeDir, "probe-dir", "", "the directory make conformance-probe built the probe into")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	if !slices.Contains(config.Kinds, o.isolation) {
		return o, fmt.Errorf("--isolation must be one of %s", strings.Join(config.Kinds, ", "))
	}
	if o.trust != config.TrustFallible && o.trust != config.TrustHostile {
		return o, errors.New("--trust must be fallible or hostile")
	}
	if only != "" {
		for _, id := range strings.Split(only, ",") {
			if id = strings.TrimSpace(id); id != "" {
				o.only = append(o.only, id)
			}
		}
	}
	if o.out == "" {
		o.out = filepath.Join("/tmp", fmt.Sprintf("caboose-conformance-%s-%s-%s.txt", o.isolation, o.trust, now.UTC().Format("20060102T150405Z")))
	}
	if !filepath.IsAbs(o.out) || filepath.Clean(o.out) != o.out {
		return o, fmt.Errorf("--out %q must be a clean absolute path", o.out)
	}
	if _, err := os.Lstat(o.out); err == nil {
		return o, fmt.Errorf("%s exists: the results never replace a file", o.out)
	}
	if o.probeDir != "" {
		if fi, err := os.Stat(o.probeDir); err != nil || !fi.IsDir() || !filepath.IsAbs(o.probeDir) {
			return o, fmt.Errorf("--probe-dir %q is not an absolute directory", o.probeDir)
		}
	}
	return o, nil
}

// confChecks are the checks a run under isolation and trust has: under
// trust = "hostile" with any isolation but vm the launcher refuses to make
// the sandbox, so T1 is the run; otherwise every group's, groups in the
// order they run (the runner runs every destructive check after all the
// others, by group in this order), the file checks last since their last
// destructive ones unmount and wipe.
func confChecks(isolation, trust string) []conformance.Check {
	if trust == config.TrustHostile && isolation != config.KindVM {
		return checks.ForRun(isolation, trust)
	}
	var all []conformance.Check
	all = append(all, checks.ForRun(isolation, trust)...)
	all = append(all, checks.CheckpointChecks()...)
	all = append(all, checks.EgressChecks()...)
	all = append(all, checks.DaemonForRun(isolation, trust)...)
	all = append(all, checks.All()...)
	var out []conformance.Check
	seen := map[string]bool{}
	for _, c := range all {
		if !seen[c.ID] && c.Applies(isolation, trust) {
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	return out
}

// confEvery is every check of the suite, each once, for list and
// verify-doc.
func confEvery() []conformance.Check { return checks.Every() }

func confList(w io.Writer) int {
	for _, c := range confEvery() {
		where := "all isolations"
		if len(c.Isolations) > 0 {
			where = strings.Join(c.Isolations, ",")
		}
		trust := "any trust"
		if len(c.Trusts) > 0 {
			trust = strings.Join(c.Trusts, ",")
		}
		fmt.Fprintf(w, "%-4s %s\n     control: %s; area: %s; %s; %s; %s\n", c.ID, c.Title, c.Control, c.Step, c.Class, where, trust)
	}
	return 0
}

// confSelect applies --only and says what is left out and why.
func confSelect(all []conformance.Check, o confOptions) (run []conformance.Check, skipped []string, err error) {
	if len(o.only) > 0 {
		have := map[string]bool{}
		for _, c := range all {
			have[c.ID] = true
		}
		for _, id := range o.only {
			if !have[id] {
				return nil, nil, fmt.Errorf("--only: no check %s in a run under %s, %s", id, o.isolation, o.trust)
			}
		}
	}
	for _, c := range all {
		switch {
		case len(o.only) > 0 && !slices.Contains(o.only, c.ID):
			skipped = append(skipped, c.ID+" (not in --only)")
		default:
			if c.Wipe && o.skipWipe {
				skipped = append(skipped, c.ID+" (--skip-wipe)")
			}
			run = append(run, c)
		}
	}
	return run, skipped, nil
}

func confRunCmd(args []string, cio ConformanceIO) int {
	now := time.Now
	if cio.Now != nil {
		now = cio.Now
	}
	if err := conformance.Preflight(cio.Facts); err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return conformance.ExitRefused
	}
	o, err := parseConfRun(args, cio.Stderr, now())
	if err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n%s\n", err, conformanceUsage)
		return conformance.ExitRefused
	}
	run, skipped, err := confSelect(cio.Checks(o.isolation, o.trust), o)
	if err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return conformance.ExitRefused
	}
	confPlan(cio.Stdout, o, run, skipped)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var teardownStarted chan struct{}
	var current atomic.Pointer[conformance.Guard]
	var g *conformance.Guard
	var drv confDriver
	if cio.Signals != nil {
		ch, stop := cio.Signals()
		defer stop()
		teardownStarted = make(chan struct{})
		done := make(chan struct{})
		defer close(done)
		go confInterrupts(ch, done, teardownStarted, cancel, func() {
			confSayLeft(cio.Stderr, current.Load())
			if cio.Exit != nil {
				cio.Exit(130)
			}
		})
	}

	g, err = conformance.Setup(cio.Facts, o.isolation, o.trust)
	if err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return conformance.ExitRefused
	}
	current.Store(g)
	fmt.Fprintf(cio.Stdout, "environment %s in %s\n", g.Env(), g.Base())
	environ, err := confScrub(g, cio.Environ, cio.SetEnv)
	if err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return confTeardown(cio, g, nil, o, conformance.ExitRefused)
	}
	g.Attach(conformance.Inputs{LaunchEnv: environ})
	drv = cio.Driver(g, environ, o.probeDir)

	results, col, runErr := confExecute(ctx, g, drv, environ, run, o)
	code := confExitCode(results, runErr)
	if runErr != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", runErr)
	}
	engine := "unknown"
	if drv != nil {
		engine = drv.Engine()
	}
	goos := cio.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	osVersion := ""
	if cio.OSVersion != nil {
		osVersion = cio.OSVersion()
	}
	rep := conformance.BuildReport(conformance.Header{
		SuiteVersion: conformance.SuiteVersion, CabooseVersion: version.Get().Version,
		OS: goos, Arch: runtime.GOARCH, OSVersion: osVersion, Engine: engine,
		Isolation: o.isolation, Trust: o.trust, Date: now(),
	}, run, results, col)
	if text, err := rep.Format(g.Base(), cio.Redact); err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		code = max(code, 1)
	} else if err := conformance.WriteResults(o.out, text); err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		code = max(code, 1)
	} else {
		fmt.Fprintf(cio.Stdout, "results: %s\n", o.out)
	}
	fmt.Fprintln(cio.Stdout, confSummary(rep))
	if teardownStarted != nil {
		close(teardownStarted)
	}
	return confTeardown(cio, g, drv, o, code)
}

// confExecute brings the environment up and runs the checks inside the
// envelope. Under trust = "hostile" with any isolation but vm nothing is
// brought up: the launcher must refuse to make the sandbox, which is the
// one check of such a run, and the envelope is VerifyBase's.
func confExecute(ctx context.Context, g *conformance.Guard, drv confDriver, environ []string, run []conformance.Check, o confOptions) ([]conformance.Result, *conformance.Collector, error) {
	if err := drv.Prepare(ctx); err != nil {
		return nil, nil, confSetupFailed(err)
	}
	noSandbox := o.trust == config.TrustHostile && o.isolation != config.KindVM
	if noSandbox {
		env := conformance.NewEnv(confNoSandbox{}, drv.Host(), confClock{}, o.isolation, o.trust)
		r := &conformance.Runner{Env: confBaseEnvelope{g}, Wipe: false}
		return conformance.RunChecks(ctx, r, run, env)
	}
	in, err := drv.Up(ctx)
	if err != nil {
		return nil, nil, confSetupFailed(err)
	}
	in.LaunchEnv = environ
	g.Attach(in)
	if _, err := g.Pin(ctx); err != nil {
		return nil, nil, err
	}
	env := conformance.NewEnv(drv.Sandbox(), drv.Host(), confClock{}, o.isolation, o.trust)
	r := &conformance.Runner{Env: g, Wipe: !o.skipWipe, Restart: drv.Restart}
	return conformance.RunChecks(ctx, r, run, env)
}

// confExitCode is the run's exit status: 2 when the suite refused to
// start or to go on; else 1 when a check failed; else 2 when a check was
// an ERROR or the run ended early; else 0 (PASS, GAP, SKIP and INFO).
func confExitCode(results []conformance.Result, runErr error) int {
	var ref *conformance.RefusalError
	if errors.As(runErr, &ref) {
		return conformance.ExitRefused
	}
	for _, r := range results {
		if r.Status == conformance.Fail {
			return 1
		}
	}
	for _, r := range results {
		if r.Status == conformance.Error {
			return conformance.ExitRefused
		}
	}
	if runErr != nil {
		return conformance.ExitRefused
	}
	return 0
}

// confSetupFailed is a failure to bring the environment up: the suite
// did not start, which is a refusal.
func confSetupFailed(err error) error {
	var ref *conformance.RefusalError
	if errors.As(err, &ref) {
		return err
	}
	return &conformance.RefusalError{Item: "setup", Err: err}
}

// confTeardown removes the environment unless --keep, and returns the
// exit status: a teardown that left anything is 2.
func confTeardown(cio ConformanceIO, g *conformance.Guard, drv confDriver, o confOptions, code int) int {
	container, images := g.ExactNames()
	if o.keep {
		fmt.Fprintf(cio.Stdout, "kept %s and its sandbox; to remove them:\n", g.Base())
		confSayRemoval(cio.Stdout, g, container, images)
		return code
	}
	var rm conformance.Remover = confNothingMade{}
	if drv != nil {
		rm = drv.Remover()
	}
	if err := g.Teardown(context.Background(), rm); err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		confSayRemoval(cio.Stderr, g, container, images)
		return conformance.ExitRefused
	}
	fmt.Fprintf(cio.Stdout, "removed %s, %s and its images\n", g.Base(), container)
	return code
}

// confInterrupts: the first interrupt cancels the run, which takes effect
// after the step in progress (an image build does not watch it), and then
// the teardown runs; another one, or one once the teardown has started, says
// what may be left, and leaves.
func confInterrupts(ch <-chan os.Signal, done, teardown <-chan struct{}, cancel func(), left func()) {
	n := 0
	for {
		select {
		case <-done:
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			n++
			inTeardown := false
			select {
			case <-teardown:
				inTeardown = true
			default:
			}
			if n == 1 && !inTeardown {
				cancel()
				continue
			}
			left()
			return
		}
	}
}

func confSayLeft(w io.Writer, g *conformance.Guard) {
	if g == nil {
		fmt.Fprintln(w, "caboose conformance: interrupted before anything was created")
		return
	}
	container, images := g.ExactNames()
	fmt.Fprintf(w, "caboose conformance: interrupted; what may be left:\n")
	confSayRemoval(w, g, container, images)
}

// confSayRemoval prints the exact commands that remove a run's leftovers,
// for the person to run: the suite never removes them by a pattern.
func confSayRemoval(w io.Writer, g *conformance.Guard, container string, images []string) {
	confSayRunning(w, g.CabooseHome(), g.DataDir(), g.Isolation(), container)
	if g.Isolation() != config.KindVM {
		fmt.Fprintf(w, "  docker rm -f %s\n", container)
		for _, im := range images {
			fmt.Fprintf(w, "  docker image rm %s\n", im)
		}
	}
	fmt.Fprintf(w, "  rm -rf %s\n", g.Base())
}

// confSayRunning says how to stop what may still run from a run's base:
// its throwaway daemon, and under vm its caboose-vmm.
func confSayRunning(w io.Writer, home, dataDir, isolation, container string) {
	for _, dir := range daemon.Dirs(home) {
		fmt.Fprintf(w, "  the suite's daemon, if one runs: kill the pid %s names\n", filepath.Join(dir, daemon.StateFile))
	}
	if isolation == config.KindVM {
		fmt.Fprintf(w, "  the VM %s: stop the caboose-vmm whose pid is in %s\n", container, filepath.Join(dataDir, "vm", container, "vmm.pid"))
	}
}

func confPlan(w io.Writer, o confOptions, run []conformance.Check, skipped []string) {
	fmt.Fprintf(w, "caboose conformance: isolation %s, trust %s\n", o.isolation, o.trust)
	if ordered, err := conformance.RunOrder(run); err == nil {
		run = ordered
	}
	var ids, destructive []string
	for _, c := range run {
		if c.Wipe && o.skipWipe {
			continue
		}
		ids = append(ids, c.ID)
		if c.Destructive() {
			destructive = append(destructive, c.ID)
		}
	}
	fmt.Fprintf(w, "checks, in the order they run: %s\n", strings.Join(ids, " "))
	if len(destructive) > 0 {
		fmt.Fprintf(w, "destructive, after every other check: %s\n", strings.Join(destructive, " "))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(w, "skipped: %s\n", strings.Join(skipped, ", "))
	}
	fmt.Fprintf(w, "results: %s\n", o.out)
	fmt.Fprintln(w, "It creates a throwaway environment of its own (a new directory in /tmp,")
	fmt.Fprintln(w, "a new sandbox and images named after it), runs commands in that sandbox")
	fmt.Fprintln(w, "that try to escape it, including removing files as root, and then removes")
	fmt.Fprintln(w, "that environment. Nothing of yours is touched or read. It takes minutes:")
	fmt.Fprintln(w, "the first start builds the image and installs Claude Code into it.")
	fmt.Fprintln(w, "Expect about 5-15 minutes (vm: 20-60 the first time).")
	fmt.Fprintln(w, "Ctrl-C once stops after the step in progress and tears down; twice leaves.")
}

func confSummary(r conformance.Report) string {
	c := r.Counts()
	return fmt.Sprintf("%d pass, %d fail, %d error, %d gap, %d skip",
		c[conformance.Pass], c[conformance.Fail], c[conformance.Error], c[conformance.Gap], c[conformance.Skip])
}

// confClock is the wall clock.
type confClock struct{}

func (confClock) Now() time.Time { return time.Now() }

// confNoSandbox is the sandbox of a run that makes none.
type confNoSandbox struct{}

func (confNoSandbox) Exec(context.Context, string, []string, []byte) (int, string, string, error) {
	return -1, "", "", errors.New("conformance: this run has no sandbox")
}
func (confNoSandbox) Mounts() []conformance.Mount { return nil }
func (confNoSandbox) Labels() map[string]string   { return nil }
func (confNoSandbox) State() string               { return "absent" }

// confBaseEnvelope is the envelope of a run that makes no sandbox:
// VerifyBase, since there is no sandbox to audit, and no destructive step.
type confBaseEnvelope struct{ g *conformance.Guard }

func (e confBaseEnvelope) Verify(context.Context) error { return e.g.VerifyBase() }
func (e confBaseEnvelope) VerifyDestructive(context.Context) error {
	return &conformance.RefusalError{Item: "runner", Err: errors.New("a run with no sandbox has no destructive step")}
}
func (e confBaseEnvelope) Snapshot() (conformance.Snapshot, error) { return e.g.Snapshot() }
func (e confBaseEnvelope) PostCheck(before conformance.Snapshot, allowed []string) (conformance.Snapshot, error) {
	return e.g.PostCheck(before, allowed)
}

// confLeftovers lists what aborted runs left: base directories in the
// physical /tmp named as the suite names them, and the engine's
// containers and images named after a conformance environment, each with
// the exact command that removes it. It removes nothing.
func confLeftovers(cio ConformanceIO) int {
	if err := conformance.Preflight(cio.Facts); err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return conformance.ExitRefused
	}
	tmp := cio.Facts.TempRoot
	if tmp == "" {
		tmp = "/tmp"
	}
	tmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return 1
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return 1
	}
	n := 0
	ours := map[string]bool{} // the environments a base's marker names
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), conformance.BasePrefix) {
			continue
		}
		base := filepath.Join(tmp, e.Name())
		env := "unknown environment"
		m, known := confReadMarker(base)
		if known {
			env = "environment " + m.Env + ", " + m.Isolation
			ours[m.Env] = true
		}
		fmt.Fprintf(cio.Stdout, "%s (%s)\n", base, env)
		if known {
			home := filepath.Join(base, "home")
			confSayRunning(cio.Stdout, home, filepath.Join(home, "envs", m.Env, "data"), m.Isolation, config.ContainerFor(m.Env))
		}
		fmt.Fprintf(cio.Stdout, "  rm -rf %s\n", base)
		n++
	}
	cli := &docker.CLI{Path: or(cio.DockerPath, "docker")}
	if out, err := cli.Output("ps", "-a", "--format", "{{.Names}}"); err == nil {
		for _, name := range strings.Fields(out) {
			if confLeftoverName.MatchString(name) {
				confSayEngine(cio, ours, "container", name, strings.TrimPrefix(name, "caboose-"), "docker rm -f ")
				n++
			}
		}
	} else {
		fmt.Fprintf(cio.Stdout, "(docker did not answer, so its containers and images are not listed: %v)\n", firstLine(err.Error()))
	}
	if out, err := cli.Output("image", "ls", "--format", "{{.Repository}}:{{.Tag}}"); err == nil {
		for _, im := range strings.Fields(out) {
			if confLeftoverImage.MatchString(im) {
				_, env, _ := strings.Cut(im, ":")
				confSayEngine(cio, ours, "image", im, env, "docker image rm ")
				n++
			}
		}
	}
	if n == 0 {
		fmt.Fprintln(cio.Stdout, "no leftovers of the conformance suite")
	}
	return 0
}

// confSayEngine lists a container or image named after env, a name of
// the suite's shape. Its removal command is printed only when a base's
// marker names env, or when env is no environment of the caller's; a
// name of that shape can be one of theirs (a real environment cf-1a2b3c4d
// has the container caboose-cf-1a2b3c4d), and then only a note is.
func confSayEngine(cio ConformanceIO, ours map[string]bool, kind, name, env, rm string) {
	if !ours[env] {
		switch confUserEnv(cio.Facts.CabooseHome, env) {
		case nil:
		case errConfUserEnv:
			fmt.Fprintf(cio.Stdout, "%s %s\n  not the suite's: %s is an environment of yours (%s), and no run's marker names it\n",
				kind, name, env, filepath.Join(cio.Facts.CabooseHome, "envs", env))
			return
		default:
			fmt.Fprintf(cio.Stdout, "%s %s\n  maybe not the suite's: no run's marker names %s, and whether it is an environment of yours could not be told\n",
				kind, name, env)
			return
		}
	}
	fmt.Fprintf(cio.Stdout, "%s %s\n  %s%s\n", kind, name, rm, name)
}

var errConfUserEnv = errors.New("an environment of the caller's")

// confUserEnv is errConfUserEnv when env is an environment in the
// caller's CABOOSE_HOME (a directory of envs/, as realRoots enumerates
// them; anything there of that name counts), nil when it is not, and
// another error when that cannot be told.
func confUserEnv(cabooseHome, env string) error {
	if cabooseHome == "" {
		return errors.New("no CABOOSE_HOME")
	}
	_, err := os.Lstat(filepath.Join(cabooseHome, "envs", env))
	switch {
	case err == nil:
		return errConfUserEnv
	case errors.Is(err, fs.ErrNotExist):
		return nil
	}
	return err
}

var (
	confLeftoverName  = regexp.MustCompile(`^caboose-` + conformance.LeftoverEnvPattern + `$`)
	confLeftoverImage = regexp.MustCompile(`^caboose(-base)?:` + conformance.LeftoverEnvPattern + `$`)
)

// confReadMarker reads a base's marker, bounded and without following a
// symlink, to say whose it is: it decides nothing.
func confReadMarker(base string) (conformance.Marker, bool) {
	var m conformance.Marker
	b, _, err := nofollow.Dir(base).ReadFileMax(conformance.MarkerFile, 4096)
	if err != nil {
		return m, false
	}
	if err := json.Unmarshal(b, &m); err != nil || !conformance.LeftoverEnvName(m.Env) {
		return m, false
	}
	return m, true
}

// confDocID is a check id as docs/security.md cites it, in backticks.
var confDocID = regexp.MustCompile("`([A-Z][0-9]{1,2})`")

// confVerifyDoc checks that every check id docs/security.md cites is one
// of the suite's, and that every one of the suite's is cited, in the
// prose or by a row of the matrix data file. The matrix's own rule (every
// check is in some row) already refuses an uncited check; the "not cited"
// lines name it for the reader.
func confVerifyDoc(cio ConformanceIO) int {
	if cio.Checkout == "" {
		fmt.Fprintln(cio.Stderr, "caboose conformance: verify-doc needs the caboose checkout, whose docs/security.md it reads")
		return 2
	}
	b, err := os.ReadFile(filepath.Join(cio.Checkout, "docs", "security.md"))
	if err != nil {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return 2
	}
	unknown, uncited := confDocCompare(string(b), confMatrixIDs(cio.Checkout), confEvery())
	problems := confMatrixProblems(cio.Checkout, string(b))
	for _, p := range problems {
		fmt.Fprintf(cio.Stdout, "matrix: %s\n", p)
	}
	for _, id := range uncited {
		fmt.Fprintf(cio.Stdout, "not cited: %s\n", id)
	}
	for _, id := range unknown {
		fmt.Fprintf(cio.Stdout, "no such check: %s\n", id)
	}
	if len(unknown) > 0 || len(problems) > 0 || len(uncited) > 0 {
		return 1
	}
	fmt.Fprintln(cio.Stdout, "every check the document cites is the suite's, and every check is cited")
	return 0
}

// confMatrixIDs are the check ids the matrix data file's rows cite; none
// when the file cannot be read (confMatrixProblems says why).
func confMatrixIDs(checkout string) []string {
	dataPath, _ := confMatrixPaths(checkout)
	b, err := os.ReadFile(dataPath)
	if err != nil {
		return nil
	}
	d, err := matrix.ParseData(b)
	if err != nil {
		return nil
	}
	var ids []string
	for _, c := range d.Controls {
		ids = append(ids, c.Checks...)
	}
	return ids
}

// confDocCompare returns the ids cited that are no check's, and the
// checks neither the prose nor a matrix row (rowIDs) cites.
func confDocCompare(doc string, rowIDs []string, all []conformance.Check) (unknown, uncited []string) {
	have := map[string]bool{}
	for _, c := range all {
		have[c.ID] = true
	}
	cited := map[string]bool{}
	for _, m := range confDocID.FindAllStringSubmatch(doc, -1) {
		cited[m[1]] = true
	}
	for _, id := range rowIDs {
		cited[id] = true
	}
	for id := range cited {
		if !have[id] {
			unknown = append(unknown, id)
		}
	}
	for _, c := range all {
		if !cited[c.ID] {
			uncited = append(uncited, c.ID)
		}
	}
	slices.Sort(unknown)
	return unknown, uncited
}

// confMatrixMax bounds a results file the matrix command reads.
const confMatrixMax = matrix.MaxResultsSize

func confMatrixPaths(checkout string) (data, doc string) {
	return filepath.Join(checkout, "docs", "security-matrix.json"), filepath.Join(checkout, "docs", "security.md")
}

// confMatrixProblems says how the checkout's data file and the table in
// the document are wrong; empty when both are right.
func confMatrixProblems(checkout, doc string) []string {
	dataPath, _ := confMatrixPaths(checkout)
	b, err := os.ReadFile(dataPath)
	if err != nil {
		return []string{err.Error()}
	}
	d, err := matrix.ParseData(b)
	if err != nil {
		return []string{err.Error()}
	}
	want, err := matrix.Render(d, confEvery())
	if err != nil {
		return []string{err.Error()}
	}
	got, err := matrix.ReplaceTable(doc, want)
	if err != nil {
		return []string{err.Error()}
	}
	if got != doc {
		return []string{"the table in docs/security.md differs from the data file's; run caboose conformance matrix"}
	}
	return nil
}

// confReadResults reads one results file: a regular file, size-capped.
func confReadResults(path string) (matrix.Results, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return matrix.Results{}, err
	}
	if !fi.Mode().IsRegular() {
		return matrix.Results{}, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > confMatrixMax {
		return matrix.Results{}, fmt.Errorf("%s is over %d bytes", path, confMatrixMax)
	}
	f, err := os.Open(path)
	if err != nil {
		return matrix.Results{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, confMatrixMax+1))
	if err != nil {
		return matrix.Results{}, err
	}
	r, err := matrix.ParseResults(b)
	if err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// confMatrix merges results files into docs/security-matrix.json and
// rewrites the table in docs/security.md. It writes those two paths only.
func confMatrix(files []string, cio ConformanceIO) int {
	fail := func(err error) int {
		fmt.Fprintf(cio.Stderr, "caboose conformance: %v\n", err)
		return conformance.ExitRefused
	}
	if cio.Checkout == "" {
		return fail(errors.New("matrix needs the caboose checkout, whose docs it rewrites"))
	}
	dataPath, docPath := confMatrixPaths(cio.Checkout)
	for _, p := range []string{dataPath, docPath} {
		if err := confRegular(p); err != nil {
			return fail(err)
		}
	}
	b, err := os.ReadFile(dataPath)
	if err != nil {
		return fail(err)
	}
	data, err := matrix.ParseData(b)
	if err != nil {
		return fail(err)
	}
	suite := confEvery()
	before := data
	for _, f := range files {
		r, err := confReadResults(f)
		if err != nil {
			return fail(err)
		}
		if data, err = matrix.Merge(data, r, suite); err != nil {
			return fail(fmt.Errorf("%s: %w", f, err))
		}
	}
	table, err := matrix.Render(data, suite)
	if err != nil {
		return fail(err)
	}
	doc, err := os.ReadFile(docPath)
	if err != nil {
		return fail(err)
	}
	newDoc, err := matrix.ReplaceTable(string(doc), table)
	if err != nil {
		return fail(err)
	}
	out, err := data.Marshal()
	if err != nil {
		return fail(err)
	}
	for _, p := range []string{dataPath, docPath} {
		if err := confRegular(p); err != nil {
			return fail(err)
		}
	}
	if err := confOverwrite(dataPath, out); err != nil {
		return fail(err)
	}
	if err := confOverwrite(docPath, []byte(newDoc)); err != nil {
		return fail(err)
	}
	changed := matrix.Diff(before, data)
	for _, col := range matrix.Columns {
		fmt.Fprintf(cio.Stdout, "%s: %d results stored\n", col, changed[col])
	}
	return 0
}

// confRegular refuses a path that is not an existing regular file (a
// symbolic link included): the matrix command writes only those.
func confRegular(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

// confOverwrite replaces an existing regular file's contents in place,
// never through a symbolic link, and creates, renames and removes nothing.
func confOverwrite(path string, b []byte) error {
	if err := confRegular(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
