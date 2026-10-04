package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
)

func (e *setupEnv) imageDir() string { return filepath.Join(e.a.Cfg.EnvDir, "image") }

func (e *setupEnv) dockerfile() string {
	b, err := os.ReadFile(filepath.Join(e.imageDir(), "Dockerfile"))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// writeDockerfile gives the environment an image/Dockerfile, as a launch
// would find it.
func (e *setupEnv) writeDockerfile(data string) {
	e.t.Helper()
	if err := os.MkdirAll(e.imageDir(), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.imageDir(), "Dockerfile"), []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.a.Cfg.ImageDir = e.imageDir()
}

func preset(t *testing.T, names ...string) string {
	t.Helper()
	b, err := assets.Preset(names)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// With no image dir, the default is to keep it that way: a re-run at its
// defaults writes nothing and builds nothing.
func TestSetupImageKeepsTheEmbedded(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Now: the Dockerfile built into caboose, which is Ubuntu with • Node.js",
		"1 Keep building from the Dockerfile built into caboose\n", "· Nothing changed")
	if _, err := os.Stat(e.imageDir()); err == nil {
		t.Error("wrote an image dir")
	}
	if e.said("Build the image now") {
		t.Errorf("offered a build:\n%s", e.errb)
	}
}

func TestSetupImageWritesTheDefault(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("2\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if got := e.dockerfile(); got != preset(t, assets.DefaultSections()...) {
		t.Errorf("Dockerfile:\n%s", got)
	}
	if e.a.Cfg.ImageDir != e.imageDir() {
		t.Errorf("ImageDir %q", e.a.Cfg.ImageDir)
	}
	e.wantOut("✓ Wrote "+filepath.Join(e.imageDir(), "Dockerfile")+"; it is yours to edit from here on",
		"? Build the image now? It takes a few minutes. [Y/n] ",
		"· Not built: the next launch that creates the container builds it, or caboose build now")
}

// Chosen section by section: the defaults offered, an off one taken.
func TestSetupImageChooses(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	// node and gh out, rust in (a number that is none asks again); then
	// no build.
	if err := e.run("3\n1 4 12\n1 4,9\n\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if got := e.dockerfile(); got != preset(t, "bun", "go", "jj", "docker", "dockerd", "sudo", "rust") {
		t.Errorf("Dockerfile:\n%s", got)
	}
	e.wantOut("? What else goes into the image?",
		"1 [x] Node.js, npm and corepack (for npx-launched MCP servers)",
		"9 [ ] Rust (rustup, with the stable toolchain)",
		"1 [ ] Node.js, npm and corepack (for npx-launched MCP servers)",
		"9 [x] Rust (rustup, with the stable toolchain)")
}

// An image dir is kept by default; replacing it shows the difference from
// the fresh preset, starting from the sections it was written with, and
// asks.
func TestSetupImageReplaces(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	mine := preset(t, "go", "gh") + "RUN echo mine\n"
	e.writeDockerfile(mine)
	if err := e.run("\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Now: "+filepath.Join(e.imageDir(), "Dockerfile")+", written from caboose's preset "+assets.PresetID()+", with • Go • The GitHub CLI (gh)\n",
		"1 Keep it as it is\n", "· Nothing changed")
	if e.said("has changed since") {
		t.Errorf("stderr:\n%s", e.errb)
	}

	// Replace, sections as they were, then decline.
	if err := e.run("2\n\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("2 [ ] Bun", "3 [x] Go", "  - RUN echo mine\n", "? Replace "+filepath.Join(e.imageDir(), "Dockerfile")+" with it? [y/N] ")
	if e.dockerfile() != mine {
		t.Error("replaced, though declined")
	}

	// And accept, keeping the other files in the dir.
	if err := os.WriteFile(filepath.Join(e.imageDir(), "extra.sh"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.run("2\n\ny\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if got := e.dockerfile(); got != preset(t, "go", "gh") {
		t.Errorf("Dockerfile:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(e.imageDir(), "extra.sh")); err != nil {
		t.Error("another file in the dir went")
	}

	// The same again: nothing to replace.
	if err := e.run("2\n\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("· The fresh preset is what " + filepath.Join(e.imageDir(), "Dockerfile") + " has already; nothing changed")
}

func TestSetupImageOldOrNoPreset(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeDockerfile("# caboose:preset 000000000000 node\nFROM x\n")
	if err := e.run("\n", "image"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("with • Node.js", "! caboose's preset has changed since: it is "+assets.PresetID()+" now.\n")

	e.writeDockerfile("FROM mine\n")
	// replace: the defaults are the preset's own, as for no header.
	if err := e.run("2\n\nn\n", "image"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("Now: "+filepath.Join(e.imageDir(), "Dockerfile")+", your own (it names no preset).\n",
		"2 [x] Bun", "  - FROM mine\n")
}

// CABOOSE_BASE_IMAGE: from config.toml it can be kept, or given up for
// caboose's own image, which removes it from the file; from the shell it
// is only said.
func TestSetupImageFromBaseImage(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("base_image = \"node:22\"\n")
	e.a.Cfg.BaseImage = "node:22"
	if err := e.run("\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Now: your own image, 'node:22' (CABOOSE_BASE_IMAGE).\n", "? Keep building on 'node:22'? [Y/n] ", "· Nothing changed")

	// Give it up for the default image, written into the dir; no build.
	if err := e.run("n\n2\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if f := e.file(); f.Vals["BASE_IMAGE"] != "" {
		t.Errorf("base_image still set: %+v", f.Vals)
	}
	if e.a.Cfg.BaseImage != "" || e.dockerfile() != preset(t, assets.DefaultSections()...) {
		t.Errorf("BaseImage %q, Dockerfile:\n%s", e.a.Cfg.BaseImage, e.dockerfile())
	}
	e.wantOut("1 Build from the Dockerfile built into caboose\n", "✓ Removed base_image from "+e.configPath())
}

func TestSetupImageBaseImageToEmbedded(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("base_image = \"node:22\"\n")
	e.a.Cfg.BaseImage = "node:22"
	if err := e.run("n\n\nn\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if f := e.file(); f.Vals["BASE_IMAGE"] != "" {
		t.Errorf("base_image still set: %+v", f.Vals)
	}
	if _, err := os.Stat(e.imageDir()); err == nil {
		t.Error("wrote an image dir")
	}
	e.wantOut("? Build the image now")
}

func TestSetupImageBaseImageFromTheShell(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.a.Cfg.BaseImage = "node:22"
	e.a.Cfg.Getenv = func(k string) string {
		if k == "CABOOSE_BASE_IMAGE" {
			return "node:22"
		}
		return ""
	}
	if err := e.run("", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("! CABOOSE_BASE_IMAGE is set in this shell; unset it to build on anything else.", "· Nothing changed")
}

func TestSetupImageTwoBases(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeDockerfile("FROM x\n")
	e.a.Cfg.BaseImage = "node:22"
	if err := e.run("", "image"); err == nil || !strings.Contains(err.Error(), "Keep one") {
		t.Errorf("err = %v", err)
	}
}

// A build that fails is said, and setup goes on; with a container, the
// restart that moves it onto a new image is left to the user.
func TestSetupImageBuildFails(t *testing.T) {
	e := newSetupEnv(t, "default", "", true)
	if err := e.run("2\n\n", "image"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("caboose: building the base image 'caboose-base' from "+filepath.Join(e.imageDir(), "Dockerfile"),
		"✗ The build failed (above). Fix what it says, then run caboose build.")
}

func TestSetupImageBuildDeclinedWithAContainer(t *testing.T) {
	e := newSetupEnv(t, "default", "", true)
	if err := e.run("2\nn\n", "image"); err != nil {
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
