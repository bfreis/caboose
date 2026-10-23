package conformance

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
)

func TestAuditRefuses(t *testing.T) {
	add := func(src string) func(*rig) {
		return func(r *rig) { r.insp.Mounts = append(r.insp.Mounts, Mount{Source: src, Target: "/extra"}) }
	}
	cases := map[string]func(*rig){
		"source is /":                  add("/"),
		"source is the home":           add("<home>"),
		"source above B":               add("<tmp>"),
		"source is B":                  add("<B>"),
		"source is B/home":             add("<B>/home"),
		"source is the daemon dir":     add("<B>/home/daemon"),
		"source is the canaries":       add("<B>/canary"),
		"source is the data dir":       add("<D>"),
		"source is the VM dir":         add("<D>/vm"),
		"source is the data marker":    add("<D>/" + DataMarkerName),
		"source is the env dir":        add("<E>"),
		"source is config.toml":        add("<E>/config.toml"),
		"source is the store":          add("<E>/checkpoints"),
		"source is in the store":       add("<E>/checkpoints/k"),
		"source is decisions.log":      add("<E>/decisions.log"),
		"source is B/data":             add("<B>/data"),
		"source is the link's lock":    add("<D>/link.lock"),
		"source is all the roots":      add("<B>/roots"),
		"source is the real home":      add("<caboose>"),
		"source holds the real home":   add("<real>"),
		"source outside B":             add("<other>"),
		"source is the docker socket":  add("/var/run/docker.sock"),
		"source is relative":           add("roots"),
		"source is a socket in B/data": func(r *rig) {},
		"privileged":                   func(r *rig) { r.insp.Privileged = true },
		"a device":                     func(r *rig) { r.insp.Devices = []string{"/dev/kvm"} },
		"a capability":                 func(r *rig) { r.insp.CapAdd = []string{"SYS_ADMIN"} },
		"host network":                 func(r *rig) { r.insp.NetworkMode = "host" },
		"host pid":                     func(r *rig) { r.insp.PidMode = "host" },
		"host ipc":                     func(r *rig) { r.insp.IpcMode = "host" },
		"another container's network":  func(r *rig) { r.insp.NetworkMode = "container:abc" },
		"another container's pid":      func(r *rig) { r.insp.PidMode = "container:abc" },
		"another container's ipc":      func(r *rig) { r.insp.IpcMode = "container:abc" },
		"host uts":                     func(r *rig) { r.insp.UTSMode = "host" },
		"host userns":                  func(r *rig) { r.insp.UsernsMode = "host" },
		"host cgroupns":                func(r *rig) { r.insp.CgroupnsMode = "host" },
		"an unknown pid mode":          func(r *rig) { r.insp.PidMode = "weird" },
		"seccomp unconfined":           func(r *rig) { r.insp.SecurityOpt = []string{"seccomp=unconfined"} },
		"seccomp unconfined, old form": func(r *rig) { r.insp.SecurityOpt = []string{"seccomp:unconfined"} },
		"apparmor unconfined":          func(r *rig) { r.insp.SecurityOpt = []string{"apparmor=unconfined"} },
		"label disabled":               func(r *rig) { r.insp.SecurityOpt = []string{"label=disable"} },
		"label disabled, old form":     func(r *rig) { r.insp.SecurityOpt = []string{"label:disable"} },
		"new privileges allowed":       func(r *rig) { r.insp.SecurityOpt = []string{"no-new-privileges=false"} },
		"system paths unconfined":      func(r *rig) { r.insp.SecurityOpt = []string{"systempaths=unconfined"} },
		"a container with a machine":   func(r *rig) { r.insp.Machine = &Machine{} },
		"run_args":                     func(r *rig) { r.insp.Config.RunArgs = []string{"--cap-add=ALL"} },
		"engine_socket":                func(r *rig) { r.insp.Config.EngineSocket = true },
		"host_exec in config":          func(r *rig) { r.insp.Config.HostExec = true },
		"ssh_agent unset":              func(r *rig) { r.insp.Config.SSHAgent = "" },
		"a forwarded port":             func(r *rig) { r.insp.Config.ForwardPorts = "3000" },
		"hello offers the SSH agent":   func(r *rig) { r.insp.Hello = &Hello{SSHAgent: true} },
		"hello offers host exec":       func(r *rig) { r.insp.Hello = &Hello{HostExec: true} },
		"no hello":                     func(r *rig) { r.insp.Hello = nil },
		"isolation label":              func(r *rig) { r.insp.Labels = map[string]string{assets.LabelIsolation: config.KindGVisor} },
		"isolation":                    func(r *rig) { r.insp.Isolation = config.KindGVisor },
		"the user's container":         func(r *rig) { r.insp.Name = config.ContainerFor("default") },
		"a short container ID":         func(r *rig) { r.insp.ID = testContainerID[:12] },
		"two mounts at one target": func(r *rig) {
			m := r.insp.Mounts[0]
			r.insp.Mounts = append(r.insp.Mounts, m)
		},
		"an unknown volume": func(r *rig) {
			r.insp.Mounts = append(r.insp.Mounts, Mount{Volume: "caboose-default-docker", Target: "/var/lib/docker"})
		},
		"a container with shares": func(r *rig) { r.insp.Shares = []Share{{Name: "m0", Path: r.path(r.g.DataRel() + "/home")}} },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, config.KindContainer)
			other := t.TempDir()
			if name == "source is a socket in B/data" {
				sock := r.path(r.g.DataRel() + "/s")
				l, err := net.Listen("unix", sock)
				if err != nil {
					t.Skipf("no unix socket here: %v", err)
				}
				t.Cleanup(func() { l.Close() })
				r.insp.Mounts = append(r.insp.Mounts, Mount{Source: sock, Target: "/s"})
			}
			mod(r)
			for i, m := range r.insp.Mounts {
				r.insp.Mounts[i].Source = expand(m.Source, r, other)
			}
			_, err := Audit(r.insp, r.g.expect())
			if err == nil {
				t.Fatal("the audit let it through")
			}
			if err := r.g.Verify(context.Background()); err == nil {
				t.Fatal("Verify let it through")
			}
		})
	}
}

