package launcher

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// propose writes a proposal as a session would.
func (e *setupEnv) propose(name, data string) string {
	e.t.Helper()
	dir := filepath.Join(e.a.Cfg.DataDir, proposal.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// apply runs caboose apply, answering with answers.
func (e *setupEnv) apply(answers string) error {
	e.t.Helper()
	e.errb.Reset()
	e.a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(answers)), nil }
	return e.a.Apply()
}

func (e *setupEnv) configFile() string {
	b, _ := os.ReadFile(filepath.Join(e.a.Cfg.EnvDir, config.FileName))
	return string(b)
}

// dir makes rel under the home, and returns its path.
func (e *setupEnv) dir(rel string) string {
	e.t.Helper()
	e.mkdir(rel)
	return filepath.Join(e.a.Cfg.Home, rel)
}

func presetHash(t *testing.T) string {
	t.Helper()
	b, err := assets.Preset(assets.DefaultSections())
	if err != nil {
		t.Fatal(err)
	}
	return proposal.Hash(b)
}

const fooSection = `
[section]
name = "foo"
title = "foo 2.3"
body = '''
ARG FOO_VERSION=2.3.0
RUN curl -fsSL https://example.invalid/foo.tgz | tar -xz -C /usr/local/bin foo
'''
`

func TestApplyNothingPending(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.a.Terminal = func() (io.ReadCloser, error) { return nil, errors.New("no terminal") }
	if err := e.a.Apply(); err != nil {
		t.Fatal(err)
	}
	e.wantOut("no pending proposals")
}

func TestApplyNeedsATerminal(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	p := e.propose("foo.toml", "title = \"t\"\n[roots]\nfoo = \"~/src/foo\"\n")
	e.a.Terminal = func() (io.ReadCloser, error) { return nil, errors.New("no terminal") }
	if err := e.a.Apply(); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the proposal went: %v", err)
	}
}

// A section, on an environment with no Dockerfile of its own: the preset
// is written with the section, which apply says first, and the proposal is
// gone.
func TestApplySection(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	p := e.propose("foo.toml", "title = \"Install foo\"\nreason = \"for the docs\"\ndockerfile_sha256 = \""+presetHash(t)+"\"\n"+
		fooSection+"\n")
	// Apply it; don't build.
	if err := e.apply("1\nn\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Install foo", "for the docs", "caboose's preset", "+ # caboose:section foo foo 2.3",
		"no longer reach it", "Applied foo", "Not built")
	df, err := os.ReadFile(filepath.Join(e.a.Cfg.EnvDir, "image", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(df), "# caboose:section foo foo 2.3\nARG FOO_VERSION=2.3.0\n") {
		t.Errorf("Dockerfile:\n%s", df)
	}
	if h, ok := assets.ReadHeader(df); !ok || h.ID != assets.PresetID() {
		t.Errorf("the preset's header is gone: %+v", h)
	}
	if c := e.configFile(); c != "" {
		t.Errorf("config.toml written:\n%s", c)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the proposal is still there: %v", err)
	}
	// Sessions now see the new Dockerfile, and its hash.
	state, _ := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
	for _, want := range []string{`dockerfile = "image/Dockerfile"`, proposal.Hash(df)} {
		if !strings.Contains(string(state), want) {
			t.Errorf("state.toml lacks %q:\n%s", want, state)
		}
	}
	cur, _ := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "Dockerfile"))
	if string(cur) != string(df) {
		t.Error("current/Dockerfile is not image/Dockerfile")
	}
}

