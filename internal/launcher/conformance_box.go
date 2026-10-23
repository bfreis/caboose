package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/vm"
)

// The conformance suite's sandbox, as the checks reach it: every command
// goes to the sandbox by the identity pinned when it was created (the
// container's full ID on the pinned engine; under vm the VM, whose vmm,
// machine.json and boot are pinned), and every destructive one first
// re-pins and compares (Guard.VerifyDestructive), compares the sandbox's
// mountinfo with the audit from the host, and then runs behind the
// sandbox-side gate.

// confExecTimeout bounds one command in the sandbox.
const confExecTimeout = 5 * time.Minute

// confMaxOutput bounds what one command's stdout or stderr is kept of.
const confMaxOutput = 1 << 20

// confBox is the real conformance.Sandbox.
type confBox struct {
	g   *conformance.Guard
	iso string
	// open is the sandbox by its pinned identity.
	open func(p conformance.Pins) (backend.Backend, error)

	mu   sync.Mutex
	last conformance.Inspection
}

var (
	_ conformance.Sandbox      = (*confBox)(nil)
	_ conformance.GatedSandbox = (*confBox)(nil)
)

// pinned is the sandbox by its pinned identity; nothing runs before it
// was pinned.
func (b *confBox) pinned() (backend.Backend, error) {
	p, _, ok := b.g.Pinned()
	if !ok {
		return nil, errors.New("conformance: the sandbox was never pinned")
	}
	return b.open(p)
}

func (b *confBox) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	box, err := b.pinned()
	if err != nil {
		return -1, "", "", err
	}
	return confRun(ctx, box, user, argv, stdin)
}

// ExecDestructive is Exec for a destructive command, behind every check
// the envelope has: the guard's pins compared, the mounts the sandbox
// shows compared with the audit from the host, and the gate in the
// sandbox. A refusal of any runs nothing and is an error.
func (b *confBox) ExecDestructive(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if err := b.g.VerifyDestructive(ctx); err != nil {
		return -1, "", "", err
	}
	spec, err := b.g.GateSpec(conformance.FixtureFor(b.iso))
	if err != nil {
		return -1, "", "", err
	}
	box, err := b.pinned()
	if err != nil {
		return -1, "", "", err
	}
	rc, mi, mierr, err := confRun(ctx, box, user, confReadMountinfo, nil)
	if err != nil || rc != 0 {
		return -1, "", "", &conformance.RefusalError{Item: "gate", Err: fmt.Errorf("reading the sandbox's mountinfo: rc %d, %v%s", rc, err, confStderr(mierr))}
	}
	entries, err := conformance.ParseMountinfo(strings.NewReader(mi))
	if err == nil {
		err = conformance.CompareMountinfo(entries, spec)
	}
	if err != nil {
		return -1, "", "", &conformance.RefusalError{Item: "gate", Err: err}
	}
	script, err := conformance.GateScript(spec)
	if err != nil {
		return -1, "", "", err
	}
	gated, err := conformance.GatedArgv(script, argv)
	if err != nil {
		return -1, "", "", err
	}
	rc, stdout, stderr, err := confRun(ctx, box, user, gated, stdin)
	if err == nil && rc == conformance.GateExit && firstLine(stderr) == conformance.GateRefused {
		return rc, stdout, stderr, &conformance.RefusalError{Item: "gate", Err: errors.New("the sandbox-side gate refused")}
	}
	return rc, stdout, stderr, err
}

// confReadMountinfo prints the sandbox's /proc/self/mountinfo with shell
// builtins alone, so that it needs no tool a base may lack.
var confReadMountinfo = []string{"/bin/sh", "-c", `while IFS= read -r l; do printf '%s\n' "$l"; done < /proc/self/mountinfo`}

func (b *confBox) setInspection(in conformance.Inspection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = in
}

func (b *confBox) Mounts() []conformance.Mount {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]conformance.Mount(nil), b.last.Mounts...)
}