// A source that is the user's home is refused for being the home, by name,
// not only by the later rule that a source be inside the data dir or a root.
func TestAuditNamesTheHome(t *testing.T) {
	r := newRig(t, config.KindContainer)
	r.insp.Mounts = append(r.insp.Mounts, Mount{Source: r.real, Target: "/extra"})
	_, err := Audit(r.insp, r.g.expect())
	if err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("audit of a home source: %v", err)
	}
}

func expand(s string, r *rig, other string) string {
	for k, v := range map[string]string{
		"<home>": r.real, "<tmp>": r.tmp, "<B>": r.g.Base(), "<caboose>": r.f.CabooseHome,
		"<real>": filepath.Dir(r.real), "<other>": other,
		"<D>": r.g.DataDir(), "<E>": filepath.Join(r.g.Base(), filepath.FromSlash(r.g.EnvDirRel())),
	} {
		if len(s) >= len(k) && s[:len(k)] == k {
			p := v + s[len(k):]
			_ = os.MkdirAll(p, 0o755)
			return p
		}
	}
	return s
}

func TestAuditAllowsTheEngineDefaults(t *testing.T) {
	r := newRig(t, config.KindContainer)
	r.insp.NetworkMode, r.insp.PidMode, r.insp.IpcMode = "bridge", "private", "shareable"
	r.insp.UTSMode, r.insp.UsernsMode, r.insp.CgroupnsMode = "", "", "private"
	r.insp.SecurityOpt = []string{"no-new-privileges", "no-new-privileges=true", "seccomp=builtin", "label=type:x"}
	if _, err := Audit(r.insp, r.g.expect()); err != nil {
		t.Fatal(err)
	}
}

func TestAuditPassesTheSuitesOwn(t *testing.T) {
	for _, iso := range config.Kinds {
		t.Run(iso, func(t *testing.T) {
			r := newRig(t, iso)
			r.insp.Mounts = append(r.insp.Mounts, Mount{Volume: "v", Target: "/var/lib/docker"})
			in := r.g.inputs()
			in.Volumes = []string{"v"}
			r.g.Attach(in)
			res, err := Audit(r.insp, r.g.expect())
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Mounts) != len(r.insp.Mounts) {
				t.Fatalf("audited %v", res.Mounts)
			}
			for i := 1; i < len(res.Mounts); i++ {
				if res.Mounts[i-1].Target >= res.Mounts[i].Target {
					t.Fatalf("not sorted: %v", res.Mounts)
				}
			}
		})
	}
}

