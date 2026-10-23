package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/vm"
)

const confDecoyID = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// confEngine is a fake engine with two sandboxes: the suite's, and a
// decoy named as a user's own would be.
type confEngine struct {
	suite, decoy *backendtest.Fake
	byID         map[string]*backendtest.Fake
	byName       map[string]*backendtest.Fake
	mountinfo    string
	gate         func() *exec.Cmd // the gated command's answer
}

func newConfEngine(g *conformance.Guard, in conformance.Inspection) *confEngine {
	e := &confEngine{suite: &backendtest.Fake{Status: "running"}, decoy: &backendtest.Fake{Status: "running"}}
	e.byID = map[string]*backendtest.Fake{confTestID: e.suite, confDecoyID: e.decoy}
	e.byName = map[string]*backendtest.Fake{config.ContainerFor(g.Env()): e.suite, "caboose-default": e.decoy}
	var b strings.Builder
	b.WriteString("1 0 0:1 / / rw,relatime - overlay overlay rw\n2 1 0:2 / /proc rw - proc proc rw\n")
	for i, m := range in.Mounts {
		opt := "rw"
		if m.ReadOnly {
			opt = "ro"
		}
		fmt.Fprintf(&b, "%d 1 0:9 /x %s %s,relatime - virtiofs mac rw\n", 10+i, m.Target, opt)
	}
	e.mountinfo = b.String()
	e.gate = func() *exec.Cmd { return backendtest.Reply("\nrc=0\n", "", 0) }
	for _, f := range []*backendtest.Fake{e.suite, e.decoy} {
		f.Exec = func(s backend.ExecSpec) *exec.Cmd {
			switch {
			case slices.Equal(s.Argv, confReadMountinfo):
				return backendtest.Reply(e.mountinfo, "", 0)
			case len(s.Argv) > 3 && s.Argv[3] == "conformance-gate":
				return e.gate()
			}
			return backendtest.Reply("\nrc=0\n", "", 0)
		}
	}
	return e
}

// open is the sandbox by its pinned ID: never by a name.
func (e *confEngine) open(p conformance.Pins) (backend.Backend, error) {
	f, ok := e.byID[p.ContainerID]
	if !ok {
		return nil, fmt.Errorf("no container %s", p.ContainerID)
	}
	return f, nil
}

// confPinned is a guard of a container run, attached and pinned, with
// the fake engine and the real Sandbox over it.
type confPinned struct {
	g    *conformance.Guard
	in   conformance.Inspection
	pins conformance.Pins
	e    *confEngine
	box  *confBox
}

