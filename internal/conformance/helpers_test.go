package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
)

// rig is a guard made in a test's temp dirs, with fake facts and inputs
// that pass until a test changes them.
type rig struct {
	t    *testing.T
	tmp  string // the fake physical /tmp
	real string // the fake user's home, with its CABOOSE_HOME and a root
	f    Facts
	g    *Guard
	insp Inspection
	pins Pins
}

const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// underTemp fails the test unless p is inside os.TempDir(): every test
// that deletes asserts it first.
func underTemp(t *testing.T, p string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strictlyInside(rp, tmp) {
		t.Fatalf("%s is not inside %s", rp, tmp)
	}
}

func testFacts(t *testing.T) (Facts, string, string) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := Facts{
		TempRoot:    tmp,
		CabooseHome: filepath.Join(real, ".caboose"),
		UserHome:    real,
		RealRoots:   func() ([]string, error) { return []string{filepath.Join(real, "dev")}, nil },
		Detectors:   []Detector{},
		GOOS:        "darwin",
	}
	return f, tmp, real
}

func newRig(t *testing.T, isolation string) *rig {
	t.Helper()
	f, tmp, real := testFacts(t)
	g, err := Setup(f, isolation, config.TrustFallible)
	if err != nil {
		t.Fatal(err)
	}
	underTemp(t, g.Base())
	r := &rig{t: t, tmp: tmp, real: real, f: f, g: g}
	for _, d := range []string{g.DataRel() + "/home/.config", g.DataRel() + "/claude-code"} {
		if err := mkdirNoFollow(g.Base(), d); err != nil {
			t.Fatal(err)
		}
	}
	ro, dev := g.RootNames()[0], g.RootNames()[1]
	r.insp = Inspection{
		Isolation: isolation,
		Labels:    map[string]string{assets.LabelIsolation: isolation},
		Mounts: []Mount{
			{Source: g.RootPath(ro), Target: "/work/" + ro, ReadOnly: true},
			{Source: g.RootPath(dev), Target: "/work/" + dev, ReadOnly: true},
			{Source: filepath.Join(g.RootPath(dev), WritablePart), Target: "/work/" + dev + "/" + WritablePart},
			{Source: filepath.Join(g.DataDir(), "home", ".config"), Target: "/home/agent/.config"},
			{Source: filepath.Join(g.DataDir(), "claude-code"), Target: "/etc/claude-code", ReadOnly: true},
		},
		Hello:  &Hello{},
		Config: ConfigFacts{SSHAgent: "none", ForwardPorts: "none"},
	}
	if isolation == config.KindVM {
		for i, m := range r.insp.Mounts {
			r.insp.Shares = append(r.insp.Shares, Share{Name: "m" + string(rune('0'+i)), Path: m.Source, ReadOnly: m.ReadOnly})
		}
		vmDir := filepath.Join(g.DataDir(), "vm", config.ContainerFor(g.Env()))
		if err := mkdirNoFollow(g.Base(), g.DataRel()+"/vm/"+config.ContainerFor(g.Env())); err != nil {
			t.Fatal(err)
		}
		r.insp.Machine = &Machine{Dir: vmDir, Network: "nat", Paths: []string{
			filepath.Join(g.CabooseHome(), "vm", "v1", "arm64", "kernel"),
			filepath.Join(g.DataDir(), "vm", "images", "root.img"),
			filepath.Join(vmDir, "console.log"),
		}}
		r.pins = Pins{VMMStart: "12345", MachineHash: strings.Repeat("ab", 32), BootID: "01234567-89ab-cdef-0123-456789abcdef"}
	} else {
		r.insp.Name = config.ContainerFor(g.Env())
		r.insp.ID = testContainerID
		r.pins = Pins{EngineID: "ENGINE", ContainerID: testContainerID}
	}
	r.attach()
	return r
}

// attach gives the guard inputs that read the rig's current fields.
func (r *rig) attach() {
	in := Inputs{
		LaunchEnv: r.g.LaunchEnv([]string{"PATH=/usr/bin", "CABOOSE_ENV=default", "DOCKER_HOST=tcp://x"}),
		Inspect:   func(context.Context) (Inspection, error) { return r.insp, nil },
		Pin:       func(context.Context) (Pins, error) { return r.pins, nil },
	}
	if r.insp.Isolation == config.KindVM {
		in.PlannedShares = append([]Share(nil), r.insp.Shares...)
	}
	r.g.Attach(in)
}

func (r *rig) path(rel string) string { return filepath.Join(r.g.Base(), filepath.FromSlash(rel)) }

func (r *rig) write(rel, content string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.path(rel)), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.path(rel), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}
