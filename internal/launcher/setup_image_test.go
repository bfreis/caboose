package launcher

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
)

func (e *setupEnv) imageDir() string { return config.DockerfileDir(e.a.Cfg.EnvDir, "default") }

func (e *setupEnv) dockerfile() string {
	b, err := os.ReadFile(filepath.Join(e.imageDir(), "Dockerfile"))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// writeDockerfile gives the environment a dockerfile/default/Dockerfile.
func (e *setupEnv) writeDockerfile(data string) {
	e.t.Helper()
	if err := os.MkdirAll(e.imageDir(), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.imageDir(), "Dockerfile"), []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func seed(t *testing.T) string {
	t.Helper()
	b, err := assets.Seed(assets.DefaultSections())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// groupPackages are the packages of the named groups, in the order setup
// writes them.
func groupPackages(names ...string) []any {
	out := []any{}
	for _, g := range apkobuild.Groups() {
		if slices.Contains(names, g.Name) {
			for _, p := range g.Packages {
				out = append(out, p)
			}
		}
	}
	return out
}

// With nothing configured, the image is caboose's packages, and the
// default is to keep it: a re-run at its defaults writes nothing and
// builds nothing.
func TestSetupImageKeepsTheDefault(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Now: apko.default (caboose's packages).", "1 caboose's packages (recommended)\n",
		"4 An image of your own\n", "choose 1-4 [1]", "· Nothing changed")
	if b, _ := os.ReadFile(e.configPath()); string(b) != config.Template {
		t.Errorf("config.toml:\n%s", b)
	}
	if e.said("Build the image now") {
		t.Errorf("offered a build:\n%s", e.errb)
	}
}

// Package groups, chosen one by one: written as the profile's packages,
// without the defaults; offered again as they were; and given up for
// caboose's packages, which takes them out again.
func TestSetupImageChoosesGroups(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	// core out, rust in; then no build.
	if err := e.run("2\n1 9\n\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("? What goes into the image?", "1 [x] Command-line basics", "9 [ ] Rust (rustup)",
		"1 [ ] Command-line basics", "9 [x] Rust (rustup)",
		`✓ Wrote image = "apko.default" to `+e.configPath(), "? Build the image now? It takes a few minutes. [Y/n] ")
	f := e.file()
	want := groupPackages("node", "bun", "go", "gh", "docker", "dockerd", "sudo", "rust")
	if f.Vals["image"] != "apko.default" || f.Vals["apko.default.defaults"] != false ||
		!slices.Equal(f.Vals["apko.default.packages"].([]any), want) {
		t.Errorf("config.toml: %+v", f.Vals)
	}
	if p := e.a.Cfg.ImageProfile; p.String() != "apko.default" || p.Defaults || len(p.Packages) != len(want) {
		t.Errorf("profile %+v", p)
	}

	// Offered as written: the same answers change nothing.
	if err := e.run("\n\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("choose 1-4 [2]", "1 [ ] Command-line basics", "9 [x] Rust (rustup)", "· Nothing changed")

	// A package of the user's own stays through both.
	e.writeConfig(strings.Replace(e.configFile(), `packages = ["`, `packages = ["postgresql-17-client", "`, 1))
	if err := e.run("1\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Your own packages in [apko.default] stay: postgresql-17-client.")
	f = e.file()
	if f.Has("apko.default.defaults") || !slices.Equal(f.Vals["apko.default.packages"].([]any), []any{"postgresql-17-client"}) {
		t.Errorf("config.toml: %+v", f.Vals)
	}
	if p := e.a.Cfg.ImageProfile; !p.Defaults || !slices.Equal(p.Packages, []string{"postgresql-17-client"}) {
		t.Errorf("profile %+v", p)
	}
}

// An apko profile not in use keeps what it says when it is chosen again:
// its own packages stay, through either apko choice, and the groups it
// lists are the ones ticked to start with.
func TestSetupImageKeepsAnInactiveApkoProfile(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeDockerfile("FROM mine\n")
	inactive := func(apko string) {
		e.t.Helper()
		e.writeConfig("format = 1\nimage = \"dockerfile.default\"\n[dockerfile.default]\n[apko.default]\n" + apko)
		if err := e.a.rereadConfigFile(); err != nil {
			t.Fatal(err)
		}
		if err := e.a.Cfg.ReadImage(); err != nil || e.a.Cfg.ImageProfile.Kind != config.ImageKindDockerfile {
			t.Fatalf("profile %+v, %v", e.a.Cfg.ImageProfile, err)
		}
	}

	// Package groups, at the default ticks: the profile's own package stays.
	inactive("packages = [\"postgresql-17-client\"]\n")
	if err := e.run("2\n\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	want := append(groupPackages("core", "node", "bun", "go", "gh", "docker", "dockerd", "sudo"), "postgresql-17-client")
	if got, _ := e.file().Vals["apko.default.packages"].([]any); !slices.Equal(got, want) {
		t.Errorf("packages %v, want %v", got, want)
	}

	// caboose's packages: the same.
	inactive("packages = [\"postgresql-17-client\"]\n")
	if err := e.run("1\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Your own packages in [apko.default] stay: postgresql-17-client.")
	if p := e.a.Cfg.ImageProfile; p.String() != "apko.default" || !p.Defaults || !slices.Equal(p.Packages, []string{"postgresql-17-client"}) {
		t.Errorf("profile %+v", p)
	}

	// Its groups, without the defaults, are what the checklist starts from.
	rust := groupPackages("rust")
	list := []string{}
	for _, p := range rust {
		list = append(list, `"`+p.(string)+`"`)
	}
	inactive("defaults = false\npackages = [" + strings.Join(list, ", ") + "]\n")
	if err := e.run("2\n\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("1 [ ] Command-line basics", "9 [x] Rust (rustup)")
	if got, _ := e.file().Vals["apko.default.packages"].([]any); !slices.Equal(got, rust) {
		t.Errorf("packages %v, want %v", got, rust)
	}
}

// A Dockerfile of the user's: the profile is written, and its dir seeded
// with caboose's Dockerfile when it has none -- never over one.
func TestSetupImageDockerfile(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("3\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	f := e.file()
	if f.Vals["image"] != "dockerfile.default" || !slices.Contains(f.ImageProfiles, "dockerfile.default") {
		t.Errorf("config.toml: %+v %v", f.Vals, f.ImageProfiles)
	}
	if got := e.dockerfile(); got != seed(t) {
		t.Errorf("Dockerfile:\n%s", got)
	}
	if p := e.a.Cfg.ImageProfile; p.Kind != config.ImageKindDockerfile || p.Dir != e.imageDir() {
		t.Errorf("profile %+v", p)
	}
	e.wantOut("✓ Wrote "+filepath.Join(e.imageDir(), "Dockerfile")+"; it is yours to edit from here on",
		"· Not built: the next launch that creates the container builds it, or caboose build now")

	// Edited, it stays; choosing it again changes nothing.
	e.writeDockerfile("FROM mine\n")
	if err := e.run("\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Now: dockerfile.default ("+e.imageDir()+").", "choose 1-4 [3]", "is there already, and stays as it is", "· Nothing changed")
	if e.dockerfile() != "FROM mine\n" {
		t.Error("the Dockerfile was replaced")
	}
}

// Switching to a Dockerfile whose dir has one already keeps it.
func TestSetupImageDockerfileKeepsOne(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeDockerfile("FROM mine\n")
	if err := e.run("3\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if e.dockerfile() != "FROM mine\n" || e.file().Vals["image"] != "dockerfile.default" {
		t.Errorf("Dockerfile %q, config %+v", e.dockerfile(), e.file().Vals)
	}
	e.wantOut("is there already, and stays as it is: it is yours.", "? Build the image now?")
}

// An image of the user's own: asked for until it is one, and written as a
// ref profile, which a profile of that kind keeps the name of.
func TestSetupImageRef(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("4\n\ncaboose:default\nnode:22\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("✗ Name an image", "is the environment's own image")
	f := e.file()
	if f.Vals["image"] != "ref.default" || f.Vals["ref.default.image"] != "node:22" {
		t.Errorf("config.toml: %+v", f.Vals)
	}
	if p := e.a.Cfg.ImageProfile; p.Kind != config.ImageKindRef || p.Ref != "node:22" {
		t.Errorf("profile %+v", p)
	}

	e.writeConfig("image = \"ref.mine\"\n[ref.mine]\nimage = \"node:22\"\n")
	if err := e.run("\n\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Now: ref.mine (node:22).", "choose 1-4 [4]", "[node:22]", "· Nothing changed")
	if err := e.run("\ndebian:13\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if f := e.file(); f.Vals["ref.mine.image"] != "debian:13" || f.Has("ref.default.image") {
		t.Errorf("config.toml: %+v", f.Vals)
	}
}

// A build that fails is said, and setup goes on; with a container, the
// restart that moves it onto a new image is left to the user.
func TestSetupImageBuildFails(t *testing.T) {
	e := newSetupEnv(t, "default", "", true)
	if err := e.run("3\n\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("caboose: building the base image 'caboose-base:default' from "+filepath.Join(e.imageDir(), "Dockerfile"),
		"✗ The build failed (above). Fix what it says, then run caboose build.")
}

func TestSetupImageBuildDeclinedWithAContainer(t *testing.T) {
	e := newSetupEnv(t, "default", "", true)
	if err := e.run("3\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("! The container keeps the image it was created from: caboose restart moves it onto the new one, and ends running sessions.")
}

func TestLineDiff(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	b := "1\n2\nthree\n4\n5\n6\n7\n8\n9\n10\neleven\n"
	want := "  1\n  2\n- 3\n+ three\n  4\n  5\n...\n  9\n  10\n+ eleven\n"
	if got := lineDiff(a, b); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := lineDiff("x\n", "x\n"); got != "" {
		t.Errorf("same: %q", got)
	}
}
