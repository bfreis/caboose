package launcher

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
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

	// The proposal is shown as the physical path, which differs from
	// other when the temp dir is reached through a symlink (macOS's).
	phys, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}

	// Apply; type the name wrong.
	if err := e.apply("1\nothre\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("/work/other", phys, "becomes readable from the sandbox. It is mounted read-only", "credentials or keys: .env",
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
	if f.Roots["dev"].Host != "~/dev" || f.Roots["other"].Host != "~/src/other" || len(f.Roots) != 2 || f.Roots["other"].Writable != nil {
		t.Errorf("roots %v", f.Roots)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the proposal is still there: %v", err)
	}
}

// A root through a symlink is judged, and written, as where it leads.
// Applying a root keeps what the other roots let the sandbox write, and
// adds the new one read-only.
func TestApplyRootKeepsWritable(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.dir("dev/p")
	e.dir("src/other")
	e.writeConfig("[roots]\ndev = { host = \"~/dev\", writable = [\"p\"] }\n")
	e.propose("other.toml", "title = \"Mount other\"\n[roots]\nother = \"~/src/other\"\n")
	if err := e.apply("1\nother\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Roots["dev"].Writable, []string{"p"}) || f.Roots["other"].Writable != nil || len(f.Roots) != 2 {
		t.Errorf("roots %+v\n%s", f.Roots, e.configFile())
	}
}

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
		{"x", e.a.Cfg.CabooseHome, "is CABOOSE_HOME"},
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
	// A CABOOSE_HOME not made yet, under a second name for the root: no
	// path comparison sees it, the identity of its nearest ancestor does.
	e.mkdir("real")
	if err := os.Symlink(filepath.Join(home, "real"), filepath.Join(home, "alias")); err != nil {
		t.Fatal(err)
	}
	prev := e.a.Cfg.CabooseHome
	e.a.Cfg.CabooseHome = filepath.Join(home, "alias", "caboose")
	_, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "real", Path: "~/real"})
	if got := strings.Join(refusals, "\n"); !strings.Contains(got, "holds CABOOSE_HOME") {
		t.Errorf("refusals %q", got)
	}
	e.a.Cfg.CabooseHome = prev
	// A name one of several roots has is taken.
	e.a.Cfg.File = &config.File{Roots: map[string]config.FileRoot{"dev": {Host: "~/dev"}}}
	if _, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "dev", Path: "~/ok"}); len(refusals) != 1 || !strings.Contains(refusals[0], "named dev already") {
		t.Errorf("refusals %q", refusals)
	}
}

// The home's hidden directories and ~/Library are judged ignoring case,
// as a case-insensitive volume finds them: a root proposed under another
// case of the home is still in ~/.caboose or ~/.ssh.
func TestProposedRootHomeIgnoringCase(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	e.mkdir(".caboose/daemon")
	e.mkdir(".ssh")
	e.mkdir("library/x")
	e.mkdir("ok")
	e.a.Cfg.Home = filepath.Join(filepath.Dir(home), "HOME")
	for _, tc := range []struct{ path, want string }{
		{filepath.Join(home, ".caboose", "daemon"), "is in ~/.caboose"},
		{filepath.Join(home, ".ssh"), "is in ~/.ssh"},
		{filepath.Join(home, "library", "x"), "is in ~/library"},
		{filepath.Join(home, "ok"), ""},
	} {
		_, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "x", Path: tc.path})
		got := strings.Join(refusals, "\n")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusals %q, want %q", tc.path, got, tc.want)
		}
	}
}

// caboose's state is judged by identity: a root holding an environment's
// directory not made yet, under a second name for the root, which no
// comparison of paths sees.
func TestProposedRootStateByIdentity(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	e.mkdir("real")
	symlink(t, filepath.Join(home, "real"), filepath.Join(home, "second"))
	e.a.Cfg.CabooseHome = filepath.Join(filepath.Dir(home), "elsewhere")
	e.a.Cfg.EnvDir = filepath.Join(home, "second", "env")
	e.a.Cfg.DataDir = filepath.Join(home, "second", "env", "data")
	_, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "x", Path: "~/real"})
	if got := strings.Join(refusals, "\n"); !strings.Contains(got, "overlaps this environment") {
		t.Errorf("refusals %q", got)
	}
}

