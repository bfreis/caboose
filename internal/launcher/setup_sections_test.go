package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/statesync"
)

// configPath is the environment's config.toml.
func (e *setupEnv) configPath() string { return filepath.Join(e.a.Cfg.EnvDir, config.FileName) }

// writeConfig writes the environment's config.toml, and loads it as a
// launch would.
func (e *setupEnv) writeConfig(data string) {
	e.t.Helper()
	if err := os.MkdirAll(e.a.Cfg.EnvDir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.configPath(), []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.reload()
}

// reload reads config.toml back into the App, as the next command would.
func (e *setupEnv) reload() {
	e.t.Helper()
	data, err := os.ReadFile(e.configPath())
	if err != nil {
		e.t.Fatal(err)
	}
	f, err := config.ParseFile(e.configPath(), data)
	if err != nil {
		e.t.Fatalf("config.toml does not parse: %v\n%s", err, data)
	}
	e.a.Cfg.File = f
	// What the file says of the isolation, as the next command would see it.
	cfg, err := config.Load(e.a.Cfg.Getenv, config.OSFS{}, e.a.Cfg.Env)
	if err != nil {
		e.t.Fatalf("config.toml does not load: %v\n%s", err, data)
	}
	e.a.Cfg.Isolation, e.a.Cfg.Profile, e.a.Cfg.AutoSync = cfg.Isolation, cfg.Profile, cfg.AutoSync
}

// file is the environment's config.toml, parsed.
func (e *setupEnv) file() *config.File {
	e.t.Helper()
	e.reload()
	return e.a.Cfg.File
}

func (e *setupEnv) mkdir(rel string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Join(e.a.Cfg.Home, rel), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

// wantOut checks that setup said each of wants, however it wrapped them:
// runs of blanks and newlines compare as one space.
func (e *setupEnv) wantOut(wants ...string) {
	e.t.Helper()
	for _, w := range wants {
		if !e.said(w) {
			e.t.Errorf("no %q in:\n%s", w, e.errb)
		}
	}
}

// said is whether setup said s, however it wrapped it.
func (e *setupEnv) said(s string) bool {
	return strings.Contains(squash(e.errb.String()), squash(s))
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestSetupRootsKeepsTheDefault(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	// change it, to a directory that is not there, then Enter: the
	// default is still the current one; use these.
	if err := e.run("2\n/nonexistent\n\n\n", "roots"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("/work/dev ← ~/dev\n", "? Use these roots? 1 Use these 2 Change dev (~/dev) 3 Add another root\n",
		"✗ /nonexistent does not exist\n? Directory (an absolute path, or one starting with ~/): [~/dev] ", "· Nothing changed")
	if f := e.file(); len(f.Roots) != 0 {
		t.Errorf("wrote roots: %+v", f)
	}
}

// With the default missing, it cannot be used: the menu starts at
// changing it, and a directory that does not exist is asked again.
func TestSetupRootsReplacesAMissingDefault(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := os.Remove(filepath.Join(e.a.Cfg.Home, "dev")); err != nil {
		t.Fatal(err)
	}
	e.mkdir("src")
	if err := e.run("\n/nonexistent\nsrc\n~/src\n\n", "roots"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("✗ ~/dev does not exist\n", "? These roots cannot be used as they are. Fix them: 1 Change dev (~/dev) 2 Add another root\nchoose 1-2 [1]: ",
		"✗ /nonexistent does not exist\n", "✗ src is not an absolute path (or one starting with ~/)\n",
		"✓ Wrote them to "+e.configPath())
	if e.said("anyway?") {
		t.Errorf("a root nobody could use asked about moving projects:\n%s", e.errb)
	}
	if f := e.file(); len(f.Roots) != 1 || f.Roots["dev"].Host != "~/src" {
		t.Errorf("roots = %+v", f.Roots)
	}
}

// A second root is named, and added beside the first, whose projects keep
// their paths. A hand-edited config.toml keeps the rest of itself.
func TestSetupRootsAddsASecond(t *testing.T) {
	e := newSetupEnv(t, "default", "", true)
	e.mkdir("work")
	e.writeConfig("# my notes\n[session]\nkeep_versions = 3\n")
	// add; ~/work; name: an invalid one, dev (taken), then the default;
	// use these.
	if err := e.run("3\n~/work\nBad Name\ndev\n\n\n", "roots"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut(
		"? Name for ~/work, mounted at /work/NAME: [work] ",
		"✗ 'Bad Name' is not a root name",
		"✗ Another root is named 'dev'\n",
		"/work/dev ← ~/dev\n /work/work ← ~/work\n",
		"1 Use these 2 Change dev (~/dev) 3 Change work (~/work) 4 Add another root 5 Remove dev (~/dev) 6 Remove work (~/work)\n",
		"! The container keeps the roots it was created with: caboose restart remounts them",
	)
	if e.said("anyway?") {
		t.Errorf("asked about moving projects that stay put:\n%s", e.errb)
	}
	f := e.file()
	if f.Roots["dev"].Host != "~/dev" || f.Roots["work"].Host != "~/work" || len(f.Roots) != 2 {
		t.Errorf("config: %+v", f)
	}
	b, _ := os.ReadFile(e.configPath())
	if !strings.HasPrefix(string(b), "# my notes\n[session]\nkeep_versions = 3\n") {
		t.Errorf("config.toml:\n%s", b)
	}
}

// A sole root in its long form, at /work itself, moves to /work/NAME when
// a second is added: its projects' paths change, which is said and confirmed.
func TestSetupRootsLongFormSoleRoot(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("work")
	e.writeConfig("[roots.dev]\nhost = \"~/dev\"\npath = \"/work\"\n")
	if err := e.run("3\n~/work\n\n\ny\n", "roots"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("! Projects under ~/dev move from /work/... to /work/dev/...", "? Change the roots anyway? [y/N] ")
	f := e.file()
	if len(f.Roots) != 2 || f.Roots["dev"].Host != "~/dev" || f.Roots["dev"].Path != "" || f.Roots["work"].Host != "~/work" {
		t.Errorf("config: %+v", f)
	}
}

// An edit that changes nothing leaves the file as it is: not rewritten.
func TestWriteConfigUnchanged(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("[session]\nauto_sync = true\n")
	before, err := os.Stat(e.configPath())
	if err != nil {
		t.Fatal(err)
	}
	changed, err := e.a.writeConfig(config.Edit{Set: map[string]any{"session.auto_sync": true}})
	after, _ := os.Stat(e.configPath())
	if changed || err != nil || !os.SameFile(before, after) {
		t.Errorf("changed %v, err %v, same file %v", changed, err, os.SameFile(before, after))
	}
}

func TestSetupRootsMoveDeclined(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("work")
	e.writeConfig("[roots.dev]\nhost = \"~/dev\"\npath = \"/work\"\n")
	if err := e.run("3\n~/work\n\n\n\n", "roots"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("· Nothing changed")
	if f := e.file(); len(f.Roots) != 1 || f.Roots["dev"].Path != "/work" {
		t.Errorf("config: %+v", f)
	}
}

// Removing one of two keeps the other's name, and so its projects' paths;
// the removed one's projects are said to go.
func TestSetupRootsRemovesOne(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("work")
	e.writeConfig("[roots]\ndev = \"~/dev\"\nwork = \"~/work\"\n")
	if err := e.run("6\n\ny\n", "roots"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("! Projects under ~/work are no longer mounted (they were at /work/work)\n")
	if e.said("~/dev move") || e.said("restart") {
		t.Errorf("stderr:\n%s", e.errb)
	}
	if f := e.file(); len(f.Roots) != 1 || f.Roots["dev"].Host != "~/dev" {
		t.Errorf("config: %+v", f)
	}
}

// Roots that hold one another cannot be used together.
func TestSetupRootsOverlap(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/sub")
	e.writeConfig("[roots]\ndev = \"~/dev\"\nsub = \"~/dev/sub\"\n")
	// no "use these": 1 change dev, 2 change sub, 3 add, 4 remove dev,
	// 5 remove sub. Remove sub, then use these.
	if err := e.run("5\n\ny\n", "roots"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("✗ ~/dev and ~/dev/sub overlap", "1 Change dev (~/dev)\n")
	if f := e.file(); len(f.Roots) != 1 {
		t.Errorf("config: %+v", f)
	}
}

// A config.toml the line editor cannot change safely is left alone, with
// what to write by hand.
func TestSetupRootsUnsafeEdit(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("src")
	// A multi-line string whose body looks like the roots table: the editor would
	// replace a line inside it.
	odd := "[session]\ntz = \"\"\"\n[roots]\ndev = \"~/dev\"\n\"\"\"\n"
	if err := os.MkdirAll(e.a.Cfg.EnvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.configPath(), []byte(odd), 0o644); err != nil {
		t.Fatal(err)
	}
	err := e.run("2\n~/src\n\ny\n", "roots")
	if err == nil || !strings.Contains(err.Error(), "cannot edit "+e.configPath()+" safely") ||
		!strings.Contains(err.Error(), `dev = "~/src"`) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(e.configPath()); string(b) != odd {
		t.Errorf("config.toml changed:\n%s", b)
	}
}

// syncSetupEnv is a setupEnv whose container is up and runs its git with
// the host's, on the data dir's sync repo, as cmd/caboose's sync tests do;
// and a bare remote to sync with.
func syncSetupEnv(t *testing.T) (e *setupEnv, remote string) {
	t.Helper()
	e = newSetupEnv(t, "work", "", false)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	remote = filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command(git, "init", "-q", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	c := e.a.Cfg
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c.ReadyTimeout = 1
	c.Roots = []config.Root{{Name: config.DefaultRootName, Host: filepath.Join(t.TempDir(), "dev"), Container: config.WorkDir + "/" + config.DefaultRootName}}
	repo := filepath.Join(c.DataDir, statesync.Dir)
	home := filepath.Join(c.DataDir, datadir.HomeDir)
	script := `#!/bin/sh
case "$*" in
  "inspect --type=container -f {{.State.Status}} ` + c.Container + `") echo running; exit 0 ;;
  "exec ` + c.Container + ` test -f ` + ReadyMarker + `") exit 0 ;;
  "exec ` + c.Container + ` bash -c for d in /proc/"*) echo "7 1 sleep"; exit 0 ;;
  "inspect --type=container ` + c.Container + ` --format "*)
    for k in .claude .claude.json .config/caboose; do printf '/home/agent/%s\t%s\n' "$k" "` + home + `/$k"; done
    printf '` + statesync.ContainerDir + `\t%s\n' "` + repo + `"; exit 0 ;;
esac
[ "$1" = exec ] || exit 1
shift
while :; do
  case "$1" in
    -i|-t) shift ;;
    -e) shift 2 ;;
    *) break ;;
  esac
done
[ "$1" = ` + c.Container + ` ] || exit 1
shift
case "$1" in
  git) exec "$@" ;;
  ` + AgentPath + `) shift
    exec env ` + testSyncHome + `="` + home + `" ` + testSyncRepo + `="` + repo + `" "` + os.Args[0] + `" "$@" ;;
esac
exit 1
`
	if err := os.WriteFile(e.a.Docker.Path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	return e, remote
}

// A new remote is set and synced with, as caboose sync --remote does, and
// auto_sync goes into config.toml. A re-run at the defaults does neither.
func TestSetupSyncSetsTheRemote(t *testing.T) {
	e, remote := syncSetupEnv(t)
	e.writeConfig("")
	mem := filepath.Join(e.a.Cfg.DataDir, datadir.ClaudeDir, "projects", statesync.ProjectKey("/work/dev/proj"), "memory")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "m.md"), []byte("a fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.run(remote+"\ny\n", "sync"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("? Sync remote, a git URL (empty for none): ",
		"? Sync by itself, before a launch that finds nothing running? (auto_sync) [y/N] ",
		"✓ Wrote auto_sync = true in [session] to "+e.configPath(),
		"Setting the remote and syncing, as 'caboose sync --remote "+remote+"' does",
		"pushed", "✓ Synced with "+remote)
	if strings.Contains(e.errb.String(), "is not set up") || strings.Contains(e.errb.String(), "no git identity") {
		t.Errorf("a launch's note, in setup:\n%s", e.errb)
	}
	if e.file().Vals["session.auto_sync"] != true {
		t.Error("auto_sync not written")
	}
	got, err := exec.Command("git", "--git-dir", remote, "show", "main:home/.claude/projects/"+statesync.ProjectKey("/work/dev/proj")+"/memory/m.md").CombinedOutput()
	if err != nil || string(got) != "a fact\n" {
		t.Errorf("the remote holds %q (%v)", got, err)
	}

	before, _ := os.ReadFile(e.configPath())
	if err := e.run("\n\n", "sync"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("? Sync remote, a git URL: ["+remote+"] ", "[Y/n]", "· Nothing changed")
	if strings.Contains(e.errb.String(), "syncing") {
		t.Errorf("synced again:\n%s", e.errb)
	}
	if after, _ := os.ReadFile(e.configPath()); string(after) != string(before) {
		t.Errorf("config.toml rewritten:\n%s", after)
	}

	// Off again: written as false, and said.
	if err := e.run("\nn\n", "sync"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("✓ Wrote auto_sync = false in [session]")
	if e.file().Vals["session.auto_sync"] != false {
		t.Error("auto_sync still on")
	}
}

// A sync that cannot run is said, with what to run later; setup goes on.
func TestSetupSyncCannotRun(t *testing.T) {
	e := newSetupEnv(t, "default", "", false) // no container, no image
	e.writeConfig("")
	if err := e.run("git@example.invalid:me/state.git\n\n", "sync"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("✗ Did not sync: ",
		"! The remote is not set; once that is fixed, caboose sync --remote git@example.invalid:me/state.git sets it")
}

// No remote, and none given: no question about auto_sync, nothing written.
func TestSetupSyncNone(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("\n", "sync"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if strings.Contains(e.errb.String(), "auto_sync") {
		t.Errorf("stderr:\n%s", e.errb)
	}
	e.wantOut("· Nothing changed")
}

// A whole run asks roots, the image and the isolation, brings the
// container up, then git and sync, and ends at the login; at the defaults (and declining to
// create the container) it writes nothing but the template.
func TestSetupWholeRunOrder(t *testing.T) {
	e := newSetupEnv(t, "default", "[user]\n\tname = Me\n\temail = me@example.invalid\n", false)
	if err := e.run("\n\nn\n\n\n\n\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	out := e.errb.String()
	last := -1
	for _, h := range []string{"\n  Roots ", "\n  Image ", "\n  Isolation ", "\n  Container ", "\n  Git ", "\n  Sync ", "is set up", "\n  Claude login "} {
		i := strings.Index(out, h)
		if i <= last {
			t.Errorf("%q out of order (at %d, after %d):\n%s", h, i, last, out)
		}
		last = i
	}
	e.wantOut("· Not created: the first launch creates it", "! Not logged in yet. Run caboose in a project under ", "Roots 1/7", "Claude login 7/7")
	if b, _ := os.ReadFile(e.configPath()); string(b) != config.Template {
		t.Errorf("config.toml:\n%s", b)
	}
}
