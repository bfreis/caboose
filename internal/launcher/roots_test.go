package launcher

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/proposal"
)

// randomRoots are up to four roots with writable lists config.toml would
// take: none, the whole root, or a few parts, nested at most three deep.
func randomRoots(rng *rand.Rand) []config.Root {
	n := 1 + rng.Intn(4)
	var roots []config.Root
	for i := range n {
		name := fmt.Sprintf("r%d", i)
		r := config.Root{Name: name, Host: "/h/" + name, Container: config.WorkDir + "/" + name}
		if n == 1 && rng.Intn(4) == 0 {
			r.Container = config.WorkDir
		}
		switch rng.Intn(4) {
		case 0:
		case 1:
			r.Writable = []string{"."}
		default:
			for range 1 + rng.Intn(5) {
				var comps []string
				for range 1 + rng.Intn(3) {
					comps = append(comps, string(rune('a'+rng.Intn(3))))
				}
				w := strings.Join(comps, "/")
				if config.CheckWritable(append(slices.Clone(r.Writable), w)) == nil {
					r.Writable = append(r.Writable, w)
				}
			}
		}
		roots = append(roots, r)
	}
	return roots
}

// The mount plan, over many random configurations: every writable mount is
// a configured writable part (or a root marked "."), every root not so
// marked is read-only, every file keeps the container path the whole root
// gives it, every part is there, and no mount comes before one that holds
// it. The plan is a function of the roots alone: nothing the sandbox asks
// for, granted or not, can make a mount writable.
func TestRootMountsProperty(t *testing.T) {
	if got, want := reflect.TypeOf(rootMounts), reflect.TypeOf(func([]config.Root) []backend.Mount { return nil }); got != want {
		t.Fatalf("rootMounts is %v: the mount plan takes the roots and nothing else", got)
	}
	rng := rand.New(rand.NewSource(24))
	for c := range 10000 {
		roots := randomRoots(rng)
		ms := rootMounts(roots)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("case %d, roots %+v, mounts %+v: %s", c, roots, ms, fmt.Sprintf(format, args...))
		}
		writable := map[string]bool{}
		parts := 0
		for _, r := range roots {
			for _, h := range r.WritableHost() {
				writable[h] = true
			}
			if !r.WholeWritable() {
				parts += len(r.Writable)
			}
		}
		if len(ms) != len(roots)+parts {
			fail("%d mounts, want %d", len(ms), len(roots)+parts)
		}
		seen := map[string]bool{}
		for i, m := range ms {
			seen[m.Source] = true
			if !m.ReadOnly && !writable[m.Source] {
				fail("%s is writable, and no writable part", m.Source)
			}
			for _, r := range roots {
				if m.Source == r.Host && m.ReadOnly == r.WholeWritable() {
					fail("root %s mounted read-only %v with writable %q", r.Name, m.ReadOnly, r.Writable)
				}
			}
			if at, ok := config.ContainerPath(roots, m.Source); !ok || at != m.Target {
				fail("%s mounted at %s, which the whole root has at %q", m.Source, m.Target, at)
			}
			for _, later := range ms[i+1:] {
				if m.Target != later.Target && config.Within(m.Target, later.Target) {
					fail("%s comes before %s, which holds it", m.Target, later.Target)
				}
			}
		}
		for h := range writable {
			if !seen[h] {
				fail("writable part %s is not mounted", h)
			}
		}
	}
}