func TestAuditVM(t *testing.T) {
	cases := map[string]func(*rig){
		"no planned shares": func(r *rig) {
			in := r.g.inputs()
			in.PlannedShares = nil
			r.g.Attach(in)
		},
		"a share not planned": func(r *rig) {
			r.insp.Shares = append(r.insp.Shares, Share{Name: "mx", Path: r.path(r.g.DataRel() + "/home")})
		},
		"a share made writable": func(r *rig) { r.insp.Shares[0].ReadOnly = !r.insp.Shares[0].ReadOnly },
		"a share outside B": func(r *rig) {
			r.insp.Shares[0].Path = r.real
			in := r.g.inputs()
			in.PlannedShares = append([]Share(nil), r.insp.Shares...)
			r.g.Attach(in)
		},
		"the home shared": func(r *rig) {
			r.insp.Shares[0].Path = r.path("home")
			in := r.g.inputs()
			in.PlannedShares = append([]Share(nil), r.insp.Shares...)
			r.g.Attach(in)
		},
		"hello offers host exec": func(r *rig) { r.insp.Hello.HostExec = true },
		"no machine":             func(r *rig) { r.insp.Machine = nil },
		"a bridged network":      func(r *rig) { r.insp.Machine.Network = "bridged" },
		"no network named":       func(r *rig) { r.insp.Machine.Network = "" },
		"nat under hostile":      func(r *rig) { r.g.trust = config.TrustHostile },
		"another VM's dir": func(r *rig) {
			r.insp.Machine.Dir = filepath.Join(r.g.DataDir(), "vm", config.ContainerFor("default"))
		},
		"a VM dir outside B": func(r *rig) {
			r.insp.Machine.Dir = filepath.Join(r.real, "vm", config.ContainerFor(r.g.Env()))
		},
		"a disk outside B": func(r *rig) {
			r.insp.Machine.Paths = append(r.insp.Machine.Paths, filepath.Join(r.real, "root.img"))
		},
		"a relative path": func(r *rig) { r.insp.Machine.Paths = append(r.insp.Machine.Paths, "root.img") },
		"B itself":        func(r *rig) { r.insp.Machine.Paths = append(r.insp.Machine.Paths, r.g.Base()) },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, config.KindVM)
			mod(r)
			if _, err := Audit(r.insp, r.g.expect()); err == nil {
				t.Fatal("the audit let it through")
			}
		})
	}
	// No network passes under either trust.
	for _, trust := range []string{config.TrustFallible, config.TrustHostile} {
		r := newRig(t, config.KindVM)
		r.g.trust = trust
		r.insp.Machine.Network = "none"
		if _, err := Audit(r.insp, r.g.expect()); err != nil {
			t.Fatalf("%s, no network: %v", trust, err)
		}
	}
}

func TestPinsAndVerifyDestructive(t *testing.T) {
	ctx := context.Background()
	for _, iso := range config.Kinds {
		t.Run(iso, func(t *testing.T) {
			r := newRig(t, iso)
			if err := r.g.VerifyDestructive(ctx); err == nil {
				t.Fatal("verified before pinning")
			}
			if _, err := r.g.GateSpec(&GateFixture{Isolation: iso}); err == nil {
				t.Fatal("a gate spec before pinning")
			}
			if _, err := r.g.Pin(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.g.VerifyDestructive(ctx); err != nil {
				t.Fatal(err)
			}
			pins := r.pins
			if iso == config.KindVM {
				r.pins.BootID = "11111111-89ab-cdef-0123-456789abcdef"
			} else {
				r.pins.EngineID = "OTHER"
			}
			if err := r.g.VerifyDestructive(ctx); err == nil {
				t.Fatal("an identity change was not seen")
			}
			r.pins = pins
			r.insp.Mounts[2].ReadOnly = true
			if iso == config.KindVM {
				r.insp.Shares[2].ReadOnly = true
				in := r.g.inputs()
				in.PlannedShares = append([]Share(nil), r.insp.Shares...)
				r.g.Attach(in)
			}
			if err := r.g.VerifyDestructive(ctx); err == nil {
				t.Fatal("a mount change was not seen")
			}
		})
	}
}

func TestPinRefuses(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*rig){
		"no pin reader":          func(r *rig) { in := r.g.inputs(); in.Pin = nil; r.g.Attach(in) },
		"no engine ID":           func(r *rig) { r.pins.EngineID = "" },
		"a short container ID":   func(r *rig) { r.pins.ContainerID = "abc" },
		"another container":      func(r *rig) { r.pins.ContainerID = testContainerID[1:] + "0" },
		"vm pins on a container": func(r *rig) { r.pins.BootID = "x" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, config.KindContainer)
			mod(r)
			var re *RefusalError
			if _, err := r.g.Pin(ctx); !errors.As(err, &re) {
				t.Fatalf("Pin did not refuse: %v", err)
			}
		})
	}
	r := newRig(t, config.KindVM)
	r.pins.BootID = "not-a-boot-id"
	if _, err := r.g.Pin(ctx); err == nil {
		t.Fatal("a bad boot_id was pinned")
	}
}

