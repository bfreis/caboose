package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/docker"
)

// confDriver is what a run drives: the real one creates the throwaway
// environment with this launcher, in this process; the tests' is a fake.
type confDriver interface {
	// Prepare writes the environment's config.toml and the canaries made
	// before the launch, and builds the App, creating no sandbox.
	Prepare(ctx context.Context) error
	// Up builds the image, creates and starts the sandbox and its link,
	// makes the canaries that go after the launch, and returns the
	// guard's inputs (without LaunchEnv).
	Up(ctx context.Context) (conformance.Inputs, error)
	Sandbox() conformance.Sandbox
	Host() conformance.Host
	// Restart restarts the sandbox and re-pins it, for a Fresh check.
	Restart(ctx context.Context) error
	// Remover removes what the run created, by its exact names.
	Remover() conformance.Remover
	// Engine is the engine and its version, for the results file.
	Engine() string
}

// confLinkWait bounds the wait for the link to come up.
const confLinkWait = 60 * time.Second

// confReal is the real driver.
type confReal struct {
	g              *conformance.Guard
	environ        []string
	checkout       string
	probeDir       string
	stdout, stderr io.Writer

	a     *App
	box   *confBox
	host  *confHost
	after []checks.Spec
	spec  *backend.Spec
}

func newConfReal(g *conformance.Guard, environ []string, checkout, probeDir string, stdout, stderr io.Writer) *confReal {
	return &confReal{g: g, environ: environ, checkout: checkout, probeDir: probeDir, stdout: stdout, stderr: stderr}
}

// Prepare lays out B before anything of the launcher's runs
// (confLayOut), then builds the App, the suite's daemon and the sandbox
// by its pins, and opens the checkpoint store with the product's own code,
// with its canary (confOpenStore).
func (r *confReal) Prepare(ctx context.Context) error {
	h, store, after, err := confLayOut(r.g, r.probeDir, runtime.GOARCH)
	if err != nil {
		return err
	}
	r.host, r.after = h, after
	engine := ""
	if r.g.Isolation() != config.KindVM {
		if engine, err = confEngineEndpoint(&docker.CLI{Path: "docker"}); err != nil {
			return err
		}
	}
	a, err := confApp(r.g, r.environ, engine, r.checkout, r.stdout, r.stderr)
	if err != nil {
		return err
	}
	a.creating = func(s backend.Spec) { r.spec = &s }
	r.a = a
	r.box = &confBox{g: r.g, iso: r.g.Isolation(), open: r.open}
	h.a, h.box = a, r.box
	h.daemon = newConfDaemon(r.g, r.environ, engine)
	a.Spawn = h.daemon.spawn
	return confOpenStore(h, store)
}