// The host path a proposed root leads to, through a symlink the sandbox
// may have made, is shown escaped: a directory's name can hold control
// characters.
func TestProposedRootShowsHostPathEscaped(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	e.mkdir("dev/sub/\x1b]52;c;evil\x07")
	symlink(t, filepath.Join(home, "dev", "sub", "\x1b]52;c;evil\x07"), filepath.Join(home, "dev", "l"))
	e.a.Cfg.File = &config.File{Roots: map[string]config.FileRoot{"dev": {Host: "~/dev"}}}
	host, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "x", Path: "~/dev/l"})
	got := strings.Join(refusals, "\n")
	if !strings.Contains(got, "overlaps the root ~/dev") || strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("refusals %q", got)
	}
	pl := plan{root: &proposal.Root{Name: "x", Path: "~/dev/l"}, host: host}
	if note := e.a.rootNote(pl); strings.ContainsAny(note, "\x1b\x07") {
		t.Errorf("note %q", note)
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

// otherEnv makes environment name's directory, with conf as its
// config.toml ("" for none).
func otherEnv(t *testing.T, e *setupEnv, name, conf string) {
	t.Helper()
	dir := config.EnvDirFor(e.a.Cfg.CabooseHome, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if conf != "" {
		if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A proposed root in what another environment's sandbox can write is
// refused, whether that sandbox exists now or only its config.toml says
// so: that sandbox could swap it for a symlink to anywhere before a
// launch. One whose writable paths cannot be had at all is refused too.
func TestProposedRootInWhatASandboxWrites(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	e.mkdir("shared/x")
	e.mkdir("live/x")
	e.mkdir("ok")
	if err := os.MkdirAll(e.a.Cfg.EnvDir, 0o700); err != nil {
		t.Fatal(err)
	}
	otherEnv(t, e, "other", "[roots]\nshared = { host = \"~/shared\", writable = [\".\"] }\n")
	theirs := sandbox(nil, filepath.Join(home, "live"))
	e.a.envBox = func(env string) (backend.Backend, bool, error) {
		if env == "other" {
			return theirs, true, nil
		}
		return &backendtest.Fake{}, true, nil
	}
	for _, tc := range []struct{ path, want string }{
		{"~/shared/x", "which a sandbox can write"},
		{"~/live/x", "which a sandbox can write"},
		{"~/ok", ""},
		// The very directory another environment mounts writable whole is
		// shared: a sandbox cannot replace its own mount point.
		{"~/shared", ""},
	} {
		_, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "x", Path: tc.path})
		got := strings.Join(refusals, "\n")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusals %q, want %q", tc.path, got, tc.want)
		}
	}
	// Its sandbox cannot be asked, but its config.toml says enough.
	e.a.envBox = func(string) (backend.Backend, bool, error) { return nil, false, errors.New("engine down") }
	if _, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "x", Path: "~/shared/x"}); !strings.Contains(strings.Join(refusals, "\n"), "which a sandbox can write") {
		t.Errorf("configured only: %q", refusals)
	}
	// Its config.toml does not load.
	otherEnv(t, e, "other", "not toml [[[")
	_, refusals, _ := e.a.checkProposedRoot(proposal.Root{Name: "ok", Path: "~/ok"})
	if got := strings.Join(refusals, "\n"); !strings.Contains(got, "cannot be checked against what the sandboxes can write") || !strings.Contains(got, "other: its config.toml does not load") {
		t.Errorf("an environment that cannot be read: %q", got)
	}
}