func newConfPinned(t *testing.T) *confPinned {
	t.Helper()
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	p := &confPinned{g: g, in: confInspection(t, g), pins: conformance.Pins{EngineID: "E", ContainerID: confTestID}}
	p.e = newConfEngine(g, p.in)
	g.Attach(conformance.Inputs{
		LaunchEnv: g.LaunchEnv([]string{"PATH=/usr/bin"}),
		Inspect:   func(context.Context) (conformance.Inspection, error) { return p.in, nil },
		Pin:       func(context.Context) (conformance.Pins, error) { return p.pins, nil },
	})
	if _, err := g.Pin(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.box = &confBox{g: g, iso: config.KindContainer, open: p.e.open}
	return p
}

func gatedRuns(f *backendtest.Fake) (gated, plain int) {
	for _, s := range f.Execs {
		switch {
		case len(s.Argv) > 3 && s.Argv[3] == "conformance-gate":
			gated++
		case !slices.Equal(s.Argv, confReadMountinfo):
			plain++
		}
	}
	return gated, plain
}

// A destructive command reaches only the suite's sandbox, by its pinned
// ID, behind the gate; the decoy named caboose-default sees nothing.
func TestConfBoxTargetsTheSuitesSandbox(t *testing.T) {
	p := newConfPinned(t)
	h := &conformance.GuardHost{G: p.g}
	if _, err := h.MakeCanary("roots/"+p.g.RootNames()[0], "x", conformance.File); err != nil {
		t.Fatal(err)
	}
	read := conformance.Check{ID: "O1", Title: "read", Group: "g", Class: conformance.Observe, Roles: []string{"x"},
		Run: func(ctx context.Context, env conformance.Env) conformance.CheckResult {
			c, _ := env.Canary("x")
			sp, _ := conformance.SandboxPath(c)
			cmd, _ := conformance.Read(sp)
			if _, err := env.RunCmd(ctx, "agent", cmd); err != nil {
				return conformance.CheckResult{Status: conformance.Error, Reason: err.Error()}
			}
			return conformance.CheckResult{Status: conformance.Pass}
		}}
	rm := conformance.Check{ID: "D1", Title: "remove", Group: "g", Class: conformance.Destructive, Roles: []string{"x"},
		Run: func(ctx context.Context, env conformance.Env) conformance.CheckResult {
			c, _ := env.Canary("x")
			sp, _ := conformance.SandboxPath(c)
			cmd, _ := conformance.Remove(sp)
			if _, err := env.RunCmd(ctx, "root", cmd); err != nil {
				return conformance.CheckResult{Status: conformance.Error, Reason: err.Error()}
			}
			return conformance.CheckResult{Status: conformance.Pass}
		}}
	wipe := conformance.Check{ID: "W1", Title: "wipe", Group: "g", Class: conformance.Destructive, Wipe: true,
		Run: func(ctx context.Context, env conformance.Env) conformance.CheckResult {
			if _, err := env.Wipe(ctx); err != nil {
				return conformance.CheckResult{Status: conformance.Error, Reason: err.Error()}
			}
			return conformance.CheckResult{Status: conformance.Pass}
		}}
	env := conformance.NewEnv(p.box, h, confClock{}, config.KindContainer, config.TrustFallible)
	res, _, err := conformance.RunChecks(context.Background(), &conformance.Runner{Env: p.g, Wipe: true}, []conformance.Check{read, rm, wipe}, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Status != conformance.Pass {
			t.Fatalf("%s: %s %s", r.ID, r.Status, r.Reason)
		}
	}
	if len(p.e.decoy.Execs) != 0 {
		t.Fatalf("the decoy ran %v", p.e.decoy.Execs)
	}
	gated, plain := gatedRuns(p.e.suite)
	if gated != 2 || plain != 1 {
		t.Fatalf("the suite's sandbox ran %d gated and %d plain commands", gated, plain)
	}
	roots := p.g.RootNames()
	wants := []string{"rm /work/" + roots[0] + "/", "rm -rf /work/" + roots[0] + " /work/" + roots[1] + "; }"}
	for _, s := range p.e.suite.Execs {
		if len(s.Argv) > 3 && s.Argv[3] == "conformance-gate" {
			if len(wants) == 0 || !strings.Contains(strings.Join(s.Argv[4:], " "), wants[0]) {
				t.Errorf("the gated command is %q", s.Argv[4:])
			}
			if len(wants) > 0 {
				wants = wants[1:]
			}
			if !strings.Contains(s.Argv[2], p.g.Nonce()) || s.User != "root" {
				t.Errorf("the gate does not hold the nonce, or runs as %q", s.User)
			}
		}
	}
}

// Nothing destructive runs when the identity changed since it was
// pinned, when the mounts the sandbox shows are not the audited ones, or
// when the gate in the sandbox refuses.
func TestConfBoxRefuses(t *testing.T) {
	argv := []string{"/bin/sh", "-c", "rm /work/x"}
	cases := map[string]func(p *confPinned){
		"another container under the name": func(p *confPinned) {
			p.pins.ContainerID = confDecoyID
			p.in.ID = confDecoyID
		},
		"another engine": func(p *confPinned) { p.pins.EngineID = "F" },
		"a mount added": func(p *confPinned) {
			p.in.Mounts = append(p.in.Mounts, conformance.Mount{Source: filepath.Join(p.g.DataDir(), "home", ".config"), Target: "/home/agent/.extra"})
		},
		"a root writable in the sandbox": func(p *confPinned) {
			p.e.mountinfo = strings.Replace(p.e.mountinfo, "/work/"+p.g.RootNames()[0]+" ro,", "/work/"+p.g.RootNames()[0]+" rw,", 1)
		},
		"a disk the isolation does not show": func(p *confPinned) {
			p.e.mountinfo += "99 1 254:1 / /mnt rw - ext4 /dev/vda1 rw\n"
		},
		"the gate refuses": func(p *confPinned) {
			p.e.gate = func() *exec.Cmd { return backendtest.Reply("", conformance.GateRefused+"\n", conformance.GateExit) }
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			p := newConfPinned(t)
			mut(p)
			_, _, _, err := p.box.ExecDestructive(context.Background(), "root", argv, nil)
			var ref *conformance.RefusalError
			if !errors.As(err, &ref) {
				t.Fatalf("ran, or failed otherwise: %v", err)
			}
			if len(p.e.decoy.Execs) != 0 {
				t.Errorf("the decoy ran %v", p.e.decoy.Execs)
			}
			if name != "the gate refuses" {
				if gated, plain := gatedRuns(p.e.suite); gated+plain != 0 {
					t.Errorf("the suite's sandbox ran %d commands", gated+plain)
				}
			}
		})
	}
}