func (b *confBox) Labels() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.last.Labels {
		out[k] = v
	}
	return out
}

func (b *confBox) State() string {
	box, err := b.pinned()
	if err != nil {
		return "unavailable"
	}
	return box.State()
}

// confRun runs argv in box as user, bounded in time and in output.
func confRun(ctx context.Context, box backend.Backend, user string, argv []string, stdin []byte) (int, string, string, error) {
	execUser, err := confExecUser(box, user)
	if err != nil {
		return -1, "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, confExecTimeout)
	defer cancel()
	cmd := box.Command(backend.ExecSpec{Argv: argv, User: execUser, Stdin: stdin != nil})
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb capped
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return -1, "", "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return -1, out.String(), errb.String(), ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, out.String(), errb.String(), nil
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return ee.ExitCode(), out.String(), errb.String(), nil
	}
	return -1, out.String(), errb.String(), err
}

// confExecUser is the suite's user as box's exec takes it: docker takes
// a name, the vm guest's exec port a UID or UID:GID only (as the
// launcher's own execs give it), so under vm root is "0". The agent user
// is never tried under vm, whose sandbox runs as root: the suite runs as
// the sessions' user there, and the agent's UID is not the guest's to say.
func confExecUser(box backend.Backend, user string) (string, error) {
	if user != "agent" && user != "root" {
		return "", fmt.Errorf("conformance: unknown sandbox user %q", user)
	}
	if _, vm := box.(*backend.VM); !vm {
		return user, nil
	}
	if user == "root" {
		return "0", nil
	}
	return "", errors.New("conformance: under vm the sandbox runs as root, and the suite does not run as the agent user")
}

// confStderr is ": " and the first line of a command's stderr, cut
// short, or "" for none: what the sandbox or the exec helper said of a
// command that failed.
func confStderr(stderr string) string {
	l := firstLine(stderr)
	if l == "" {
		return ""
	}
	return ": " + clip(l, 300)
}

// capped keeps the first confMaxOutput bytes written to it.
type capped struct{ b bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := confMaxOutput - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.b.String() }

// What the launcher side reads of the sandbox it created, for the guard.

// dockerInspect is the part of docker inspect's output the audit reads.
type dockerInspect struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Labels map[string]string
	}
	Mounts []struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
	HostConfig struct {
		Privileged                                                       bool
		Binds                                                            []string
		Devices                                                          []struct{ PathOnHost, PathInContainer string }
		CapAdd                                                           []string
		SecurityOpt                                                      []string
		NetworkMode, PidMode, IpcMode, UTSMode, UsernsMode, CgroupnsMode string
	}
}

// parseContainerInspect is the Inspection of docker inspect's output for
// one container. A mount of another type than bind or volume, or a bind
// the mounts do not show, is refused: the audit could not judge it.
func parseContainerInspect(b []byte, isolation string) (conformance.Inspection, error) {
	var list []dockerInspect
	if err := json.Unmarshal(b, &list); err != nil {
		return conformance.Inspection{}, fmt.Errorf("docker inspect: %v", err)
	}
	if len(list) != 1 {
		return conformance.Inspection{}, fmt.Errorf("docker inspect gave %d containers", len(list))
	}
	d := list[0]
	in := conformance.Inspection{
		Isolation:    isolation,
		Name:         strings.TrimPrefix(d.Name, "/"),
		ID:           d.ID,
		Labels:       d.Config.Labels,
		Privileged:   d.HostConfig.Privileged,
		CapAdd:       d.HostConfig.CapAdd,
		SecurityOpt:  d.HostConfig.SecurityOpt,
		NetworkMode:  d.HostConfig.NetworkMode,
		PidMode:      d.HostConfig.PidMode,
		IpcMode:      d.HostConfig.IpcMode,
		UTSMode:      d.HostConfig.UTSMode,
		UsernsMode:   d.HostConfig.UsernsMode,
		CgroupnsMode: d.HostConfig.CgroupnsMode,
	}
	for _, dev := range d.HostConfig.Devices {
		in.Devices = append(in.Devices, dev.PathOnHost+":"+dev.PathInContainer)
	}
	binds := map[string]bool{}
	for _, m := range d.Mounts {
		switch m.Type {
		case "bind":
			src := docker.HostSource(m.Source)
			in.Mounts = append(in.Mounts, conformance.Mount{Source: src, Target: m.Destination, ReadOnly: !m.RW})
			binds[src+"\x00"+m.Destination] = true
		case "volume":
			in.Mounts = append(in.Mounts, conformance.Mount{Volume: m.Name, Target: m.Destination, ReadOnly: !m.RW})
		default:
			return conformance.Inspection{}, fmt.Errorf("the container has a %q mount at %s", m.Type, m.Destination)
		}
	}
	for _, bd := range d.HostConfig.Binds {
		parts := strings.Split(bd, ":")
		if len(parts) < 2 || !binds[docker.HostSource(parts[0])+"\x00"+parts[1]] {
			return conformance.Inspection{}, fmt.Errorf("the container's bind %q is not one of its mounts", bd)
		}
	}
	return in, nil
}

