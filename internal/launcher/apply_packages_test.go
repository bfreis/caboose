package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// fakeBuild stands in for the image's build in apply, recording the
// packages the profile had for each build and what config.toml held then.
// A build that succeeds leaves writing its lock to apply, as an apko
// build under apply does (App.deferLock); lockConfigs are what config.toml
// held each time apply wrote one. during, when set, runs in the build.
type fakeBuild struct {
	err         error
	packages    [][]string
	configs     []string
	lockConfigs []string
	during      func()
}

func (e *setupEnv) fakeBuild(err error) *fakeBuild {
	f := &fakeBuild{err: err}
	e.a.buildImage = func() error {
		f.packages = append(f.packages, slices.Clone(e.a.Cfg.ImageProfile.Packages))
		f.configs = append(f.configs, e.configFile())
		if f.during != nil {
			f.during()
		}
		if f.err == nil {
			if !e.a.deferLock {
				e.t.Error("apply's build would write the lock itself")
			}
			e.a.pendingLock = func() error {
				f.lockConfigs = append(f.lockConfigs, e.configFile())
				return nil
			}
		}
		return f.err
	}
	return f
}

// useConfig writes config.toml and reads it as a launch would.
func (e *setupEnv) useConfig(s string) {
	e.t.Helper()
	if err := os.MkdirAll(e.a.Cfg.EnvDir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.a.Cfg.EnvDir, config.FileName), []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := e.a.rereadConfigFile(); err != nil {
		e.t.Fatal(err)
	}
	if err := e.a.Cfg.ReadImage(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *setupEnv) fileProfile(name string) config.ImageProfile {
	e.t.Helper()
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		e.t.Fatalf("%v\n%s", err, e.configFile())
	}
	p, err := f.ApkoProfile(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// Under the implicit apko.default, the packages are built first and then
// written into an [apko.default] the file did not have.
func TestApplyPackagesImplicitProfile(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	b := e.fakeBuild(nil)
	p := e.propose("dot.toml", "title = \"Add graphviz\"\nreason = \"for diagrams\"\n[packages]\nadd = [\"graphviz\"]\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(p), "dot.check"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Add graphviz", "for diagrams", "the packages of [apko.default]", "  + graphviz", "Built image",
		"Wrote the packages of [apko.default]", "Applied dot")
	if len(b.packages) != 1 || !slices.Equal(b.packages[0], []string{"graphviz"}) {
		t.Errorf("built with %q", b.packages)
	}
	if b.configs[0] != "" {
		t.Errorf("config.toml written before the build:\n%s", b.configs[0])
	}
	if got := e.fileProfile("default"); !slices.Equal(got.Packages, []string{"graphviz"}) || !got.Defaults {
		t.Errorf("profile %+v\n%s", got, e.configFile())
	}
	if !slices.Equal(e.a.Cfg.ImageProfile.Packages, []string{"graphviz"}) {
		t.Errorf("the configuration in use: %+v", e.a.Cfg.ImageProfile)
	}
	for _, f := range []string{p, strings.TrimSuffix(p, ".toml") + ".check"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", f, err)
		}
	}
	state, _ := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
	for _, want := range []string{`packages = ["graphviz"]`, "defaults = true", `"graphviz"`, `"bash"`} {
		if !strings.Contains(string(state), want) {
			t.Errorf("state.toml lacks %q:\n%s", want, state)
		}
	}
}

// Under a profile the file defines, the profile's packages change, its
// other keys and the rest of the file stay.
func TestApplyPackagesExplicitProfile(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.useConfig("# mine\nimage = \"apko.mine\"\n\n[apko.mine]\ndefaults = false\npackages = [\n  \"jq\",\n  \"yq\",\n]\n")
	b := e.fakeBuild(nil)
	e.propose("x.toml", "title = \"Swap\"\n[packages]\nadd = [\"graphviz\"]\nremove = [\"jq\"]\n")
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("  + graphviz", "  - jq")
	if len(b.packages) != 1 || !slices.Equal(b.packages[0], []string{"yq", "graphviz"}) {
		t.Errorf("built with %q", b.packages)
	}
	got := e.fileProfile("mine")
	if !slices.Equal(got.Packages, []string{"yq", "graphviz"}) || got.Defaults {
		t.Errorf("profile %+v\n%s", got, e.configFile())
	}
	if c := e.configFile(); !strings.HasPrefix(c, "# mine\nimage = \"apko.mine\"\n") || strings.Contains(c, "apko.default") {
		t.Errorf("config.toml:\n%s", c)
	}
}

// A build that fails writes nothing, and the proposal stays.
func TestApplyPackagesFailedBuildWritesNothing(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.useConfig("[apko.default]\npackages = [\"yq\"]\n")
	before := e.configFile()
	b := e.fakeBuild(errors.New("no such package graphvis"))
	p := e.propose("x.toml", "title = \"t\"\n[packages]\nadd = [\"graphvis\"]\n")
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("The build failed (no such package graphvis): nothing was applied, and the proposal is left pending", "Nothing was applied")
	if len(b.packages) != 1 {
		t.Errorf("%d builds", len(b.packages))
	}
	if c := e.configFile(); c != before {
		t.Errorf("config.toml changed:\n%s", c)
	}
	if !slices.Equal(e.a.Cfg.ImageProfile.Packages, []string{"yq"}) {
		t.Errorf("the configuration in use changed: %+v", e.a.Cfg.ImageProfile)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the proposal went: %v", err)
	}
}

// What cannot be applied is refused before anything is asked or built.
func TestApplyPackagesRefusals(t *testing.T) {
	const optional = "ripgrep" // in the core group, a default one
	for _, tc := range []struct {
		name, body string
		image      *config.ImageProfile
		want       []string
	}{
		{"required", "add = [\"bash\"]", nil, []string{"Already installed: bash (caboose's packages)."}},
		{"default group", "add = [\"" + optional + "\"]", nil, []string{"Already installed: " + optional + " (caboose's packages)."}},
		{"own", "add = [\"yq\"]", nil, []string{"Already installed: yq (this profile's packages)."}},
		{"remove required", "remove = [\"bash\"]", nil, []string{"bash cannot be removed: the sandbox requires it."}},
		{"remove a group's", "remove = [\"" + optional + "\"]", nil, []string{optional + " is one of caboose's packages, not this profile's own: to drop it, run 'caboose setup image' and choose package groups."}},
		{"remove unknown", "remove = [\"nothere\"]", nil, []string{"nothere is not in this profile's packages (apko.default has: yq)."}},
		{"dockerfile", "add = [\"x\"]", &config.ImageProfile{Kind: config.ImageKindDockerfile, Name: "default"},
			[]string{"This environment's image is dockerfile.default, built from a Dockerfile: packages apply to an apko image only, and under a dockerfile image a [section] is what applies."}},
		{"ref", "add = [\"x\"]", &config.ImageProfile{Kind: config.ImageKindRef, Name: "mine", Ref: "debian:13"},
			[]string{"This environment's image is ref.mine, an image of the user's own: packages apply to an apko image only, and nothing can be proposed for a ref image."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSetupEnv(t, "default", "", false)
			e.useConfig("[apko.default]\npackages = [\"yq\"]\n")
			if tc.image != nil {
				e.a.Cfg.ImageProfile = *tc.image
			}
			before := e.configFile()
			b := e.fakeBuild(nil)
			p := e.propose("x.toml", "title = \"t\"\n[packages]\n"+tc.body+"\n")
			if err := e.apply("2\n"); err != nil {
				t.Fatalf("%v\n%s", err, e.errb)
			}
			e.wantOut(append(tc.want, "Nothing in it can be applied", "Left pending")...)
			if len(b.packages) != 0 || e.configFile() != before {
				t.Errorf("built %q, config.toml:\n%s", b.packages, e.configFile())
			}
			if _, err := os.Stat(p); err != nil {
				t.Errorf("left pending, yet gone: %v", err)
			}
		})
	}
}

// Removing a package of the profile's own that caboose's groups also have
// is allowed, with a word that it stays installed.
func TestApplyPackagesRemoveWarnsWhenItStays(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.useConfig("[apko.default]\npackages = [\"jq\", \"yq\"]\n")
	e.fakeBuild(nil)
	e.propose("x.toml", "title = \"t\"\n[packages]\nremove = [\"jq\"]\n")
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("jq stays installed all the same", "Applied x")
	if got := e.fileProfile("default"); !slices.Equal(got.Packages, []string{"yq"}) {
		t.Errorf("profile %+v", got)
	}
}

// Each proposal is planned against config.toml as the one before left it.
func TestApplyPackagesTwoInARow(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	b := e.fakeBuild(nil)
	e.propose("a.toml", "title = \"A\"\n[packages]\nadd = [\"graphviz\"]\n")
	e.propose("b.toml", "title = \"B\"\n[packages]\nadd = [\"yq\"]\n")
	c := e.propose("c.toml", "title = \"C\"\n[packages]\nadd = [\"graphviz\"]\n")
	if err := e.apply("1\n1\n2\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Applied a", "Applied b", "Already installed: graphviz (this profile's packages).")
	if len(b.packages) != 2 || !slices.Equal(b.packages[1], []string{"graphviz", "yq"}) {
		t.Errorf("built with %q", b.packages)
	}
	if got := e.fileProfile("default"); !slices.Equal(got.Packages, []string{"graphviz", "yq"}) {
		t.Errorf("profile %+v\n%s", got, e.configFile())
	}
	if _, err := os.Stat(c); err != nil {
		t.Errorf("c went: %v", err)
	}
}

// Packages and a root in one proposal: the root's name is typed, the
// build runs, and both are written together.
func TestApplyPackagesWithARoot(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.dir("src/other")
	b := e.fakeBuild(nil)
	e.propose("x.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n[roots]\nother = \"~/src/other\"\n")
	if err := e.apply("1\nother\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Wrote the roots and the packages of [apko.default]")
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	if f.Roots["other"].Host != "~/src/other" || len(b.packages) != 1 {
		t.Errorf("roots %v, builds %q", f.Roots, b.packages)
	}
	if got := e.fileProfile("default"); !slices.Equal(got.Packages, []string{"graphviz"}) {
		t.Errorf("profile %+v", got)
	}
}

// editConfigFile rewrites config.toml as the user would, in an editor.
func (e *setupEnv) editConfigFile(data string) {
	e.t.Helper()
	if err := os.WriteFile(e.configPath(), []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// The lock the build resolved is written only once config.toml holds the
// packages it is of.
func TestApplyPackagesWritesTheLockAfterTheConfig(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.useConfig("[apko.default]\npackages = [\"yq\"]\n")
	b := e.fakeBuild(nil)
	e.propose("x.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n")
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if len(b.lockConfigs) != 1 || !strings.Contains(b.lockConfigs[0], "graphviz") {
		t.Errorf("lock written with config.toml %q", b.lockConfigs)
	}
	if e.a.deferLock || e.a.pendingLock != nil {
		t.Error("apply left the lock deferred")
	}
}

// A profile changed in config.toml while the image was built is not
// overwritten: the proposal stays, and neither config.toml nor the lock
// is written.
func TestApplyPackagesRefusesAProfileChangedMeanwhile(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.useConfig("[apko.default]\npackages = [\"yq\"]\n")
	b := e.fakeBuild(nil)
	edited := "[apko.default]\npackages = [\"yq\", \"jq\"]\n"
	b.during = func() { e.editConfigFile(edited) }
	p := e.propose("x.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n")
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("[apko.default] in", "changed while the image was built: nothing was written, and the proposal is left pending")
	if c := e.configFile(); c != edited {
		t.Errorf("config.toml:\n%s", c)
	}
	if len(b.lockConfigs) != 0 {
		t.Errorf("lock written: %q", b.lockConfigs)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the proposal went: %v", err)
	}

	// defaults changed is a change too.
	e.useConfig("[apko.default]\npackages = [\"yq\"]\n")
	b.during = func() { e.editConfigFile("[apko.default]\npackages = [\"yq\"]\ndefaults = false\n") }
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("changed while the image was built")
	if len(b.lockConfigs) != 0 {
		t.Errorf("lock written: %q", b.lockConfigs)
	}
}