// Before it was pinned, nothing runs at all.
func TestConfBoxUnpinned(t *testing.T) {
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	e := newConfEngine(g, confInspection(t, g))
	b := &confBox{g: g, iso: config.KindContainer, open: e.open}
	if _, _, _, err := b.Exec(context.Background(), "agent", []string{"true"}, nil); err == nil {
		t.Error("an unpinned sandbox ran a command")
	}
	if _, _, _, err := b.ExecDestructive(context.Background(), "root", []string{"true"}, nil); err == nil {
		t.Error("an unpinned sandbox ran a destructive command")
	}
	if len(e.suite.Execs)+len(e.decoy.Execs) != 0 {
		t.Error("something ran")
	}
}

const confInspectJSON = `[{
  "Id": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "Name": "/caboose-cf-0011aabb",
  "Config": {"Labels": {"caboose.isolation": "container"}},
  "Mounts": [
    {"Type": "bind", "Source": "/tmp/caboose-conf.1/roots/conf-0011aabb-ro", "Destination": "/work/conf-0011aabb-ro", "RW": false},
    {"Type": "bind", "Source": "/tmp/caboose-conf.1/data/home/.config", "Destination": "/home/agent/.config", "RW": true},
    {"Type": "volume", "Name": "v1", "Source": "/var/lib/docker/volumes/v1/_data", "Destination": "/var/lib/docker", "RW": true}
  ],
  "HostConfig": {
    "Privileged": false,
    "Binds": ["/tmp/caboose-conf.1/roots/conf-0011aabb-ro:/work/conf-0011aabb-ro:ro", "/tmp/caboose-conf.1/data/home/.config:/home/agent/.config"],
    "Devices": [{"PathOnHost": "/dev/fuse", "PathInContainer": "/dev/fuse"}],
    "CapAdd": ["SYS_ADMIN"], "SecurityOpt": ["seccomp=unconfined"],
    "NetworkMode": "host", "PidMode": "host", "IpcMode": "private", "UTSMode": "", "UsernsMode": "", "CgroupnsMode": "private"
  }
}]`