// confHello is what the running link offered the sandbox, from the
// link.json the host's link wrote when it started: the settings its hello
// was made from. A link that is not running, or serves another isolation,
// is no hello.
func confHello(dataDir, isolation string) (*conformance.Hello, error) {
	if held, err := linkRunning(dataDir); err != nil || !held {
		return nil, fmt.Errorf("no link is running for the sandbox (%v)", err)
	}
	b, _, err := nofollow.Dir(dataDir).ReadFileMax(linkStateFile, 64<<10)
	if err != nil {
		return nil, err
	}
	var st linkState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %v", linkStateFile, err)
	}
	if st.Isolation != isolation {
		return nil, fmt.Errorf("the link serves %q, not %q", st.Isolation, isolation)
	}
	return &conformance.Hello{
		// Anything but none is taken as offered.
		SSHAgent: st.SSHAgent != "" && st.SSHAgent != "none",
		HostExec: st.HostExec && st.Trust == config.TrustFallible,
	}, nil
}

// confConfigFacts are what the loaded configuration says of the keys the
// audit pins.
func confConfigFacts(c *config.Config) conformance.ConfigFacts {
	return conformance.ConfigFacts{
		RunArgs:      c.RunArgs,
		EngineSocket: c.EngineSocket,
		HostExec:     c.HostExec,
		SSHAgent:     c.SSHAgent,
		ForwardPorts: c.ForwardPorts,
	}
}

// confInspectContainer is what the engine says the container named name
// was created with, plus the link's hello and the configuration.
func confInspectContainer(a *App, name string) (conformance.Inspection, error) {
	out, err := a.Docker.RawOutput("inspect", "--type=container", name)
	if err != nil {
		return conformance.Inspection{}, err
	}
	in, err := parseContainerInspect([]byte(out), a.Cfg.Isolation)
	if err != nil {
		return in, err
	}
	if in.Hello, err = confHello(a.Cfg.DataDir, a.Cfg.Isolation); err != nil {
		return in, err
	}
	in.Config = confConfigFacts(a.Cfg)
	return in, nil
}

// confPinContainer is the engine's ID and the container's full ID, the
// container asked for by name: one swapped in under the name shows
// another ID.
func confPinContainer(a *App, name string) (conformance.Pins, error) {
	engine, err := a.Docker.Output("info", "--format", "{{.ID}}")
	if err != nil {
		return conformance.Pins{}, fmt.Errorf("docker info: %v", err)
	}
	id, err := a.Docker.Output("inspect", "--type=container", "--format", "{{.Id}}", name)
	if err != nil {
		return conformance.Pins{}, err
	}
	return conformance.Pins{EngineID: strings.TrimSpace(engine), ContainerID: strings.TrimSpace(id)}, nil
}

