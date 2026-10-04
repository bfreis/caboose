// Package builder builds the sandbox's image under the vm isolation, where
// the host may have no Docker engine: in a builder guest of its own, which
// boots vm/builder's disk read-only with caboose's kernel and init and runs
// dockerd (spike 4). The host drives it through the exec port with the
// docker commands caboose runs today: the contexts go in as tars on stdin,
// the image check is imagecheck.Args, and what comes out is the layer as a
// root disk (mkfs.ext4 -d, owners kept) and the image's config, which the
// guest that boots that disk cannot read.
package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/vm"
)

// The builder guest's side (vm/builder/caboose-builder) and its disks, in
// the order vmm attaches them after the builder's own root.
const (
	script    = "caboose-builder" // on the guest's PATH
	entry     = "/usr/local/bin/" + script
	readyFile = "/run/caboose-builder.ready"
)

// Disk sizes: sparse files, so only what is written takes space. The
// default root is 1.2 GB (spike 4); its cache grows with every base.
const (
	cacheSize = 128 << 30
	diskSize  = 64 << 30
)

// How long each step may take: a cold base build installs toolchains.
var (
	readyTimeout = 3 * time.Minute
	stepTimeout  = 15 * time.Minute
	baseTimeout  = 60 * time.Minute
)

// Builder is the builder guest of one environment.
type Builder struct {
	// Dir is its VM's directory, the data dir's vm/builder: vm.sock, the
	// logs, and its cache disk, kept between builds.
	Dir vm.Dir
	// Kernel, Initramfs and Image are what it boots: caboose's kernel and
	// init, and vm/builder's disk.
	Kernel, Initramfs, Image string
	CPUs, MemoryMiB          int
	// StartVMM starts caboose-vmm, detached, for a VM's directory.
	StartVMM func(vm.Dir) error
	// Self is the executable Command runs as the exec helper: this
	// launcher.
	Self string
	// Stderr gets docker's progress and what the builder says.
	Stderr io.Writer
	// Egress, when not "", is where the builder's agent is to serve the
	// outbound proxy (agentproto.EgressListen), and Link opens the host
	// link that serves it, for the guest's life: under vm with
	// egress_proxy on, the build's downloads are made from the host, as
	// the sandbox's are (egressArgs).
	Egress string
	Link   func(vm.Dir) (io.Closer, error)
}

// Cache is the builder's cache disk: dockerd's storage.
func (b *Builder) Cache() string { return string(b.Dir) + "/cache.img" }

// Guest is a running builder guest.
type Guest struct {
	b    *Builder
	spec agentproto.BootSpec
	// egress is where the outbound proxy serves, "" for none; link is
	// the host link serving it, closed by Stop.
	egress string
	link   io.Closer
	// run, when set, stands in for the guest's exec port (tests): stderr
	// is where the command's stderr is shown, nil when it is not.
	run func(timeout time.Duration, in io.Reader, out, stderr io.Writer, argv ...string) error
}

// Start boots the builder guest with out attached as the disk a root is
// written to, created anew, and template, when not "", as the disk an
// empty ext4 goes to (Template). A builder left running by an earlier
// build is stopped first: it has other disks.
func (b *Builder) Start(out, template string) (*Guest, error) {
	if err := os.MkdirAll(string(b.Dir), 0o700); err != nil {
		return nil, err
	}
	if b.Dir.Serving() {
		if err := b.Dir.Stop(); err != nil {
			return nil, fmt.Errorf("stopping the builder left from before: %w", err)
		}
	}
	if _, err := os.Stat(b.Cache()); errors.Is(err, os.ErrNotExist) {
		if err := sparse(b.Cache(), cacheSize); err != nil {
			return nil, err
		}
	}
	disks := []vm.Disk{{Path: b.Image, ReadOnly: true}, {Path: b.Cache()}}
	for _, d := range []string{out, template} {
		if d == "" {
			continue
		}
		_ = os.Remove(d)
		if err := sparse(d, diskSize); err != nil {
			return nil, err
		}
		disks = append(disks, vm.Disk{Path: d})
	}
	m := vm.Machine{
		Kernel: b.Kernel, Initramfs: b.Initramfs,
		Cmdline: vm.DefaultCmdline("nat") + " " + agentproto.ScratchTmpfs,
		CPUs:    b.CPUs, MemoryMiB: b.MemoryMiB,
		Disks: disks, Network: "nat", Console: b.Dir.Console(),
	}
	spec := bootSpec(b.Egress)
	if err := b.Dir.Boot(m, spec, b.StartVMM); err != nil {
		return nil, err
	}
	g := &Guest{b: b, spec: spec}
	if err := b.Dir.WaitReady(spec, readyTimeout); err != nil {
		_ = g.Stop()
		return nil, fmt.Errorf("the builder guest did not come up: %w", err)
	}
	if err := g.linkEgress(); err != nil {
		_ = g.Stop()
		return nil, err
	}
	return g, nil
}

// proxyWait bounds the wait for the builder's outbound proxy once its link
// is open: the agent serves it as soon as the host's hello reaches it.
const proxyWait = 30

// linkEgress opens the host link that serves the builder's outbound proxy,
// when it has one, and waits until the proxy serves: the build's first
// step may be a download.
func (g *Guest) linkEgress() error {
	if g.b.Egress == "" {
		return nil
	}
	if g.b.Link == nil {
		return errors.New("the builder's outbound proxy needs a host link")
	}
	l, err := g.b.Link(g.b.Dir)
	if err != nil {
		return fmt.Errorf("linking the builder guest, for its outbound proxy: %w", err)
	}
	g.link, g.egress = l, g.b.Egress
	if err := g.runQuiet(time.Minute, nil, nil, agentproto.GuestAgent, "wait-proxy", strconv.Itoa(proxyWait)); err != nil {
		return fmt.Errorf("the builder's outbound proxy did not come up: %w", err)
	}
	return nil
}