func TestParseContainerInspect(t *testing.T) {
	in, err := parseContainerInspect([]byte(confInspectJSON), config.KindContainer)
	if err != nil {
		t.Fatal(err)
	}
	if in.Name != "caboose-cf-0011aabb" || in.ID != confTestID || in.Labels["caboose.isolation"] != "container" {
		t.Fatalf("%+v", in)
	}
	want := []conformance.Mount{
		{Source: "/tmp/caboose-conf.1/roots/conf-0011aabb-ro", Target: "/work/conf-0011aabb-ro", ReadOnly: true},
		{Source: "/tmp/caboose-conf.1/data/home/.config", Target: "/home/agent/.config"},
		{Volume: "v1", Target: "/var/lib/docker"},
	}
	if !slices.Equal(in.Mounts, want) {
		t.Fatalf("mounts %+v", in.Mounts)
	}
	if !slices.Equal(in.Devices, []string{"/dev/fuse:/dev/fuse"}) || !slices.Equal(in.CapAdd, []string{"SYS_ADMIN"}) ||
		in.NetworkMode != "host" || in.PidMode != "host" || !slices.Equal(in.SecurityOpt, []string{"seccomp=unconfined"}) {
		t.Fatalf("host config %+v", in)
	}
	for name, bad := range map[string]string{
		"a tmpfs mount":       strings.Replace(confInspectJSON, `"Type": "volume"`, `"Type": "tmpfs"`, 1),
		"a bind not mounted":  strings.Replace(confInspectJSON, `"Binds": [`, `"Binds": ["/:/host", `, 1),
		"two containers":      "[" + confInspectJSON[1:len(confInspectJSON)-1] + "," + confInspectJSON[1:],
		"not docker inspect":  "{}",
		"no container at all": "[]",
	} {
		if _, err := parseContainerInspect([]byte(bad), config.KindContainer); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

func TestConfVMRecord(t *testing.T) {
	r := confVMRecord{
		Dir: "/B/data/vm/caboose-cf-0011aabb",
		Machine: vm.Machine{
			Kernel: "/repo/vm-dist/kernel-arm64", Initramfs: "/B/data/vm/initramfs-arm64.cpio",
			Disks:   []vm.Disk{{Path: "/B/home/vm/images/root.img", ReadOnly: true}, {Path: "/B/data/vm/x/scratch.img"}, {Path: "/B/data/vm/x/docker.img"}},
			Shares:  []vm.Share{{Name: "s0", Path: "/B/roots/conf-0011aabb-ro", ReadOnly: true}, {Name: "s1", Path: "/B/data"}},
			Network: "nat", Console: "/B/data/vm/x/console.log",
		},
		State: vm.State{
			Labels:  map[string]string{"caboose.isolation": "vm"},
			Volumes: []string{"docker"},
			Boot: agentproto.BootSpec{
				Mounts: []agentproto.GuestMount{{Tag: vm.ShareTag, Path: "s0", Target: "/work/conf-0011aabb-ro", ReadOnly: true}, {Tag: vm.ShareTag, Path: "s1/home/.config", Target: "/home/agent/.config"}},
				Disks:  []agentproto.GuestDisk{{Device: "/dev/vdc", Target: "/var/lib/docker"}},
			},
		},
	}
	in, err := r.inspection()
	if err != nil {
		t.Fatal(err)
	}
	want := []conformance.Mount{
		{Source: "/B/roots/conf-0011aabb-ro", Target: "/work/conf-0011aabb-ro", ReadOnly: true},
		{Source: "/B/data/home/.config", Target: "/home/agent/.config"},
		{Volume: "docker", Target: "/var/lib/docker"},
	}
	if !slices.Equal(in.Mounts, want) {
		t.Fatalf("mounts %+v", in.Mounts)
	}
	m := in.Machine
	if !slices.Equal(m.BootPaths, []string{"/repo/vm-dist/kernel-arm64", "/B/data/vm/initramfs-arm64.cpio", "/B/home/vm/images/root.img"}) ||
		!slices.Equal(m.Paths, []string{"/B/data/vm/x/scratch.img", "/B/data/vm/x/docker.img", "/B/data/vm/x/console.log"}) {
		t.Fatalf("machine %+v", m)
	}
	for _, bad := range []agentproto.GuestMount{
		{Tag: vm.ShareTag, Path: "s9"}, {Tag: vm.ShareTag, Path: "s9/x"}, {Tag: "s0"}, {Tag: "s0", Path: "s0"},
		{Tag: vm.ShareTag}, {Tag: vm.ShareTag, Path: "../s0"}, {Tag: vm.ShareTag, Path: "/s0"}, {Tag: vm.ShareTag, Path: "s0/../s1"},
	} {
		r.State.Boot.Mounts[0].Tag, r.State.Boot.Mounts[0].Path = bad.Tag, bad.Path
		if _, err := r.inspection(); err == nil {
			t.Errorf("a bind of tag %q, path %q was taken", bad.Tag, bad.Path)
		}
	}
}

// The audit reads the mounts as the vm backend really plans them: one
// share, each mount a path of it under its directory's name.
func TestConfVMRecordPlanned(t *testing.T) {
	b := t.TempDir()
	for _, d := range []string{"roots/ro", "data/home/.config"} {
		if err := os.MkdirAll(filepath.Join(b, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b, "data/home/.claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := []backend.Mount{
		{Source: filepath.Join(b, "roots/ro"), Target: "/work/ro", ReadOnly: true},
		{Source: filepath.Join(b, "data/home/.config"), Target: "/home/agent/.config"},
		{Source: filepath.Join(b, "data/home/.claude.json"), Target: "/home/agent/.claude.json"},
	}
	shares, mounts, err := backend.PlanGuest(ms)
	if err != nil {
		t.Fatal(err)
	}
	r := confVMRecord{Machine: vm.Machine{Shares: shares}, State: vm.State{Boot: agentproto.BootSpec{Mounts: mounts}}}
	in, err := r.inspection()
	if err != nil {
		t.Fatal(err)
	}
	var want []conformance.Mount
	for _, m := range ms {
		want = append(want, conformance.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	if !slices.Equal(in.Mounts, want) {
		t.Fatalf("mounts %+v, want %+v", in.Mounts, want)
	}
}

func TestConfVMMStart(t *testing.T) {
	data := t.TempDir()
	name := "caboose-cf-0011aabb"
	if err := os.MkdirAll(filepath.Join(data, "vm", name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "vm", name, "vmm.pid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := func(comm string) func(args ...string) (string, error) {
		return func(args ...string) (string, error) {
			if args[1] == "comm=" {
				return comm + "\n", nil
			}
			return "Sun Oct 18  12:00:00 2026\n", nil
		}
	}
	got, err := confVMMStart(data, name, ps("/Users/x/caboose/caboose-vmm"))
	if err != nil || got != "4242@Sun Oct 18 12:00:00 2026" {
		t.Fatalf("%q, %v", got, err)
	}
	if _, err := confVMMStart(data, name, ps("bash")); err == nil {
		t.Error("a pid that is not the vmm was taken")
	}
}

func TestConfHello(t *testing.T) {
	data := t.TempDir()
	write := func(st linkState) {
		if err := writeLinkState(data, st); err != nil {
			t.Fatal(err)
		}
	}
	write(linkState{linkSettings: linkSettings{Isolation: "container", SSHAgent: "none", Trust: "fallible"}})
	if _, err := confHello(data, "container"); err == nil {
		t.Fatal("a hello without a running link was taken")
	}
	_, unlock, err := lockLinkFile(data)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	h, err := confHello(data, "container")
	if err != nil || h.SSHAgent || h.HostExec {
		t.Fatalf("%+v, %v", h, err)
	}
	if _, err := confHello(data, "vm"); err == nil {
		t.Error("a link of another isolation was taken")
	}
	write(linkState{linkSettings: linkSettings{Isolation: "container", SSHAgent: "/tmp/agent.sock", HostExec: true, Trust: "fallible"}})
	if h, err := confHello(data, "container"); err != nil || !h.SSHAgent || !h.HostExec {
		t.Fatalf("offers not seen: %+v, %v", h, err)
	}
}

// The remover removes the environment's own names and no other.
func TestConfRemoverNames(t *testing.T) {
	a := &App{Cfg: &config.Config{Env: "cf-0011aabb", Container: config.ContainerFor("cf-0011aabb")}}
	r := &confRemover{a: a}
	ctx := context.Background()
	if err := r.RemoveContainer(ctx, "caboose-default"); err == nil {
		t.Error("caboose-default was removable")
	}
	for _, im := range []string{"caboose:default", "caboose-base:default", "caboose:cf-ffffffff"} {
		if err := r.RemoveImage(ctx, im); err == nil {
			t.Errorf("%s was removable", im)
		}
	}
}

// The vm guest's exec port takes a UID or UID:GID, as agentproto's
// ExecRequest says, never a name: the suite's root is "0" there, and a
// refusal says what the guest said, not just its status. The helper here
// answers as the guest's agent does.
func TestConfBootIDUnderVM(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "self")
	guest := "#!/bin/sh\n" +
		"case \"$CABOOSE_VM_EXEC\" in\n" +
		"*'\"user\":\"0\"'*) printf '01234567-89ab-cdef-0123-456789abcdef' ;;\n" +
		"*'\"user\":'*) echo 'caboose-agent: user is not UID or UID:GID' >&2; exit 126 ;;\n" +
		"*) echo 'caboose-agent: no user' >&2; exit 126 ;;\n" +
		"esac\n"
	if err := os.WriteFile(self, []byte(guest), 0o755); err != nil {
		t.Fatal(err)
	}
	box := &backend.VM{Name: "box", Dir: vm.Dir(filepath.Join(dir, "vm")), Self: self}
	id, err := confBootID(context.Background(), box)
	if err != nil || id != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("boot_id %q, %v", id, err)
	}

	refuse := "#!/bin/sh\necho 'caboose-agent: the sandbox has not booted yet' >&2\nexit 126\n"
	if err := os.WriteFile(self, []byte(refuse), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = confBootID(context.Background(), box)
	if err == nil || !strings.Contains(err.Error(), "rc 126") || !strings.Contains(err.Error(), "has not booted yet") {
		t.Errorf("a refusal: %v", err)
	}
}

func TestConfExecUser(t *testing.T) {
	box := &backend.VM{Name: "box"}
	if u, err := confExecUser(box, "root"); err != nil || u != "0" {
		t.Errorf("the vm's root: %q %v", u, err)
	}
	if _, err := confExecUser(box, "agent"); err == nil {
		t.Error("the agent user was handed to the vm guest")
	}
	if u, err := confExecUser(&backendtest.Fake{}, "root"); err != nil || u != "root" {
		t.Errorf("docker's root: %q %v", u, err)
	}
	if u, err := confExecUser(&backendtest.Fake{}, "agent"); err != nil || u != "agent" {
		t.Errorf("docker's agent: %q %v", u, err)
	}
	for _, u := range []string{"", "nobody", "0"} {
		if _, err := confExecUser(&backendtest.Fake{}, u); err == nil {
			t.Errorf("user %q was taken", u)
		}
	}
}