func TestVerifyDestructiveSeesAnotherContainer(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, config.KindContainer)
	if _, err := r.g.Pin(ctx); err != nil {
		t.Fatal(err)
	}
	r.insp.ID = testContainerID[1:] + "0"
	if err := r.g.VerifyDestructive(ctx); err == nil {
		t.Fatal("a container swapped after pinning was not seen")
	}
}

// fakeEngine holds two sandboxes, the suite's and a decoy named like a
// user's, and records which one each exec reached.
type fakeEngine struct {
	byID  map[string]string // ID -> name
	execs []string          // names reached
}

func (e *fakeEngine) execByID(id string) bool {
	name, ok := e.byID[id]
	if ok {
		e.execs = append(e.execs, name)
	}
	return ok
}

func TestDestructiveExecReachesOnlyThePinnedSandbox(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, config.KindContainer)
	eng := &fakeEngine{byID: map[string]string{
		testContainerID:           config.ContainerFor(r.g.Env()),
		testContainerID[1:] + "1": config.ContainerFor("default"),
	}}
	if _, err := r.g.Pin(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := r.g.MakeCanary("roots/"+r.g.RootNames()[1]+"/"+WritablePart, "wipe", Tree)
	if err != nil {
		t.Fatal(err)
	}
	steps := []Step{
		{ID: "o1", Group: "R", Class: Observe, Run: func(context.Context) Outcome { return Outcome{Pass, ""} }},
		{ID: "d1", Group: "R", Class: Destructive, Canaries: []string{c.Rel}, Run: func(context.Context) Outcome {
			p, _, _ := r.g.Pinned()
			eng.execByID(p.ContainerID)
			return Outcome{Pass, ""}
		}},
	}
	run := &Runner{Env: r.g}
	if _, err := run.Run(ctx, steps); err != nil {
		t.Fatal(err)
	}
	if len(eng.execs) != 1 || eng.execs[0] != config.ContainerFor(r.g.Env()) {
		t.Fatalf("execs reached %v", eng.execs)
	}
	// The sandbox swapped between the pin and the exec: nothing runs.
	eng.execs = nil
	r.insp.ID = testContainerID[1:] + "1"
	r.insp.Name = config.ContainerFor("default")
	if _, err := run.Run(ctx, steps); err == nil {
		t.Fatal("the run went on against a swapped sandbox")
	}
	if len(eng.execs) != 0 {
		t.Fatalf("execs reached %v", eng.execs)
	}
}

// A VM may boot from read-only files outside B (a checkout's vm-dist, a
// release's assets) only when named exactly; a sibling, the directory, a
// directory named as an input, and a share outside B are refused.
func TestAuditBootInputs(t *testing.T) {
	r := newRig(t, config.KindVM)
	dist := t.TempDir()
	kernel := filepath.Join(dist, "kernel-arm64")
	sibling := filepath.Join(dist, "kernel-amd64")
	for _, p := range []string{kernel, sibling} {
		if err := os.WriteFile(p, []byte("k"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	in := r.g.inputs()
	in.BootInputs = []string{kernel, dist}
	r.g.Attach(in)
	audit := func(boot ...string) error {
		r.insp.Machine.BootPaths = boot
		_, err := Audit(r.insp, r.g.expect())
		return err
	}
	if err := audit(kernel, filepath.Join(r.g.CabooseHome(), "vm", "initramfs")); err != nil {
		t.Fatalf("the named kernel was refused: %v", err)
	}
	for _, bad := range []string{sibling, dist, filepath.Join(dist, "missing"), "relative/kernel"} {
		if err := audit(bad); err == nil {
			t.Errorf("boot path %s was taken", bad)
		}
	}
	if err := audit(); err != nil {
		t.Fatal(err)
	}
	// A boot input does not make its path a share's, nor a writable
	// path's.
	r.insp.Machine.Paths = append(r.insp.Machine.Paths, kernel)
	if err := audit(); err == nil {
		t.Error("a writable machine path outside B was taken because it is a boot input")
	}
	r.insp.Machine.Paths = r.insp.Machine.Paths[:len(r.insp.Machine.Paths)-1]
	r.insp.Shares = append(r.insp.Shares, Share{Name: "k", Path: dist, ReadOnly: true})
	in.PlannedShares = append(in.PlannedShares, Share{Name: "k", Path: dist, ReadOnly: true})
	r.g.Attach(in)
	if err := audit(); err == nil {
		t.Error("a share outside B was taken because it is a boot input's directory")
	}
}
