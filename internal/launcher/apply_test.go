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

// useDockerfile makes the environment's image a dockerfile profile, at
// its default dir, as Load would read [dockerfile.default].
func (e *setupEnv) useDockerfile() {
	e.a.Cfg.ImageProfile = config.ImageProfile{Kind: config.ImageKindDockerfile, Name: "default",
		Dir: config.DockerfileDir(e.a.Cfg.EnvDir, "default")}
}

func seedHash(t *testing.T) string {
	t.Helper()
	b, err := assets.Seed(assets.DefaultSections())
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

// A section, on a dockerfile profile whose dir has no Dockerfile yet:
// caboose's seed is written with the section, which apply says first, and
// the proposal is gone.
func TestApplySection(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.useDockerfile()
	p := e.propose("foo.toml", "title = \"Install foo\"\nreason = \"for the docs\"\ndockerfile_sha256 = \""+seedHash(t)+"\"\n"+
		fooSection+"\n")
	// Apply it; don't build.
	if err := e.apply("1\nn\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Install foo", "for the docs", "caboose's Dockerfile", "+ # caboose:section foo foo 2.3",
		"has no Dockerfile yet", "Applied foo", "Not built")
	df, err := os.ReadFile(filepath.Join(config.DockerfileDir(e.a.Cfg.EnvDir, "default"), "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(df), "# caboose:section foo foo 2.3\nARG FOO_VERSION=2.3.0\n") {
		t.Errorf("Dockerfile:\n%s", df)
	}
	if !strings.HasPrefix(string(df), "# Written by caboose from the Dockerfile it embeds.") {
		t.Errorf("not the seed:\n%s", df[:200])
	}
	if c := e.configFile(); c != "" {
		t.Errorf("config.toml written:\n%s", c)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the proposal is still there: %v", err)
	}
	// Sessions now see the new Dockerfile, and its hash.
	state, _ := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
	for _, want := range []string{`image = "dockerfile.default"`, `dockerfile = "dockerfile"`, proposal.Hash(df)} {
		if !strings.Contains(string(state), want) {
			t.Errorf("state.toml lacks %q:\n%s", want, state)
		}
	}
	cur, _ := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "Dockerfile"))
	if string(cur) != string(df) {
		t.Error("current/Dockerfile is not the profile's Dockerfile")
	}
}

// A section written against another Dockerfile is refused, whatever the
// answer; it can be left pending, or deleted.
func TestApplyRefusesAStaleSection(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.useDockerfile()
	p := e.propose("foo.toml", "title = \"Install foo\"\ndockerfile_sha256 = \""+proposal.Hash([]byte("FROM old\n"))+"\"\n"+fooSection)
	if err := e.apply("2\n"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("The Dockerfile has changed since this was proposed")
	if _, err := os.Stat(p); err != nil {
		t.Errorf("left pending, yet gone: %v", err)
	}
	if _, err := os.Stat(config.DockerfileDir(e.a.Cfg.EnvDir, "default")); !os.IsNotExist(err) {
		t.Errorf("the build context was written: %v", err)
	}
	if err := e.apply("1\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("deleted, yet there: %v", err)
	}
}

// Under apko or a ref there is no Dockerfile to put a section in, and
// sessions are told there is none.
func TestApplyRefusesASectionWithoutADockerfile(t *testing.T) {
	for _, p := range []config.ImageProfile{
		config.DefaultImageProfile(),
		{Kind: config.ImageKindRef, Name: "mine", Ref: "debian:13"},
	} {
		e := newSetupEnv(t, "default", "", false)
		e.a.Cfg.ImageProfile = p
		prop := e.propose("foo.toml", "title = \"Install foo\"\ndockerfile_sha256 = \""+seedHash(t)+"\"\n"+fooSection)
		if err := e.apply("2\n"); err != nil {
			t.Fatal(err)
		}
		e.wantOut("This environment's image is " + p.String() + ", which has no Dockerfile: a section applies to a dockerfile image profile only.")
		if _, err := os.Stat(prop); err != nil {
			t.Errorf("%s: left pending, yet gone: %v", p, err)
		}
		if _, err := os.Stat(config.DockerfileDir(e.a.Cfg.EnvDir, "default")); !os.IsNotExist(err) {
			t.Errorf("%s: the build context was written: %v", p, err)
		}
		state, _ := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
		if !strings.Contains(string(state), `image = "`+p.String()+`"`) || strings.Contains(string(state), "\ndockerfile") {
			t.Errorf("%s: state.toml:\n%s", p, state)
		}
	}
}

// A root is added only once its name is typed.
func TestApplyRoot(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	other := e.dir("src/other")
	if err := os.WriteFile(filepath.Join(other, ".env"), []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := e.propose("other.toml", "title = \"Mount other\"\n[roots]\nother = \"~/src/other\"\n")

	// Apply; type the name wrong.
	if err := e.apply("1\nothre\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("/work/other", other, "readable and writable from the sandbox", "credentials or keys: .env",
		"That is not other")
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

	if err := e.apply("1\nother\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	if f.Roots["dev"].Host != "~/dev" || f.Roots["other"].Host != "~/src/other" || len(f.Roots) != 2 {
		t.Errorf("roots %v", f.Roots)
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
	e.a.Cfg.File = &config.File{Roots: map[string]config.FileRoot{"dev": {Host: "~/dev"}}}
	e.propose("x.toml", "title = \"t\"\n[roots]\nx = \"~/link\"\n")
	if err := e.apply("1\nx\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	if f.Roots["x"].Host != "~/src/real" {
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
		{"ok", "~/ok", ""},
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
	e.a.Cfg.File = &config.File{Roots: map[string]config.FileRoot{"dev": {Host: "~/dev"}}}
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
	e.useDockerfile()
	e.a.exportProposals()
	e.a.notePendingProposals()
	e.wantOut("2 pending proposals (bar, foo): 'caboose -e work apply' reviews them")
	state, err := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`image = "dockerfile.default"`, `dockerfile = "seed"`, seedHash(t), `host = "~/dev"`, `path = "/work/dev"`} {
		if !strings.Contains(string(state), want) {
			t.Errorf("state.toml lacks %q:\n%s", want, state)
		}
	}
}

// An error quoting a name the sandbox chose is shown escaped: a proposal
// gone by the time it is deleted fails with its path in the error.
func TestDeleteProposalErrorIsPrintable(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.propose("other.toml", "title = \"t\"\n")
	var out strings.Builder
	p := newPrompter(strings.NewReader(""), &out)
	if err := e.a.deleteProposal(p, proposal.Entry{File: "x\x1b]0;owned\a.toml"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.ContainsAny(got, "\x1b\a") || !strings.Contains(got, `\u001B`) {
		t.Errorf("output %q", got)
	}
}