// confEngineEndpoint is the endpoint docker's current context names, with
// DOCKER_HOST and DOCKER_CONTEXT already scrubbed: what every call of the
// run is then pinned to.
func confEngineEndpoint(cli *docker.CLI) (string, error) {
	h, err := cli.Output("context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return "", fmt.Errorf("docker context inspect: %v", err)
	}
	h = strings.TrimSpace(h)
	if h == "" || strings.ContainsAny(h, " \n\t") {
		return "", fmt.Errorf("docker's context names no endpoint (%q)", h)
	}
	return h, nil
}

// The vm isolation's record of a VM, as the audit reads it.

// confVMRecord is the VM's machine.json and state.json, read without
// following a symlink from the data dir, and machine.json's sha256.
type confVMRecord struct {
	Dir     string
	Machine vm.Machine
	State   vm.State
	Hash    string
}

func confReadVM(dataDir, name string) (confVMRecord, error) {
	rec := confVMRecord{Dir: filepath.Join(dataDir, vmDirName, name)}
	d := nofollow.Dir(dataDir)
	mb, _, err := d.ReadFileMax(path.Join(vmDirName, name, "machine.json"), 1<<20)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(mb, &rec.Machine); err != nil {
		return rec, fmt.Errorf("machine.json: %v", err)
	}
	sum := sha256.Sum256(mb)
	rec.Hash = hex.EncodeToString(sum[:])
	sb, _, err := d.ReadFileMax(path.Join(vmDirName, name, "state.json"), 4<<20)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(sb, &rec.State); err != nil {
		return rec, fmt.Errorf("state.json: %v", err)
	}
	return rec, nil
}

// inspection is what the VM was really made with: its labels and guest
// mounts from the boot spec it was given, each mount's source the host
// path of the share it binds, its shares, disks and network from
// machine.json.
func (r confVMRecord) inspection() (conformance.Inspection, error) {
	in := conformance.Inspection{Isolation: config.KindVM, Labels: r.State.Labels}
	shares := map[string]vm.Share{}
	for _, s := range r.Machine.Shares {
		shares[s.Name] = s
		in.Shares = append(in.Shares, conformance.Share{Name: s.Name, Path: s.Path, ReadOnly: s.ReadOnly})
	}
	for _, gm := range r.State.Boot.Mounts {
		// Every mount binds a path of the one share, ShareTag, whose
		// first element is the directory machine.json names (backend's
		// planShares: "m0/file").
		if gm.Tag != vm.ShareTag {
			return in, fmt.Errorf("the VM binds share %q, not %q", gm.Tag, vm.ShareTag)
		}
		if p := gm.Path; p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return in, fmt.Errorf("the VM binds %q of its share, which is no directory of it", gm.Path)
		}
		name, rest, _ := strings.Cut(gm.Path, "/")
		s, ok := shares[name]
		if !ok {
			return in, fmt.Errorf("the VM binds share directory %q, which machine.json does not have", name)
		}
		src := s.Path
		if rest != "" {
			src = filepath.Join(s.Path, filepath.FromSlash(rest))
		}
		in.Mounts = append(in.Mounts, conformance.Mount{Source: src, Target: gm.Target, ReadOnly: gm.ReadOnly || s.ReadOnly})
	}
	if len(r.State.Boot.Disks) != len(r.State.Volumes) {
		return in, errors.New("the VM's disks and volumes differ in number")
	}
	for i, dk := range r.State.Boot.Disks {
		in.Mounts = append(in.Mounts, conformance.Mount{Volume: r.State.Volumes[i], Target: dk.Target})
	}
	m := &conformance.Machine{Dir: r.Dir, Network: r.Machine.Network}
	m.BootPaths = append(m.BootPaths, r.Machine.Kernel)
	if r.Machine.Initramfs != "" {
		m.BootPaths = append(m.BootPaths, r.Machine.Initramfs)
	}
	for _, dk := range r.Machine.Disks {
		if dk.ReadOnly {
			m.BootPaths = append(m.BootPaths, dk.Path)
		} else {
			m.Paths = append(m.Paths, dk.Path)
		}
	}
	if r.Machine.Console != "" {
		m.Paths = append(m.Paths, r.Machine.Console)
	}
	in.Machine = m
	return in, nil
}