// What a sandbox may write of a root, read back from its label: an older
// sandbox's (no label), or one that cannot be read, is taken to write all
// of it.
func TestMountedWritable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   []string
	}{
		{"no label", map[string]string{}, []string{"."}},
		{"not JSON", map[string]string{assets.LabelWritable: "nope"}, []string{"."}},
		{"null", map[string]string{assets.LabelWritable: "null"}, []string{"."}},
		{"root not in it", map[string]string{assets.LabelWritable: `{"/work/x":[]}`}, []string{"."}},
		{"a bad part", map[string]string{assets.LabelWritable: `{"/work/dev":["../up"]}`}, []string{"."}},
		{"read-only", map[string]string{assets.LabelWritable: `{"/work/dev":[]}`}, nil},
		{"parts", map[string]string{assets.LabelWritable: `{"/work/dev":["a","b/c"]}`}, []string{"a", "b/c"}},
		{"whole", map[string]string{assets.LabelWritable: `{"/work/dev":["."]}`}, []string{"."}},
	} {
		if got := mountedWritable(tc.labels, "/work/dev"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// The label round-trips, sorted, every root in it.
	roots := []config.Root{{Container: "/work/a", Writable: []string{"z", "b"}}, {Container: "/work/b"}}
	l := map[string]string{assets.LabelWritable: writableLabel(roots)}
	if l[assets.LabelWritable] != `{"/work/a":["b","z"],"/work/b":[]}` {
		t.Errorf("label %s", l[assets.LabelWritable])
	}
	if got := mountedWritable(l, "/work/a"); !slices.Equal(got, []string{"b", "z"}) {
		t.Errorf("read back %q", got)
	}
	if got := mountedWritable(l, "/work/b"); got != nil {
		t.Errorf("read back %q", got)
	}
}

// writableBox is a running sandbox under iso that mounts the boxApp's
// root at /work, writable as label says ("" for no label at all).
func writableBox(t *testing.T, iso, label string, configured []string) *boxApp {
	t.Helper()
	box := runningBox(iso)
	b := newBoxApp(t, iso, box)
	box.SandboxLabels[assets.LabelRoots] = `["/work"]`
	if label != "" {
		box.SandboxLabels[assets.LabelWritable] = label
	}
	box.SandboxMounts = []backend.Mount{{Source: b.tmp, Target: "/work", ReadOnly: true}}
	b.Cfg.Roots[0].Writable = configured
	return b
}

// A launch notes a restart when only what the sandbox may write changed,
// under every isolation, and when the sandbox records no writable parts;
// not when nothing changed. Doctor says the same, as a problem.
func TestWritableDrift(t *testing.T) {
	for _, iso := range isolations {
		for _, tc := range []struct {
			name, label string
			configured  []string
			want        string // "" for no note
		}{
			{"same", `{"/work":["p"]}`, []string{"p"}, ""},
			{"same, read-only", `{"/work":[]}`, nil, ""},
			{"part added", `{"/work":[]}`, []string{"p"}, "has /work read-only; config.toml now makes /work/p writable"},
			{"made read-only", `{"/work":["."]}`, nil, "has /work writable; config.toml now makes all of /work read-only"},
			{"part removed", `{"/work":["p","q"]}`, []string{"p"}, "has /work read-only except /work/p, /work/q; config.toml now makes /work/q read-only"},
			{"part moved", `{"/work":["q"]}`, []string{"p"}, "has /work read-only except /work/q; config.toml now makes /work/p writable and /work/q read-only"},
			{"whole root", `{"/work":["p"]}`, []string{"."}, "has /work read-only except /work/p; config.toml now makes all of /work writable"},
			{"whole to parts", `{"/work":["."]}`, []string{"p"}, "has /work writable; config.toml now makes /work read-only except /work/p"},
			{"no label", "", []string{"p"}, "the container records no writable parts, which a container made by this caboose always does"},
			{"no label, whole root", "", []string{"."}, "the container records no writable parts, which a container made by this caboose always does"},
		} {
			t.Run(iso+"/"+tc.name, func(t *testing.T) {
				b := writableBox(t, iso, tc.label, tc.configured)
				if err := os.MkdirAll(filepath.Join(b.tmp, "p"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := b.ensureRunning(false); err != nil {
					t.Fatalf("%v\n%s", err, b.errb)
				}
				_, errs := b.said()
				restart := "'caboose restart' applies it"
				full := tc.want
				if full != "" && !strings.HasPrefix(full, "the ") {
					full = "the running " + b.noun() + " " + tc.want
				}
				if tc.want == "" {
					if strings.Contains(errs, "the running "+b.noun()+" has") || strings.Contains(errs, "records no writable parts") || strings.Contains(errs, restart) {
						t.Errorf("noted a drift:\n%s", errs)
					}
				} else if !strings.Contains(errs, full+".\n") || !strings.Contains(errs, restart) {
					t.Errorf("no note %q:\n%s", full, errs)
				}
				c := &checkup{}
				b.doctorContainer(c, true)
				if got := c.String(); (strings.Contains(got, "running "+b.noun()+" has") || strings.Contains(got, "records no writable parts")) != (tc.want != "") || !strings.Contains(got, full) {
					t.Errorf("doctor:\n%s", got)
				}
				// A changed root is warnIfRootsDrifted's to say, not this.
				b.Cfg.Roots[0].Container = "/work/other"
				if d := b.writableDrift(); d != "" {
					t.Errorf("roots differ, and still: %s", d)
				}
			})
		}
	}
}

// A launch says, in one line, when the sandbox can write no part of any
// root; not when one has a writable part.
func TestNoteReadOnlyRoots(t *testing.T) {
	for _, iso := range isolations {
		for _, tc := range []struct {
			label      string
			configured []string
			note       bool
		}{
			{`{"/work":[]}`, nil, true},
			{`{"/work":["p"]}`, []string{"p"}, false},
			{"", nil, false}, // no label: it writes it all, until the restart
		} {
			t.Run(fmt.Sprintf("%s/%s", iso, tc.label), func(t *testing.T) {
				b := writableBox(t, iso, tc.label, tc.configured)
				if err := os.MkdirAll(filepath.Join(b.tmp, "p"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := b.ensureRunning(false); err != nil {
					t.Fatalf("%v\n%s", err, b.errb)
				}
				_, errs := b.said()
				if got := strings.Contains(errs, "every root is read-only to the "+b.noun()); got != tc.note {
					t.Errorf("noted %v:\n%s", got, errs)
				}
			})
		}
	}
}

// The relay watches only what the sandbox may write: its touch would fail
// on a read-only mount. A sandbox that writes a whole root has it relayed
// whole.
func TestWritableRelay(t *testing.T) {
	got := writableRelay([]config.Root{
		{Host: "/h/dev", Container: "/work/dev", Writable: []string{"a", "b/c"}},
		{Host: "/h/ro", Container: "/work/ro"},
		{Host: "/h/all", Container: "/work/all", Writable: []string{"."}},
	})
	want := []hostlink.Root{
		{Host: "/h/dev/a", Container: "/work/dev/a"},
		{Host: "/h/dev/b/c", Container: "/work/dev/b/c"},
		{Host: "/h/all", Container: "/work/all"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("relayed %+v", got)
	}
	// And the runner's, under gVisor, from the sandbox's own record.
	b := writableBox(t, isolationGVisor, `{"/work":["p"]}`, []string{"p"})
	r := &linkRunner{a: b.App}
	if got := r.relayRoots(); !reflect.DeepEqual(got, []hostlink.Root{{Host: b.tmp + "/p", Container: "/work/p"}}) {
		t.Errorf("runner relays %+v", got)
	}
}

// The writable parts are checked again just before the sandbox is
// created: a link planted in a part's path after the roots were resolved
// (by the sandbox being replaced, which could write it) stops the
// creation, and nothing is created.
func TestCreateRechecksWritableParts(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, &backendtest.Fake{})
			outside := t.TempDir()
			if err := os.MkdirAll(filepath.Join(b.tmp, "proj"), 0o755); err != nil {
				t.Fatal(err)
			}
			b.Cfg.Roots[0].Writable = []string{"proj/sub"}
			if err := os.Symlink(outside, filepath.Join(b.tmp, "proj", "sub")); err != nil {
				t.Fatal(err)
			}
			err := b.createContainer(false)
			if err == nil || !strings.Contains(err.Error(), "is a symbolic link") {
				t.Fatalf("err = %v", err)
			}
			if len(b.box.Specs) != 0 {
				t.Errorf("created anyway: %+v", b.box.Specs)
			}
		})
	}
}

// What a proposal is made against says what of each root is writable.
func TestExportProposalsWritable(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/p")
	e.writeConfig("[roots]\ndev = { host = \"~/dev\", writable = [\"p\"] }\nro = \"~/dev2\"\n")
	e.mkdir("dev2")
	if err := e.a.prepareDataDir(); err != nil {
		t.Fatal(err)
	}
	e.a.exportProposals()
	b, err := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
	if err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	for _, w := range []string{`[roots.dev] host = "~/dev" path = "/work/dev" writable = ["p"]`, `[roots.ro] host = "~/dev2" path = "/work/ro" writable = []`, `fails with "Read-only file system"`} {
		if !strings.Contains(squash(string(b)), w) {
			t.Errorf("state.toml lacks %q:\n%s", w, b)
		}
	}
}

// Doctor's roots row says what of each root the sandbox may write.
func TestDoctorRootsAccess(t *testing.T) {
	b := newBoxApp(t, isolationContainer, &backendtest.Fake{})
	if err := os.MkdirAll(filepath.Join(b.tmp, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.Cfg.Roots[0].Writable = []string{"p"}
	c := &checkup{}
	if !b.doctorConfig(c) {
		t.Fatalf("roots refused:\n%s", c)
	}
	if got := c.String(); !strings.Contains(got, " at /work (read-only but p)") {
		t.Errorf("doctor:\n%s", got)
	}
}

// state.toml says what the running sandbox may write when that is not
// what config.toml says, until the restart.
func TestExportProposalsMountedWritable(t *testing.T) {
	b := writableBox(t, isolationContainer, `{"/work":["."]}`, []string{"p"})
	b.Cfg.EnvDir = filepath.Join(b.tmp, "env")
	b.Cfg.File = &config.File{Roots: map[string]config.FileRoot{"w": {Host: b.tmp, Path: "/work", Writable: []string{"p"}}}}
	if err := b.prepareDataDir(); err != nil {
		t.Fatal(err)
	}
	b.exportProposals()
	got, err := os.ReadFile(filepath.Join(b.data, proposal.Dir, proposal.CurrentDir, "state.toml"))
	if err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	if !strings.Contains(squash(string(got)), `writable = ["p"] mounted_writable = ["."]`) {
		t.Errorf("state.toml:\n%s", got)
	}
}