// bootSpec is the builder guest's boot: caboose-builder serve, as root,
// ready once dockerd is up and the resolver has answered. The builder's
// first steps look names up -- the probe's curl in a user's image, whose
// libc may not be the builder's (Alpine's musl), so resolv.conf's options
// may have it wait 5 s before it retries the query vmnet loses after a
// boot, past the probe's connect timeout; dockerd's pulls, which read them
// as glibc does -- so it waits for the warm-up, which dockerd's start
// mostly covers, rather than have them race the resolver waking.
//
// With egress, the outbound proxy where the agent will serve it, the
// environment names it: dockerd's pulls go through it, and the docker CLI
// has no use for it (its daemon is a Unix socket).
func bootSpec(egress string) agentproto.BootSpec {
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	if egress != "" {
		env = append(env, agentproto.EgressEnv(egress)...)
	}
	return agentproto.BootSpec{
		Hostname:     "caboose-builder",
		Env:          env,
		User:         "0:0",
		Cmd:          []string{entry, "serve", readyFile},
		Ready:        readyFile,
		WaitResolver: true,
	}
}

// sparse makes path a file of size bytes that takes no space yet.
func sparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Stop shuts the builder guest down, which ends whatever still runs in it,
// and its link first.
func (g *Guest) Stop() error {
	if g.link != nil {
		_ = g.link.Close()
		g.link = nil
	}
	return g.b.Dir.Stop()
}

// Run runs argv in the guest, stdin from in (nil for none) and stdout to
// out, within timeout; its stderr goes to the Builder's, and its last
// lines into the error when it fails.
func (g *Guest) Run(timeout time.Duration, in io.Reader, out io.Writer, argv ...string) error {
	var stderr io.Writer
	if g.b != nil {
		stderr = g.b.Stderr
	}
	return g.exec(timeout, in, out, stderr, argv...)
}

// runQuiet is Run with nothing of stderr shown: for a command whose failure
// is an answer (an image not in the store), or whose error is all a caller
// shows of it, which then says it once.
func (g *Guest) runQuiet(timeout time.Duration, in io.Reader, out io.Writer, argv ...string) error {
	return g.exec(timeout, in, out, nil, argv...)
}

// exec runs argv in the guest with its stderr shown on stderr (nil for
// nowhere) and kept, the last of it, for the error.
func (g *Guest) exec(timeout time.Duration, in io.Reader, out, stderr io.Writer, argv ...string) error {
	if g.run != nil {
		return g.run(timeout, in, out, stderr, argv...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := vm.ExecCommand(g.b.Self, g.b.Dir.Socket(), agentproto.ExecRequest{Argv: argv, Stdin: in != nil})
	if cmd.Err != nil {
		return cmd.Err
	}
	tail := &tailWriter{}
	cmd.Stdin, cmd.Stdout = in, out
	cmd.Stderr = io.MultiWriter(tail, orDiscard(stderr))
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return &StepError{Argv: argv, Err: err, Said: tail.String()}
		}
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return &StepError{Argv: argv, Err: fmt.Errorf("no end after %v", timeout), Said: tail.String()}
	}
}

// Output is runQuiet with stdout returned: what it said on stderr is in
// its error, not on the terminal.
func (g *Guest) Output(timeout time.Duration, argv ...string) (string, error) {
	var b bytes.Buffer
	err := g.runQuiet(timeout, nil, &b, argv...)
	return b.String(), err
}

// StepError is a command in the builder that failed.
type StepError struct {
	Argv []string
	Err  error
	Said string // the last of its stderr
}

func (e *StepError) Error() string {
	name := strings.Join(e.Argv[:min(len(e.Argv), 3)], " ")
	if e.Said == "" {
		return fmt.Sprintf("%s: %v", name, e.Err)
	}
	return fmt.Sprintf("%s: %v; it said:\n%s", name, e.Err, e.Said)
}

func (e *StepError) Unwrap() error { return e.Err }

// ExitCode is the command's status, or -1 when it has none.
func (e *StepError) ExitCode() int {
	var ee *exec.ExitError
	if errors.As(e.Err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// tailWriter keeps the last lines written to it.
type tailWriter struct{ b []byte }

const tailBytes = 4096

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*tailBytes {
		t.b = append([]byte(nil), t.b[len(t.b)-tailBytes:]...)
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	s := string(t.b)
	if len(s) > tailBytes {
		s = s[len(s)-tailBytes:]
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-20):], "\n")
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// Config is what the guest that boots an image needs of its config, which
// only docker can read.
type Config struct {
	User       string
	Env        []string
	Entrypoint []string
	Cmd        []string
	WorkingDir string
}

// Inspect is image's ID and config.
func (g *Guest) Inspect(image string) (string, Config, error) {
	out, err := g.Output(stepTimeout, "docker", "image", "inspect", "--format", "{{json .Id}} {{json .Config}}", image)
	if err != nil {
		return "", Config{}, err
	}
	id, cfg, ok := strings.Cut(strings.TrimSpace(out), " ")
	var c Config
	var s string
	if !ok || json.Unmarshal([]byte(id), &s) != nil || json.Unmarshal([]byte(cfg), &c) != nil {
		return "", Config{}, fmt.Errorf("docker image inspect %s said %q", image, out)
	}
	return s, c, nil
}

// Template writes an empty ext4 onto the template disk Start attached.
func (g *Guest) Template() error {
	return g.Run(stepTimeout, nil, nil, script, "template")
}