// confVMMStart is the start time of the process vmm.pid names, and that
// it is a caboose-vmm: PID reuse cannot pass for the same vmm.
func confVMMStart(dataDir, name string, ps func(args ...string) (string, error)) (string, error) {
	b, _, err := nofollow.Dir(dataDir).ReadFileMax(path.Join(vmDirName, name, "vmm.pid"), 64)
	if err != nil {
		return "", err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return "", fmt.Errorf("vmm.pid holds %q", strings.TrimSpace(string(b)))
	}
	comm, err := ps("-o", "comm=", "-p", strconv.Itoa(pid))
	if err != nil {
		return "", fmt.Errorf("ps: %v", err)
	}
	if !strings.Contains(path.Base(strings.TrimSpace(comm)), "caboose-vmm") {
		return "", fmt.Errorf("pid %d is %q, not the vmm", pid, strings.TrimSpace(comm))
	}
	start, err := ps("-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return "", fmt.Errorf("ps: %v", err)
	}
	start = strings.Join(strings.Fields(start), " ")
	if start == "" {
		return "", fmt.Errorf("no start time for pid %d", pid)
	}
	return strconv.Itoa(pid) + "@" + start, nil
}

// hostPS is ps on this machine.
func hostPS(args ...string) (string, error) {
	out, err := exec.Command("ps", args...).Output()
	return string(out), err
}

// confBootID is the guest's boot_id, read with shell builtins.
func confBootID(ctx context.Context, box backend.Backend) (string, error) {
	rc, out, stderr, err := confRun(ctx, box, "root", []string{"/bin/sh", "-c", `IFS= read -r l < /proc/sys/kernel/random/boot_id && printf '%s' "$l"`}, nil)
	if err != nil || rc != 0 {
		return "", fmt.Errorf("reading the guest's boot_id: rc %d, %v%s", rc, err, confStderr(stderr))
	}
	return strings.TrimSpace(out), nil
}

// confRemover removes the throwaway environment's sandbox and images, by
// the exact names it is given, which must be the environment's own.
type confRemover struct {
	a *App
	// stopLink stops the environment's link helper, before anything of B
	// goes.
	stopLink func() error
}

func (r *confRemover) RemoveContainer(_ context.Context, name string) error {
	if name != r.a.Cfg.Container {
		return fmt.Errorf("conformance: %s is not the environment's sandbox", name)
	}
	if r.stopLink != nil {
		if err := r.stopLink(); err != nil {
			return fmt.Errorf("stopping the link: %w", err)
		}
	}
	if r.a.isVM() {
		v := r.a.vmBox()
		if v.Name != name || v.State() == "absent" {
			return nil
		}
		return v.Remove()
	}
	if r.a.Docker.Quiet("inspect", "--type=container", name) != nil {
		return nil
	}
	_, err := r.a.Docker.Output("rm", "-f", name)
	return err
}

func (r *confRemover) RemoveImage(_ context.Context, name string) error {
	if name != config.ImageFor(r.a.Cfg.Env) && name != config.BaseImageFor(r.a.Cfg.Env) {
		return fmt.Errorf("conformance: %s is not one of the environment's images", name)
	}
	if r.a.isVM() {
		// A VM's images are root disks in CABOOSE_HOME/vm/images, under B,
		// which goes with it.
		return nil
	}
	if r.a.Docker.Quiet("image", "inspect", name) != nil {
		return nil
	}
	_, err := r.a.Docker.Output("image", "rm", name)
	return err
}
