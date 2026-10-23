package conformance

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/shelltest"
)

const (
	gateNonce  = "0011aabb00112233445566778899aabb"
	gateBootID = "01234567-89ab-cdef-0123-456789abcdef"
)

var gateRoots = []string{"conf-0011aabb-ro", "conf-0011aabb-dev"}

// gateMountinfo is a container's mountinfo for gateSpec.
const gateMountinfo = `612 520 0:180 / / rw,relatime master:208 - overlay overlay rw,lowerdir=/x
613 612 0:183 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
614 612 0:184 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
616 612 0:186 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
620 612 254:1 /docker/containers/c/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/vda1 rw
621 612 254:1 /docker/containers/c/hosts /etc/hosts rw,relatime - ext4 /dev/vda1 rw
623 612 0:52 /B/roots/conf-0011aabb-ro /work/conf-0011aabb-ro ro,relatime - virtiofs mac rw
624 612 0:52 /B/roots/conf-0011aabb-dev /work/conf-0011aabb-dev ro,relatime - virtiofs mac rw
625 624 0:52 /B/roots/conf-0011aabb-dev/part /work/conf-0011aabb-dev/part rw,relatime shared:3 - virtiofs mac rw
626 612 0:52 /B/data/home/.config /home/agent/.config rw,relatime - virtiofs mac rw
628 612 0:52 /B/data/claude-code /etc/claude-code ro,relatime - virtiofs mac rw
`

func gateSpec(iso string) GateSpec {
	s := GateSpec{
		Isolation: iso,
		Nonce:     gateNonce,
		Roots:     gateRoots,
		Mounts: []AuditedMount{
			{"/etc/claude-code", true},
			{"/home/agent/.config", false},
			{"/work/conf-0011aabb-dev", true},
			{"/work/conf-0011aabb-dev/part", false},
			{"/work/conf-0011aabb-ro", true},
		},
		Pseudo: DefaultPseudo,
		Fixed:  FixtureFor(config.KindContainer).Fixed,
	}
	if iso == config.KindVM {
		s.BootID = gateBootID
	}
	return s
}

// gateWorld is a work dir, a mountinfo and a boot_id in a temp dir.
type gateWorld struct {
	t   *testing.T
	dir string
	p   gatePaths
}

func newGateWorld(t *testing.T) *gateWorld {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &gateWorld{t: t, dir: dir, p: gatePaths{
		work: filepath.Join(dir, "work"), mountinfo: filepath.Join(dir, "mountinfo"), bootID: filepath.Join(dir, "boot_id"),
	}}
	for _, r := range gateRoots {
		w.write("work/"+r+"/"+RootMarker, gateNonce+"\n")
	}
	w.write("mountinfo", gateMountinfo)
	w.write("boot_id", gateBootID+"\n")
	return w
}