// A section written against another Dockerfile is refused, whatever the
// answer; it can be left pending, or deleted.
func TestApplyRefusesAStaleSection(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	p := e.propose("foo.toml", "title = \"Install foo\"\ndockerfile_sha256 = \""+proposal.Hash([]byte("FROM old\n"))+"\"\n"+fooSection)
	if err := e.apply("2\n"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("The Dockerfile has changed since this was proposed")
	if _, err := os.Stat(p); err != nil {
		t.Errorf("left pending, yet gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.a.Cfg.EnvDir, "image")); !os.IsNotExist(err) {
		t.Errorf("image/ was written: %v", err)
	}
	if err := e.apply("1\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("deleted, yet there: %v", err)
	}
}

// On a base image there is no Dockerfile to put a section in.
func TestApplyRefusesASectionOnABaseImage(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.a.Cfg.BaseImage = "debian:13"
	e.propose("foo.toml", "title = \"Install foo\"\ndockerfile_sha256 = \""+presetHash(t)+"\"\n"+fooSection)
	if err := e.apply("2\n"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("builds on base_image debian:13")
}

// A root: the single root there is gets a name, the move is said and
// confirmed, and the root is added only once its name is typed.
func TestApplyRoot(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	other := e.dir("src/other")
	if err := os.WriteFile(filepath.Join(other, ".env"), []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := e.propose("other.toml", "title = \"Mount other\"\n[roots]\nother = \"~/src/other\"\n")

	// Apply; name the root there is "dev" (Enter); go ahead with the move;
	// type the name wrong.
	if err := e.apply("1\n\ny\nothre\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("/work/other", other, "readable and writable from the sandbox", "credentials or keys: .env",
		"move from /work/... to /work/dev/...", "That is not other")
	if c := e.configFile(); c != "" {
		t.Errorf("config.toml written on a wrong name:\n%s", c)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the proposal went: %v", err)
	}

	// Enter alone leaves a root pending.
	if err := e.apply("\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.errb.String(), "Left pending") || e.configFile() != "" {
		t.Errorf("Enter applied a root:\n%s", e.errb)
	}

	if err := e.apply("1\n\ny\nother\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	if f.Roots["dev"] != "~/dev" || f.Roots["other"] != "~/src/other" || len(f.Roots) != 2 || f.Vals["REPO_ROOT"] != "" {
		t.Errorf("roots %v, repo_root %q", f.Roots, f.Vals["REPO_ROOT"])
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the proposal is still there: %v", err)
	}
}

// A root through a symlink is judged, and written, as where it leads.
func TestApplyRootThroughASymlink(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	real := e.dir("src/real")
	if err := os.Symlink(real, filepath.Join(e.a.Cfg.Home, "link")); err != nil {
		t.Fatal(err)
	}
	e.a.Cfg.File = &config.File{Roots: map[string]string{"dev": "~/dev"}}
	e.propose("x.toml", "title = \"t\"\n[roots]\nx = \"~/link\"\n")
	if err := e.apply("1\nx\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	if f.Roots["x"] != "~/src/real" {
		t.Errorf("roots %v", f.Roots)
	}
}

func TestProposedRootRefusals(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	e.mkdir(".ssh/keys")
	e.mkdir("Library/Keychains")
	e.mkdir("dev/sub")
	e.mkdir("ok")
	if err := os.MkdirAll(e.a.Cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, filepath.Join(home, "ok", "to-home")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(home, "ok", "to-ssh")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, want string }{
		{"x", "/", "the whole disk"},
		{"x", "~", "your home directory"},
		{"x", filepath.Dir(home), "your home directory"},
		{"x", "~/ok/to-home", "your home directory"},
		{"x", "~/.ssh", "in ~/.ssh"},
		{"x", "~/.ssh/keys", "in ~/.ssh"},
		{"x", "~/ok/to-ssh", "in ~/.ssh"},
		{"x", "~/Library/Keychains", "in ~/Library"},
		{"x", e.a.Cfg.DataDir, "overlaps caboose's home"},
		{"x", e.a.Cfg.CabooseHome, "overlaps caboose's home"},
		{"x", "~/dev/sub", "overlaps the root ~/dev"},
		{"dev", "~/ok", ""}, // the root there is has no name: only a path
		{"x", "~/nope", "does not exist"},
		{"x", "relative", "not an absolute path"},
	} {
		_, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: tc.name, Path: tc.path})
		got := strings.Join(refusals, "\n")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusals %q, want %q", tc.path, got, tc.want)
		}
	}
	// A name one of several roots has is taken.
	e.a.Cfg.File = &config.File{Roots: map[string]string{"dev": "~/dev"}}
	if _, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "dev", Path: "~/ok"}); len(refusals) != 1 || !strings.Contains(refusals[0], "named dev already") {
		t.Errorf("refusals %q", refusals)
	}
}

// What the sandbox wrote is never put on the terminal as it is: a file's
// name, or an error quoting the file, has its control characters escaped.
func TestApplyEscapesWhatTheSandboxWrote(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.propose("x\x1b[2J.toml", "junk")
	e.propose("y.toml", "title = \"t\"\n[\"\\u001b[1Aroots\"]\nx = \"/x\"\n")
	if err := e.apply("2\n2\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	out := e.errb.String()
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b[1A") {
		t.Errorf("an escape sequence reached the terminal:\n%q", out)
	}
	if !strings.Contains(out, `x\u001B[2J`) || !strings.Contains(out, `unknown key`) || !strings.Contains(out, `1Aroots`) {
		t.Errorf("not shown escaped:\n%s", out)
	}
}

// A launch says what is pending, and writes what sessions propose against.
func TestLaunchNotesPendingProposals(t *testing.T) {
	e := newSetupEnv(t, "work", "", false)
	e.propose("foo.toml", "title = \"t\"\n")
	e.propose("bar.toml", "title = \"t\"\n")
	e.a.exportProposals()
	e.a.notePendingProposals()
	e.wantOut("2 pending proposals (bar, foo): 'caboose -e work apply' reviews them")
	state, err := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`dockerfile = "preset"`, presetHash(t), `repo_root = "~/dev"`} {
		if !strings.Contains(string(state), want) {
			t.Errorf("state.toml lacks %q:\n%s", want, state)
		}
	}
}