// Just before a sandbox is created, a root that lies, or is reached
// through a symlink that lies, where another environment's sandbox can
// write is refused: configured there or mounted now. A root that is the
// very directory another environment mounts writable whole is shared,
// and fine. An environment whose engine is down counts by its config.toml
// alone; one whose config.toml does not load refuses, named.
func TestCheckRootReach(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	e.mkdir("shared/x")
	e.mkdir("ok")
	if err := os.MkdirAll(e.a.Cfg.EnvDir, 0o700); err != nil {
		t.Fatal(err)
	}
	down := func(string) (backend.Backend, bool, error) { return nil, false, errors.New("engine down") }
	e.a.envBox = down
	e.a.Cfg.Roots = []config.Root{{Name: "x", Host: filepath.Join(home, "shared", "x"), Container: "/work/x"}}
	if err := e.a.checkRootReach(); err != nil {
		t.Fatalf("no other environment: %v", err)
	}
	otherEnv(t, e, "other", "[roots]\nshared = { host = \"~/shared\", writable = [\".\"] }\n")
	if err := e.a.checkRootReach(); err == nil || !strings.Contains(err.Error(), "another environment's sandbox can write") {
		t.Errorf("a plain directory in a configured writable part, its engine down: %v", err)
	}
	e.a.Cfg.Roots = []config.Root{{Name: "shared", Host: filepath.Join(home, "shared"), Container: "/work/shared"}}
	if err := e.a.checkRootReach(); err != nil {
		t.Errorf("a root another environment mounts writable whole, shared: %v", err)
	}
	theirs := sandbox(nil, filepath.Join(home, "shared"))
	e.a.envBox = func(string) (backend.Backend, bool, error) { return theirs, true, nil }
	if err := e.a.checkRootReach(); err != nil {
		t.Errorf("the same, its sandbox mounting it now: %v", err)
	}
	e.a.envBox = down
	e.a.Cfg.Roots = []config.Root{{Name: "p", Host: filepath.Join(home, "ok"), Container: "/work/p", Via: []string{filepath.Join(home, "shared", "l")}}}
	if err := e.a.checkRootReach(); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a link in a configured writable part: %v", err)
	}
	e.a.Cfg.Roots = []config.Root{{Name: "p", Host: filepath.Join(home, "ok"), Container: "/work/p", Via: []string{filepath.Join(home, "elsewhere", "l")}}}
	if err := e.a.checkRootReach(); err != nil {
		t.Errorf("elsewhere, the other environment's engine down: %v", err)
	}
	// A link where its sandbox mounts something writable now.
	live := sandbox(nil, filepath.Join(home, "elsewhere"))
	e.a.envBox = func(string) (backend.Backend, bool, error) { return live, true, nil }
	if err := e.a.checkRootReach(); err == nil || !strings.Contains(err.Error(), "which a sandbox can write") {
		t.Errorf("a link where the live sandbox writes: %v", err)
	}
	// Its sandbox is there, but its mounts cannot be read: it may mount
	// more than its config.toml now says, so the launch is refused.
	blind := &failingBox{Fake: sandbox(nil, filepath.Join(home, "elsewhere")), mounts: errors.New("timed out")}
	e.a.envBox = func(string) (backend.Backend, bool, error) { return blind, true, nil }
	e.a.Cfg.Roots = []config.Root{{Name: "p", Host: filepath.Join(home, "ok"), Container: "/work/p"}}
	if err := e.a.checkRootReach(); err == nil || !strings.Contains(err.Error(), "other: its sandbox's mounts cannot be read") {
		t.Errorf("a sandbox whose mounts cannot be read: %v", err)
	}
	// Its config.toml does not load: the launch is refused, naming it.
	otherEnv(t, e, "other", "not toml [[[")
	e.a.envBox = down
	if err := e.a.checkRootReach(); err == nil || !strings.Contains(err.Error(), "other: its config.toml does not load") || !strings.Contains(err.Error(), "fix it, or remove the environment") {
		t.Errorf("a config.toml that does not load: %v", err)
	}
}