func (w *gateWorld) write(rel, content string) {
	w.t.Helper()
	p := filepath.Join(w.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// run runs `echo ran` behind the gate with each shell; it returns the
// exit status and output of the first, failing if the shells disagree.
func (w *gateWorld) run(spec GateSpec) (int, string) {
	w.t.Helper()
	script, err := gateScript(spec, w.p)
	if err != nil {
		w.t.Fatal(err)
	}
	argv, err := GatedArgv(script, []string{"/bin/sh", "-c", "echo ran"})
	if err != nil {
		w.t.Fatal(err)
	}
	first, firstOut := -1, ""
	for i, sh := range shelltest.Shells(w.t) {
		cmd := exec.Command(sh[0], append(sh[1:], argv[1:]...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		rc := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rc = ee.ExitCode()
		} else if err != nil {
			w.t.Fatal(err)
		}
		if i == 0 {
			first, firstOut = rc, string(out)
		} else if rc != first || string(out) != firstOut {
			w.t.Errorf("%v: %d %q, but /bin/sh: %d %q", sh, rc, out, first, firstOut)
		}
	}
	return first, firstOut
}

func (w *gateWorld) expect(spec GateSpec, pass bool) {
	w.t.Helper()
	rc, out := w.run(spec)
	switch {
	case pass && (rc != 0 || out != "ran\n"):
		w.t.Fatalf("the gate refused: %d %q", rc, out)
	case !pass && (rc != GateExit || out != GateRefused+"\n"):
		w.t.Fatalf("the gate let it through: %d %q", rc, out)
	}
}

func TestGateScriptSyntax(t *testing.T) {
	for _, iso := range config.Kinds {
		script, err := GateScript(gateSpec(iso))
		if err != nil {
			t.Fatal(err)
		}
		for _, sh := range shelltest.Shells(t) {
			if out, err := exec.Command(sh[0], append(sh[1:], "-n", "-c", script)...).CombinedOutput(); err != nil {
				t.Errorf("%s %v: %v %s", iso, sh, err, out)
			}
		}
		for _, w := range []string{"/work/*", "/proc/self/mountinfo", gateNonce, "conf-0011aabb-ro"} {
			if !strings.Contains(script, w) {
				t.Errorf("%s: the gate does not name %s", iso, w)
			}
		}
		if (iso == config.KindVM) != strings.Contains(script, gateBootID) {
			t.Errorf("%s: boot_id compared: %t", iso, strings.Contains(script, gateBootID))
		}
	}
}

func TestGatePasses(t *testing.T) {
	for _, iso := range []string{config.KindContainer, config.KindVM} {
		w := newGateWorld(t)
		w.expect(gateSpec(iso), true)
	}
}

func TestGateRefuses(t *testing.T) {
	cases := []struct {
		name  string
		world func(w *gateWorld)
		spec  func(s *GateSpec)
		iso   string
	}{
		{name: "another root in /work", world: func(w *gateWorld) { w.write("work/dev/x", "") }},
		{name: "a hidden entry in /work", world: func(w *gateWorld) { w.write("work/.x", "") }},
		{name: "a file named * in /work", world: func(w *gateWorld) { w.write("work/*", "") }},
		{name: "a root missing", world: func(w *gateWorld) {
			if err := os.RemoveAll(filepath.Join(w.dir, "work", gateRoots[0])); err != nil {
				w.t.Fatal(err)
			}
		}},
		{name: "a root that is a link", world: func(w *gateWorld) {
			ro := filepath.Join(w.dir, "work", gateRoots[0])
			if err := os.Rename(ro, filepath.Join(w.dir, "elsewhere")); err != nil {
				w.t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(w.dir, "elsewhere"), ro); err != nil {
				w.t.Fatal(err)
			}
		}},
		{name: "a wrong nonce", world: func(w *gateWorld) { w.write("work/"+gateRoots[1]+"/"+RootMarker, strings.Repeat("f", 32)+"\n") }},
		{name: "a nonce and more", world: func(w *gateWorld) { w.write("work/"+gateRoots[1]+"/"+RootMarker, gateNonce+"\nx") }},
		{name: "a nonce without its newline", world: func(w *gateWorld) { w.write("work/"+gateRoots[0]+"/"+RootMarker, gateNonce) }},
		{name: "a marker that is a link", world: func(w *gateWorld) {
			m := filepath.Join(w.dir, "work", gateRoots[0], RootMarker)
			w.write("nonce", gateNonce+"\n")
			if err := os.Remove(m); err != nil {
				w.t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(w.dir, "nonce"), m); err != nil {
				w.t.Fatal(err)
			}
		}},
		{name: "the part read-only", world: func(w *gateWorld) {
			w.write("mountinfo", strings.Replace(gateMountinfo, "/work/conf-0011aabb-dev/part rw,", "/work/conf-0011aabb-dev/part ro,", 1))
		}},
		{name: "a root writable", world: func(w *gateWorld) {
			w.write("mountinfo", strings.Replace(gateMountinfo, "/work/conf-0011aabb-ro ro,", "/work/conf-0011aabb-ro rw,", 1))
		}},
		{name: "an extra mount under /work", world: func(w *gateWorld) {
			w.write("mountinfo", gateMountinfo+"630 612 0:52 /Users/x/dev /work/dev rw - virtiofs mac rw\n")
		}},
		{name: "a mount stacked on the part", world: func(w *gateWorld) {
			w.write("mountinfo", gateMountinfo+"630 625 0:9 / /work/conf-0011aabb-dev/part rw - tmpfs tmpfs rw\n")
		}},
		{name: "an extra mount under the home", world: func(w *gateWorld) {
			w.write("mountinfo", gateMountinfo+"630 612 0:52 /x /home/agent/.ssh rw - virtiofs mac rw\n")
		}},
		{name: "a host disk elsewhere", world: func(w *gateWorld) {
			w.write("mountinfo", gateMountinfo+"630 612 254:1 / /mnt rw - ext4 /dev/vda1 rw\n")
		}},
		{name: "an audited mount missing", world: func(w *gateWorld) {
			w.write("mountinfo", strings.Replace(gateMountinfo, "/etc/claude-code ro", "/etc/other ro", 1))
		}},
		{name: "no mountinfo", world: func(w *gateWorld) {
			if err := os.Remove(w.p.mountinfo); err != nil {
				w.t.Fatal(err)
			}
		}},
		{name: "another boot", iso: config.KindVM, world: func(w *gateWorld) { w.write("boot_id", "11111111-89ab-cdef-0123-456789abcdef\n") }},
		{name: "a mount audited read-only is writable", spec: func(s *GateSpec) { s.Mounts[1].ReadOnly = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newGateWorld(t)
			underTemp(t, w.dir)
			iso := tc.iso
			if iso == "" {
				iso = config.KindContainer
			}
			spec := gateSpec(iso)
			w.expect(spec, true)
			if tc.world != nil {
				tc.world(w)
			}
			if tc.spec != nil {
				spec.Mounts = append([]AuditedMount(nil), spec.Mounts...)
				tc.spec(&spec)
			}
			w.expect(spec, false)
		})
	}
}

func TestGateSpecRefused(t *testing.T) {
	cases := map[string]func(s *GateSpec){
		"a quote in a target":  func(s *GateSpec) { s.Mounts[0].Target = "/home/agent/x'y" },
		"a space in a target":  func(s *GateSpec) { s.Mounts[0].Target = "/home/agent/x y" },
		"a glob in a target":   func(s *GateSpec) { s.Mounts[0].Target = "/home/agent/*" },
		"/ audited":            func(s *GateSpec) { s.Mounts[0].Target = "/" },
		"no mounts":            func(s *GateSpec) { s.Mounts = nil },
		"a short nonce":        func(s *GateSpec) { s.Nonce = "0011aabb" },
		"a root named dev":     func(s *GateSpec) { s.Roots = []string{"conf-0011aabb-ro", "dev"} },
		"another run's root":   func(s *GateSpec) { s.Roots = []string{"conf-0011aabb-ro", "conf-ffffffff-dev"} },
		"one root":             func(s *GateSpec) { s.Roots = s.Roots[:1] },
		"a boot_id off vm":     func(s *GateSpec) { s.BootID = gateBootID },
		"a pattern as a type":  func(s *GateSpec) { s.Pseudo = []string{"*"} },
		"a fixed mount's type": func(s *GateSpec) { s.Fixed = []FixedMount{{"/etc/hosts", "ext4|*"}} },
		"no pseudo types":      func(s *GateSpec) { s.Pseudo = nil },
		"an unknown isolation": func(s *GateSpec) { s.Isolation = "chroot" },
	}
	for name, mut := range cases {
		s := gateSpec(config.KindContainer)
		s.Mounts = append([]AuditedMount(nil), s.Mounts...)
		mut(&s)
		if _, err := GateScript(s); err == nil {
			t.Errorf("%s: a gate was built", name)
		}
	}
	vm := gateSpec(config.KindVM)
	vm.BootID = "not-one"
	if _, err := GateScript(vm); err == nil {
		t.Error("a vm gate with no boot_id was built")
	}
}

// Every destructive command goes to a GatedSandbox's ExecDestructive, and
// nothing else does.
type gatedRecorder struct {
	plain, gated []string
}

func (g *gatedRecorder) Exec(_ context.Context, _ string, argv []string, _ []byte) (int, string, string, error) {
	g.plain = append(g.plain, argv[2])
	return 0, "\nrc=0\n", "", nil
}

func (g *gatedRecorder) ExecDestructive(_ context.Context, _ string, argv []string, _ []byte) (int, string, string, error) {
	g.gated = append(g.gated, argv[2])
	return 0, "\nrc=0\n", "", nil
}

func (g *gatedRecorder) Mounts() []Mount           { return nil }
func (g *gatedRecorder) Labels() map[string]string { return nil }
func (g *gatedRecorder) State() string             { return StateRunning }

// only reports whether every command went the one way.
func (g *gatedRecorder) only(gated bool) bool {
	if gated {
		return len(g.plain) == 0 && len(g.gated) > 0
	}
	return len(g.gated) == 0 && len(g.plain) > 0
}

func has(list []string, sub string) bool { return strings.Contains(strings.Join(list, "\n"), sub) }

// runHost is a Host with the run's nonce and its list of canaries, and
// nothing else.
type runHost struct {
	Host
	nonce string
	cans  []Canary
}

func (h runHost) Nonce() string      { return h.nonce }
func (h runHost) Canaries() []Canary { return h.cans }

func (g *gatedRecorder) sealedEnv(class Class, wipe bool) Env {
	h := runHost{nonce: "0011aabb" + strings.Repeat("0", 24), cans: []Canary{{Role: "managed", Rel: "data/claude-code/conf-0011aabb-managed"}}}
	e := NewEnv(g, h, nil, config.KindContainer, config.TrustFallible)
	e.seal = &seal{id: "X", class: class, wipe: wipe}
	return e
}

func TestDestructiveGoesGated(t *testing.T) {
	g := &gatedRecorder{}
	p := SPath{"/work/conf-0011aabb-ro/conf-0011aabb-ro-file"}
	ctx := context.Background()
	app, _ := Append(p)
	rm, _ := Remove(p)
	if _, err := g.sealedEnv(Write, false).RunCmd(ctx, "agent", app); err != nil {
		t.Fatal(err)
	}
	if !g.only(false) || !has(g.plain, "echo x >>") {
		t.Fatalf("a write went %q / %q", g.plain, g.gated)
	}
	g.plain, g.gated = nil, nil
	d := g.sealedEnv(Destructive, true)
	if _, err := d.RunCmd(ctx, "agent", rm); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	wipe := "rm -rf /work/conf-0011aabb-ro /work/conf-0011aabb-dev /etc/claude-code/conf-0011aabb-managed"
	if !g.only(true) || !has(g.gated, "rm "+p.p) || !has(g.gated, wipe) {
		t.Fatalf("destructive commands went %q / %q", g.plain, g.gated)
	}
}

// The sandbox-side gate lets runsc's /dev through under gVisor, and only
// there and only of its type.
func TestGatePassesGVisor(t *testing.T) {
	b, err := os.ReadFile("testdata/mountinfo-gvisor.txt")
	if err != nil {
		t.Fatal(err)
	}
	spec := gateSpec(config.KindGVisor)
	fx := FixtureFor(config.KindGVisor)
	spec.Pseudo, spec.Fixed = append(append([]string{}, DefaultPseudo...), fx.Pseudo...), fx.Fixed
	// The gate names no path with a space: the fixture's spaced mount is
	// the host-side comparison's to test.
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, `\040`) {
			keep = append(keep, l)
		}
	}
	b = []byte(strings.Join(keep, "\n"))
	w := newGateWorld(t)
	w.write("mountinfo", string(b))
	w.expect(spec, true)
	w.write("mountinfo", string(b)+"18 1 0:22 / /mnt/dev rw,noatime - dev none rw\n")
	w.expect(spec, false)
	// Nor is a 9p or gofer mount anywhere but runsc's root and the
	// engine's files.
	for _, l := range []string{
		"18 1 0:22 / /mnt rw,noatime - 9p none rw,trans=fd\n",
		"18 1 0:22 / /opt rw,noatime - gofer none rw\n",
	} {
		w.write("mountinfo", string(b)+l)
		w.expect(spec, false)
	}
	w.write("mountinfo", strings.ReplaceAll(string(b), "/ / rw,noatime - 9p", "/ / rw,noatime - gofer"))
	w.expect(spec, true)
	// docker run --init's init, at /sbin/docker-init on the engine's
	// side, is listed where a merged /usr puts it, as a real gVisor run
	// showed; the same file anywhere else is refused.
	w.write("mountinfo", string(b)+"18 1 0:22 / /usr/bin/docker-init ro,noatime - 9p none rw,trans=fd\n")
	w.expect(spec, true)
	for _, at := range []string{"/usr/local/bin/docker-init", "/usr/bin/docker-init2", "/usr/bin"} {
		w.write("mountinfo", string(b)+"18 1 0:22 / "+at+" ro,noatime - 9p none rw,trans=fd\n")
		w.expect(spec, false)
	}
	// The container fixture has no such mount.
	w.write("mountinfo", string(b))
	c := spec
	c.Isolation, c.Pseudo, c.Fixed = config.KindContainer, DefaultPseudo, FixtureFor(config.KindContainer).Fixed
	w.expect(c, false)
}

// vmGateSpec is gateSpec under vm, with the vm fixture.
func vmGateSpec() GateSpec {
	spec := gateSpec(config.KindVM)
	fx := FixtureFor(config.KindVM)
	spec.Pseudo, spec.Fixed = append(append([]string{}, DefaultPseudo...), fx.Pseudo...), fx.Fixed
	return spec
}

// A vm guest's mounts, as its init and agent make them: securityfs at
// /sys/kernel/security is let through there and only there, of its type;
// the virtio-fs share's staging mount, which the agent detaches once the
// binds are made, is refused should it be left, on both sides of the gate.
func TestGateVM(t *testing.T) {
	b, err := os.ReadFile("testdata/mountinfo-vm.txt")
	if err != nil {
		t.Fatal(err)
	}
	mi := string(b)
	staging := "10 4 0:28 / /run/caboose/shares/caboose rw,relatime - virtiofs caboose rw\n"
	secElsewhere := "17 1 0:6 / /mnt/sec rw,relatime - securityfs securityfs rw\n"
	spec := vmGateSpec()
	for _, x := range []struct {
		name, mi string
		spec     GateSpec
		pass     bool
	}{
		{"the guest's mounts", mi, spec, true},
		{"the staging mount left", mi + staging, spec, false},
		{"securityfs elsewhere", mi + secElsewhere, spec, false},
	} {
		es, err := ParseMountinfo(strings.NewReader(x.mi))
		if err != nil {
			t.Fatal(err)
		}
		if err := CompareMountinfo(es, x.spec); (err == nil) != x.pass {
			t.Errorf("%s: host side: %v", x.name, err)
		}
		w := newGateWorld(t)
		w.write("mountinfo", x.mi)
		w.expect(x.spec, x.pass)
	}
	// The refusal names the staging mount as the real run's gate did.
	es, _ := ParseMountinfo(strings.NewReader(mi + staging))
	if err := CompareMountinfo(es, spec); err == nil || !strings.Contains(err.Error(), `"/run/caboose/shares/caboose" is a "virtiofs" mount (source "caboose")`) {
		t.Errorf("refusal: %v", err)
	}
	// Under the container fixture securityfs is not let through.
	c := gateSpec(config.KindContainer)
	es, _ = ParseMountinfo(strings.NewReader(gateMountinfo + "629 616 0:6 / /sys/kernel/security rw - securityfs securityfs rw\n"))
	if err := CompareMountinfo(es, c); err == nil {
		t.Error("securityfs passed under the container fixture")
	}
}
