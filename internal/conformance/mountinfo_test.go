package conformance

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

// fixtureSpec is the gate's spec for testdata/mountinfo-container.txt.
func fixtureSpec() GateSpec {
	return GateSpec{
		Isolation: config.KindContainer,
		Nonce:     "0011aabb" + strings.Repeat("0", 24),
		Roots:     []string{"conf-0011aabb-ro", "conf-0011aabb-dev"},
		Mounts: []AuditedMount{
			{"/etc/claude-code", true},
			{"/home/agent/.config", false},
			{"/home/agent/my dir", false},
			{"/work/conf-0011aabb-dev", true},
			{"/work/conf-0011aabb-dev/part", false},
			{"/work/conf-0011aabb-ro", true},
		},
		Pseudo: DefaultPseudo,
		Fixed: []FixedMount{
			{"/etc/resolv.conf", "ext4"}, {"/etc/hostname", "ext4"}, {"/etc/hosts", "ext4"},
		},
	}
}

func readFixture(t *testing.T, name string) []MountEntry {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	es, err := ParseMountinfo(f)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestParseMountinfo(t *testing.T) {
	es := readFixture(t, "mountinfo-container.txt")
	if len(es) != 18 {
		t.Fatalf("%d entries", len(es))
	}
	e := es[0]
	if e.ID != 612 || e.Parent != 520 || e.Target != "/" || e.FSType != "overlay" || e.Source != "overlay" || e.ReadOnly() {
		t.Fatalf("root entry %+v", e)
	}
	var spaced, ro *MountEntry
	for i := range es {
		switch es[i].Target {
		case "/home/agent/my dir":
			spaced = &es[i]
		case "/etc/claude-code":
			ro = &es[i]
		}
	}
	if spaced == nil || spaced.Root != "/B/data/home/my dir" {
		t.Fatalf("escapes not undone: %+v", spaced)
	}
	if ro == nil || !ro.ReadOnly() {
		t.Fatalf("read-only not seen: %+v", ro)
	}
	for _, bad := range []string{
		"",
		"1 2 0:1 / /x rw\n",
		"1 2 0:1 / /x rw - \n",
		"x 2 0:1 / /x rw - proc proc rw\n",
		`1 2 0:1 / /x\04 rw - proc proc rw` + "\n",
		`1 2 0:1 / /x\999 rw - proc proc rw` + "\n",
	} {
		if _, err := ParseMountinfo(strings.NewReader(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestCompareMountinfo(t *testing.T) {
	if err := CompareMountinfo(readFixture(t, "mountinfo-container.txt"), fixtureSpec()); err != nil {
		t.Fatalf("the audited state refused: %v", err)
	}
	for _, name := range []string{
		"mountinfo-part-readonly.txt", // a flag differs from the audit
		"mountinfo-extra-root.txt",    // a root the audit did not see
		"mountinfo-stacked.txt",       // a mount over an audited one
		"mountinfo-unmounted.txt",     // an audited mount gone
		"mountinfo-host-disk.txt",     // a disk outside the watched targets
	} {
		if err := CompareMountinfo(readFixture(t, name), fixtureSpec()); err == nil {
			t.Fatalf("%s passed", name)
		}
	}
	// Without the isolation's fixed mounts, the engine's own binds refuse.
	spec := fixtureSpec()
	spec.Fixed = nil
	if err := CompareMountinfo(readFixture(t, "mountinfo-container.txt"), spec); err == nil {
		t.Fatal("unlisted engine binds passed")
	}
	// A fixed mount is allowed at its target only.
	spec = fixtureSpec()
	spec.Fixed = append(spec.Fixed, FixedMount{"/etc/hosts", "ext4"})
	if err := CompareMountinfo(readFixture(t, "mountinfo-host-disk.txt"), spec); err == nil {
		t.Fatal("a fixed mount's type was taken at another target")
	}
	// An audited volume outside the watched targets.
	spec = fixtureSpec()
	es := append(readFixture(t, "mountinfo-container.txt"), MountEntry{Target: "/var/lib/docker", FSType: "ext4", Options: []string{"rw"}})
	if err := CompareMountinfo(es, spec); err == nil {
		t.Fatal("an unaudited volume passed")
	}
	spec.Mounts = append(spec.Mounts, AuditedMount{"/var/lib/docker", false})
	if err := CompareMountinfo(es, spec); err != nil {
		t.Fatalf("an audited volume refused: %v", err)
	}
}

func TestGateSpec(t *testing.T) {
	ctx := context.Background()
	for _, iso := range config.Kinds {
		t.Run(iso, func(t *testing.T) {
			r := newRig(t, iso)
			if _, err := r.g.Pin(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := r.g.GateSpec(nil); err == nil {
				t.Fatal("a spec without a fixture")
			}
			other := config.KindGVisor
			if iso == other {
				other = config.KindContainer
			}
			if _, err := r.g.GateSpec(&GateFixture{Isolation: other}); err == nil {
				t.Fatal("another isolation's fixture was taken")
			}
			spec, err := r.g.GateSpec(&GateFixture{Isolation: iso, Pseudo: []string{"9p"}})
			if err != nil {
				t.Fatal(err)
			}
			if spec.Nonce != r.g.Nonce() || len(spec.Roots) != 2 || spec.Roots[0] != r.g.RootNames()[0] || len(spec.Mounts) != len(r.insp.Mounts) {
				t.Fatalf("spec %+v", spec)
			}
			for _, root := range spec.Roots {
				if root == "dev" || root == "ro" || !strings.HasPrefix(root, "conf-"+r.g.Nonce8()+"-") {
					t.Fatalf("root %q could be a user's", root)
				}
			}
			if (iso == config.KindVM) != (spec.BootID == r.pins.BootID && spec.BootID != "") {
				t.Fatalf("boot_id %q", spec.BootID)
			}
			if !strings.Contains(strings.Join(spec.Pseudo, " "), "9p") {
				t.Fatalf("pseudo %v", spec.Pseudo)
			}
		})
	}
}

// gVisor's sentry serves /dev itself, as a filesystem of type "dev" with
// source "none" (runsc names every source so), where runc mounts a tmpfs:
// the shape of runsc's mountinfo. The gVisor fixture lets that through at
// /dev and nowhere else; before it did, every destructive command of a
// gVisor run was refused at the gate.
func TestCompareMountinfoGVisor(t *testing.T) {
	spec := fixtureSpec()
	fx := FixtureFor(config.KindGVisor)
	spec.Isolation, spec.Pseudo, spec.Fixed = config.KindGVisor, append(slices.Clone(DefaultPseudo), fx.Pseudo...), fx.Fixed
	es := readFixture(t, "mountinfo-gvisor.txt")
	if err := CompareMountinfo(es, spec); err != nil {
		t.Fatalf("runsc's own mounts refused: %v", err)
	}
	// The "dev" type is allowed at /dev only.
	moved := append(slices.Clone(es), MountEntry{Target: "/mnt/dev", FSType: "dev", Source: "none", Options: []string{"rw"}})
	if err := CompareMountinfo(moved, spec); err == nil {
		t.Fatal("a dev mount elsewhere passed")
	}
	// 9p and gofer are allowed at runsc's root and the engine's files
	// only: a share of either type anywhere else outside the watched
	// targets is refused.
	for _, m := range []MountEntry{
		{Target: "/mnt", FSType: "9p", Source: "none", Options: []string{"rw"}},
		{Target: "/opt", FSType: "gofer", Source: "none", Options: []string{"rw"}},
		{Target: "/etc/hosts.d", FSType: "9p", Source: "none", Options: []string{"ro"}},
	} {
		if err := CompareMountinfo(append(slices.Clone(es), m), spec); err == nil {
			t.Fatalf("a %s mount at %s passed", m.FSType, m.Target)
		}
	}
	// runsc's root may be either type, and its engine files too.
	gofer := slices.Clone(es)
	for i := range gofer {
		if !watched(gofer[i].Target) && gofer[i].FSType == "9p" {
			gofer[i].FSType = "gofer"
		}
	}
	if err := CompareMountinfo(gofer, spec); err != nil {
		t.Fatalf("runsc's gofer mounts refused: %v", err)
	}
	// Under the container fixture it is still refused, and the refusal
	// names every mount the isolation does not show at once.
	spec = fixtureSpec()
	err := CompareMountinfo(append(readFixture(t, "mountinfo-container.txt"),
		MountEntry{Target: "/dev", FSType: "dev", Source: "none", Options: []string{"rw"}},
		MountEntry{Target: "/mnt/x", FSType: "ext4", Source: "/dev/vdb", Options: []string{"rw"}}), spec)
	if err == nil || !strings.Contains(err.Error(), `"/dev" is a "dev" mount (source "none")`) || !strings.Contains(err.Error(), `"/mnt/x" is a "ext4" mount`) {
		t.Fatalf("refusal: %v", err)
	}
	// The refusal is bounded: the sandbox writes its mount table.
	var many []MountEntry
	for i := 0; i < 50; i++ {
		many = append(many, MountEntry{Target: fmt.Sprintf("/mnt/%d/%s", i, strings.Repeat("x", 500)), FSType: "ext4", Source: strings.Repeat("s", 500), Options: []string{"rw"}})
	}
	err = CompareMountinfo(append(readFixture(t, "mountinfo-container.txt"), many...), spec)
	if err == nil || !strings.Contains(err.Error(), "and 30 more") || len(err.Error()) > 20*(3*maxFieldBytes+64)+128 {
		t.Fatalf("an unbounded refusal: %d bytes", len(err.Error()))
	}
}