// confLayOut lays out B: the stubs and the alias scripts, the probe, the
// canaries made before the launch and the git checks' repositories, then
// config.toml, which names them. It returns the Host, without its App,
// and the canaries made later: the checkpoint store's and those after the
// launch.
func confLayOut(g *conformance.Guard, probeDir, goarch string) (h *confHost, store, after []checks.Spec, err error) {
	settings := confSettingsFor(g)
	if len(settings.Aliases) == 0 {
		return nil, nil, nil, fmt.Errorf("conformance: %s is not a path the suite's aliases may use", g.Base())
	}
	before, store, after, err := confLayout(g)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, d := range confLayoutDirs(g) {
		if err := g.MakeDir(d); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := g.WriteStubs(); err != nil {
		return nil, nil, nil, err
	}
	if err := writeConfScripts(g, settings.Aliases); err != nil {
		return nil, nil, nil, err
	}
	if err := placeConfProbe(g, probeDir, goarch); err != nil {
		return nil, nil, nil, err
	}
	h = newConfHost(g, nil)
	if err := makeCanaries(h, before); err != nil {
		return nil, nil, nil, err
	}
	if err := writeConfConfig(g, settings); err != nil {
		return nil, nil, nil, err
	}
	return h, store, after, nil
}

// confOpenStore opens the environment's checkpoint store with the
// product's own code (checkpoint.Open makes it), then makes its canaries.
func confOpenStore(h *confHost, store []checks.Spec) error {
	s, err := checkpoint.Open(h.a.Cfg.CabooseHome, h.a.Cfg.EnvDir)
	if err != nil {
		return fmt.Errorf("conformance: the checkpoint store: %w", err)
	}
	s.Close()
	return makeCanaries(h, store)
}

// open is the sandbox by its pinned identity: the container by its full
// ID on the pinned engine; the VM by its directory under B, whose vmm,
// machine.json and boot the pins hold.
func (r *confReal) open(p conformance.Pins) (backend.Backend, error) {
	if r.a.isVM() {
		return r.a.vmBox(), nil
	}
	return backend.NewDockerByID(r.a.Docker, p.ContainerID)
}

func (r *confReal) Up(ctx context.Context) (conformance.Inputs, error) {
	if err := r.g.VerifyBase(); err != nil {
		return conformance.Inputs{}, err
	}
	if st := r.a.state(); st != "absent" {
		return conformance.Inputs{}, fmt.Errorf("conformance: %s exists already (%s): the suite only runs against a sandbox it creates", r.a.Cfg.Container, st)
	}
	if err := r.a.ensureRunning(true); err != nil {
		return conformance.Inputs{}, err
	}
	if err := ctx.Err(); err != nil {
		return conformance.Inputs{}, err
	}
	if err := r.link(ctx); err != nil {
		return conformance.Inputs{}, err
	}
	if err := makeCanaries(r.host, r.after); err != nil {
		return conformance.Inputs{}, err
	}
	// The daemon checks' canaries, one of them in the suite's daemon's
	// directory, which the daemon has made by now.
	daemonCanaries, err := confDaemonCanaries(r.g, r.a.daemonDir())
	if err != nil {
		return conformance.Inputs{}, err
	}
	if err := makeCanaries(r.host, daemonCanaries); err != nil {
		return conformance.Inputs{}, err
	}
	in := conformance.Inputs{Inspect: r.inspect, Pin: r.pin}
	if r.a.isVM() {
		if r.spec == nil {
			return in, errors.New("conformance: the VM's spec was not recorded")
		}
		shares, err := backend.PlanShares(r.spec.Mounts)
		if err != nil {
			return in, err
		}
		for _, s := range shares {
			in.PlannedShares = append(in.PlannedShares, conformance.Share{Name: s.Name, Path: s.Path, ReadOnly: s.ReadOnly})
		}
		in.Volumes = []string{dockerVolume}
		files, err := r.a.findVMFiles()
		if err != nil {
			return in, err
		}
		in.BootInputs = []string{files.Kernel, files.Builder}
	}
	return in, nil
}

// link has the environment's link started by the daemon under B/home
// (the throwaway CABOOSE_HOME, so a daemon of its own, which the suite
// spawns with its stubs first on its PATH), waits for it to say what its
// hello offers, and confirms the daemon serving it is the one the suite
// spawned.
func (r *confReal) link(ctx context.Context) error {
	r.a.startLink()
	deadline := time.Now().Add(confLinkWait)
	for {
		_, err := confHello(r.a.Cfg.DataDir, r.a.Cfg.Isolation)
		if err == nil {
			return r.host.daemon.check(ctx, r.a)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("conformance: the link did not come up: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (r *confReal) inspect(ctx context.Context) (conformance.Inspection, error) {
	var in conformance.Inspection
	var err error
	if r.a.isVM() {
		var rec confVMRecord
		if rec, err = confReadVM(r.a.Cfg.DataDir, r.a.Cfg.Container); err != nil {
			return in, err
		}
		if in, err = rec.inspection(); err != nil {
			return in, err
		}
		if in.Hello, err = confHello(r.a.Cfg.DataDir, r.a.Cfg.Isolation); err != nil {
			return in, err
		}
		in.Config = confConfigFacts(r.a.Cfg)
	} else if in, err = confInspectContainer(r.a, r.a.Cfg.Container); err != nil {
		return in, err
	}
	r.box.setInspection(in)
	return in, nil
}

func (r *confReal) pin(ctx context.Context) (conformance.Pins, error) {
	if !r.a.isVM() {
		return confPinContainer(r.a, r.a.Cfg.Container)
	}
	rec, err := confReadVM(r.a.Cfg.DataDir, r.a.Cfg.Container)
	if err != nil {
		return conformance.Pins{}, err
	}
	start, err := confVMMStart(r.a.Cfg.DataDir, r.a.Cfg.Container, hostPS)
	if err != nil {
		return conformance.Pins{}, err
	}
	boot, err := confBootID(ctx, r.a.vmBox())
	if err != nil {
		return conformance.Pins{}, err
	}
	return conformance.Pins{VMMStart: start, MachineHash: rec.Hash, BootID: boot}, nil
}

func (r *confReal) Sandbox() conformance.Sandbox { return r.box }
func (r *confReal) Host() conformance.Host {
	if r.host == nil {
		return nil
	}
	return r.host
}

// Restart stops and starts the sandbox by its pinned identity, waits for
// it and its link, and pins it again: a restarted guest's mounts are the
// ones it was created with, whatever a check did to them.
func (r *confReal) Restart(ctx context.Context) error {
	box, err := r.box.pinned()
	if err != nil {
		return err
	}
	if err := box.Stop(); err != nil {
		return err
	}
	if err := box.Start(); err != nil {
		return err
	}
	if err := r.a.waitUntilReady(); err != nil {
		return err
	}
	if err := r.link(ctx); err != nil {
		return err
	}
	_, err = r.g.Pin(ctx)
	return err
}

func (r *confReal) Remover() conformance.Remover {
	if r.a == nil {
		// No App, so no launcher ran: nothing of the engine's was made.
		return confNothingMade{}
	}
	a, g := r.a, r.g
	return &confRemover{a: a, stopLink: func() error { return confStopDaemon(g, a) }}
}

func (r *confReal) Engine() string {
	if r.a == nil {
		return "unknown"
	}
	if r.a.isVM() {
		return "Virtualization.framework (caboose-vmm)"
	}
	v, err := r.a.Docker.Output("version", "--format", "{{.Server.Version}}")
	if err != nil || v == "" {
		return "docker (version unknown)"
	}
	return "docker " + v
}

// confNothingMade is the remover of a run that never got as far as a
// launcher: there is nothing of the engine's to remove.
type confNothingMade struct{}

func (confNothingMade) RemoveContainer(context.Context, string) error { return nil }
func (confNothingMade) RemoveImage(context.Context, string) error     { return nil }
