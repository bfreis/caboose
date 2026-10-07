package statesync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// These drive real git: two data dirs, standing for two machines, syncing
// through one bare remote in a temp dir.

type machine struct {
	t *testing.T
	s *Syncer
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// The user's own git config must not reach the test repos.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Nor auto maintenance, which fetch and commit start detached: it can
	// still be writing objects/ when the test's TempDir is removed.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "maintenance.auto")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
}

func newRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", Branch, dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	// receive-pack starts its own detached maintenance after a push, and
	// a local push clears the environment's config: say it in the repo's.
	for _, kv := range [][2]string{{"maintenance.auto", "false"}, {"receive.autogc", "false"}} {
		if out, err := exec.Command("git", "-C", dir, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
	return dir
}

// newSyncer is a Syncer on a home and a sync repo of its own.
func newSyncer(t *testing.T, host string) *Syncer {
	d := t.TempDir()
	return &Syncer{Home: filepath.Join(d, "home"), Repo: filepath.Join(d, "sync"), Host: host, Roots: testRoots}
}

func newMachine(t *testing.T, host, remote string) *machine {
	t.Helper()
	m := &machine{t: t, s: newSyncer(t, host)}
	m.write(".claude.json", `{"oauthAccount":{"id":"`+host+`"},"numStartups":1}`)
	if err := m.s.Init(); err != nil {
		t.Fatal(err)
	}
	if err := m.s.SetRemote(remote); err != nil {
		t.Fatal(err)
	}
	return m
}

// testRoots are the machines' roots: a sole root at /work.
var testRoots = []string{"/work"}

// mem is the data dir path of a project's memory file, rel being the
// project's path under /work -- the same on every machine.
func mem(rel, file string) string {
	return ".claude/projects/" + ProjectKey("/work/"+rel) + "/memory/" + file
}

// write, read and exists take a path relative to the home, as the
// sandbox sees it; the data dir keeps it under home/.
func (m *machine) write(rel, data string) {
	m.t.Helper()
	p := m.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) read(rel string) string {
	m.t.Helper()
	b, err := os.ReadFile(m.path(rel))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (m *machine) exists(rel string) bool {
	_, err := os.Lstat(m.path(rel))
	return err == nil
}

// path is home-relative rel's place on the host.
func (m *machine) path(rel string) string {
	return filepath.Join(m.s.Home, filepath.FromSlash(rel))
}

func (m *machine) sync() *Report {
	m.t.Helper()
	m.s.Sandbox = nil // read afresh, as each caboose sync does
	r, err := m.s.Sync()
	if err != nil {
		m.t.Fatalf("%s: sync: %v", m.s.Host, err)
	}
	return r
}

// exported is what the machine would export now, in repo terms.
func (m *machine) exported() map[string]string {
	m.t.Helper()
	c, err := m.s.rules()
	if err != nil {
		m.t.Fatal(err)
	}
	e, err := ExportLive(m.s.Home, c)
	if err != nil {
		m.t.Fatal(err)
	}
	out := map[string]string{}
	for p, f := range e.Files {
		out[p] = string(f.Data)
	}
	return out
}

func converged(t *testing.T, a, b *machine) {
	t.Helper()
	if ea, eb := a.exported(), b.exported(); !reflect.DeepEqual(ea, eb) {
		t.Errorf("not converged:\n%s: %v\n%s: %v", a.s.Host, ea, b.s.Host, eb)
	}
}

func TestSyncAcrossMachines(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)

	a.write(mem("bfreis/caboose", "MEMORY.md"), "- [one](one.md) — first\n")
	a.write(mem("bfreis/caboose", "one.md"), "fact one\n")
	a.write(".claude/settings.json", `{"theme":"dark"}`)
	a.write(".claude/skills/hello/SKILL.md", "say hello\n")
	a.write(".claude/skills/hello/run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(a.s.Home, ".claude/skills/hello/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.write(".claude.json", `{"oauthAccount":{"id":"alpha"},"mcpServers":{"x":{"command":"x"}}}`)
	// Not synced: the credential, transcripts, history, a project outside
	// /work.
	a.write(".claude/.credentials.json", `{"claudeAiOauth":{}}`)
	a.write(".claude/projects/"+ProjectKey("/work/bfreis/caboose")+"/s.jsonl", "{}\n")
	a.write(".claude/history.jsonl", "{}\n")
	a.write(".claude/projects/-home-agent/memory/x.md", "outside\n")
	a.write(".claude/skills/synced/acct_org/docx/SKILL.md", "Claude Code's cache\n")
	a.write(".claude/skills/synced-by-me/SKILL.md", "mine, despite the name\n")
	a.write(".config/caboose/start.d/10-foo", "#!/bin/sh\nfoo &\n")
	if err := os.Chmod(a.path(".config/caboose/start.d/10-foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.write(".config/caboose/shell.d/aliases.sh", "alias ll='ls -l'\n")
	// Kept, but not synced by the default rules.
	a.write(".config/git/config", "[user]\n\tname = alpha\n")
	a.write(".config/gh/hosts.yml", "github.com: {}\n")

	r := a.sync()
	if !r.Committed || !r.Pushed || r.Merged {
		t.Errorf("first sync: %+v", r)
	}

	r = b.sync()
	if !r.Merged {
		t.Errorf("second machine took nothing: %+v", r)
	}
	if got := b.read(mem("bfreis/caboose", "one.md")); got != "fact one\n" {
		t.Errorf("memory = %q", got)
	}
	if got := b.read(".claude/skills/hello/SKILL.md"); got != "say hello\n" {
		t.Errorf("skill = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(b.s.Home, ".claude/skills/hello/run.sh")); err != nil || fi.Mode()&0o100 == 0 {
		t.Errorf("run.sh lost its exec bit: %v %v", fi, err)
	}
	cj := b.read(".claude.json")
	if !strings.Contains(cj, `"mcpServers"`) || !strings.Contains(cj, `"id": "beta"`) || !strings.Contains(cj, `"numStartups": 1`) {
		t.Errorf(".claude.json should gain mcpServers and keep its own keys:\n%s", cj)
	}
	if got := b.read(".claude/skills/synced-by-me/SKILL.md"); got != "mine, despite the name\n" {
		t.Errorf("skills/synced-by-me = %q", got)
	}
	if fi, err := os.Stat(b.path(".config/caboose/start.d/10-foo")); err != nil || fi.Mode()&0o100 == 0 {
		t.Errorf("start.d/10-foo missing or lost its exec bit: %v %v", fi, err)
	}
	if got := b.read(".config/caboose/shell.d/aliases.sh"); got != "alias ll='ls -l'\n" {
		t.Errorf("shell.d/aliases.sh = %q", got)
	}
	for _, p := range []string{".claude/.credentials.json", ".claude/history.jsonl", ".claude/projects/-home-agent/memory/x.md",
		".claude/skills/synced", ".config/git/config", ".config/gh/hosts.yml"} {
		if b.exists(p) {
			t.Errorf("%s reached the other machine", p)
		}
	}
	converged(t, a, b)
}

func TestSyncMergesBothSides(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.write(mem("p", "MEMORY.md"), "- one\n")
	a.write(mem("p", "facts.md"), "line 1\nline 2\nline 3\nline 4\nline 5\n")
	a.write(".claude/settings.json", `{"theme":"dark","env":{"A":"1"}}`)
	a.sync()
	b.sync()

	// Both append to the index, edit different lines of one memory, and
	// different keys of settings.
	a.write(mem("p", "MEMORY.md"), "- one\n- from alpha\n")
	b.write(mem("p", "MEMORY.md"), "- one\n- from beta\n")
	a.write(mem("p", "facts.md"), "line 1 (alpha)\nline 2\nline 3\nline 4\nline 5\n")
	b.write(mem("p", "facts.md"), "line 1\nline 2\nline 3\nline 4\nline 5 (beta)\n")
	a.write(".claude/settings.json", `{"theme":"dark","env":{"A":"1","B":"2"}}`)
	b.write(".claude/settings.json", `{"theme":"light","env":{"A":"1"}}`)
	b.write(mem("p", "new.md"), "from beta\n")

	a.sync()
	b.sync()
	a.sync()
	converged(t, a, b)

	idx := a.read(mem("p", "MEMORY.md"))
	if !strings.Contains(idx, "from alpha") || !strings.Contains(idx, "from beta") {
		t.Errorf("MEMORY.md lost a side:\n%s", idx)
	}
	if got := a.read(mem("p", "facts.md")); got != "line 1 (alpha)\nline 2\nline 3\nline 4\nline 5 (beta)\n" {
		t.Errorf("facts.md = %q", got)
	}
	st := a.read(".claude/settings.json")
	if !strings.Contains(st, `"theme": "light"`) || !strings.Contains(st, `"B": "2"`) {
		t.Errorf("settings.json not merged by key:\n%s", st)
	}
	if a.read(mem("p", "new.md")) != "from beta\n" {
		t.Error("new.md did not reach alpha")
	}
}

func TestSyncDeletes(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.write(mem("p", "gone.md"), "x\n")
	a.write(mem("p", "kept.md"), "y\n")
	a.write(".claude/skills/old/SKILL.md", "x\n")
	a.write(".claude.json", `{"mcpServers":{"x":{}}}`)
	a.sync()
	b.sync()

	for _, p := range []string{mem("p", "gone.md"), ".claude/skills/old/SKILL.md"} {
		if err := os.Remove(a.path(p)); err != nil {
			t.Fatal(err)
		}
	}
	a.write(".claude.json", `{"numStartups":3}`)
	a.sync()
	b.sync()
	if b.exists(mem("p", "gone.md")) || !b.exists(mem("p", "kept.md")) {
		t.Error("the deletion did not carry over, or took too much")
	}
	if b.exists(".claude/skills/old") || !b.exists(".claude/skills") {
		t.Error("the emptied skill dir should go, and skills/ stay")
	}
	if strings.Contains(b.read(".claude.json"), "mcpServers") {
		t.Errorf("mcpServers removed on alpha should go on beta:\n%s", b.read(".claude.json"))
	}
	converged(t, a, b)
}

func TestSyncConflict(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.write(".claude/settings.json", `{"theme":"dark","x":1}`)
	a.write(mem("p", "f.md"), "base\n")
	a.sync()
	b.sync()
	a.write(".claude/settings.json", `{"theme":"light","x":1}`)
	a.write(mem("p", "f.md"), "alpha\n")
	a.sync()
	b.write(".claude/settings.json", `{"theme":"solar","x":2}`)
	b.write(mem("p", "f.md"), "beta\n")

	// No resolver: the sync stops, and nothing live changes.
	_, err := b.s.Sync()
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	if b.read(mem("p", "f.md")) != "beta\n" || !strings.Contains(b.read(".claude/settings.json"), "solar") {
		t.Error("an aborted sync changed live files")
	}

	var seen []Conflict
	b.s.Resolve = func(c Conflict) (Resolution, error) {
		seen = append(seen, c)
		if c.Keys != nil {
			return c.Take(Theirs), nil
		}
		m, err := c.Markers()
		if err != nil {
			return Resolution{}, err
		}
		if !HasMarkers(m) || !strings.Contains(string(m), "alpha") || !strings.Contains(string(m), "beta") {
			t.Errorf("markers:\n%s", m)
		}
		return Resolution{Content: []byte("alpha and beta\n")}, nil
	}
	b.sync()
	if len(seen) != 2 {
		t.Fatalf("resolver saw %d conflicts, want 2", len(seen))
	}
	for _, c := range seen {
		if c.Path == RepoHome+".claude/settings.json" && !reflect.DeepEqual(c.Keys, []string{"theme"}) {
			t.Errorf("settings conflict keys = %v, want only theme", c.Keys)
		}
	}
	st := b.read(".claude/settings.json")
	if !strings.Contains(st, `"theme": "light"`) || !strings.Contains(st, `"x": 2`) {
		t.Errorf("theirs for theme, beta's own x:\n%s", st)
	}
	if b.read(mem("p", "f.md")) != "alpha and beta\n" {
		t.Error("the resolved content was not applied")
	}
	a.sync()
	converged(t, a, b)
}

func TestSyncRefusesSecrets(t *testing.T) {
	needGit(t)
	a := newMachine(t, "alpha", newRemote(t))
	a.write(mem("p", "oops.md"), "token sk-ant-oat01-"+strings.Repeat("x", 40)+"\n")
	_, err := a.s.Sync()
	var se *SecretsError
	if !errors.As(err, &se) || !reflect.DeepEqual(se.Paths, []string{"home/.claude/projects/-work-p/memory/oops.md"}) {
		t.Fatalf("err = %v", err)
	}
	g := &git{dir: a.s.Repo}
	if n, _ := g.str("rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("something was committed: %s commits", n)
	}
}

// A remote decides what the repo holds, so what it adds outside the
// allowlist is never written; it stays in the repo, and is not deleted by
// this machine's export either (a newer caboose may sync it).
func TestSyncIgnoresUnknownPaths(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.sync()

	for p, data := range map[string]string{
		"home/.claude/.credentials.json":        "{}",
		"home/.claude/new-kind/x.md":            "x",
		"home/.claude/projects/a.b/memory/x.md": "x",
		// A key outside /work, as the repo-relative keys of old were.
		"home/.claude/projects/p/memory/x.md": "x",
	} {
		dst := filepath.Join(a.s.Repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := &git{dir: a.s.Repo}
	if err := g.run("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := g.run("commit", "-q", "-m", "hostile"); err != nil {
		t.Fatal(err)
	}
	if err := g.run("push", "-q", "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatal(err)
	}

	r := b.sync()
	want := []string{"home/.claude/.credentials.json", "home/.claude/new-kind/x.md", "home/.claude/projects/a.b/memory/x.md",
		"home/.claude/projects/p/memory/x.md"}
	if !reflect.DeepEqual(r.Ignored, want) {
		t.Errorf("Ignored = %v, want %v", r.Ignored, want)
	}
	if b.exists(".claude/.credentials.json") || b.exists(".claude/new-kind") {
		t.Error("an unknown path was written")
	}
	b.write(mem("p", "x.md"), "change\n")
	b.sync()
	gb := &git{dir: b.s.Repo}
	for _, p := range want {
		if ok, _ := gb.ok("cat-file", "-e", "HEAD:"+p); !ok {
			t.Errorf("%s was dropped from the repo", p)
		}
	}
}

// A .claude.json that does not parse (Claude Code mid-write) must not read
// as the synced keys having been deleted.
func TestSyncKeepsClaudeJSONThatDoesNotParse(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	a.write(".claude.json", `{"mcpServers":{"x":{}}}`)
	a.sync()
	a.write(".claude.json", `{"mcpServ`)
	a.write(mem("p", "x.md"), "x\n")
	a.sync()
	g := &git{dir: a.s.Repo}
	if ok, _ := g.ok("cat-file", "-e", "HEAD:home/.claude.json"); !ok {
		t.Error("claude.json was deleted from the repo")
	}
}

func TestSyncNoRemote(t *testing.T) {
	needGit(t)
	s := newSyncer(t, "h")
	if _, err := s.Sync(); !errors.Is(err, ErrNoRemote) {
		t.Errorf("err = %v, want ErrNoRemote", err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Errorf("second lock: %v, want ErrLocked", err)
	}
	unlock()
	unlock2, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	unlock2()
}

// A remote can put a .gitattributes in the repo. It is never written to the
// data dir, but git would obey it in the sync repo: here it names filter,
// merge and diff drivers that the user's own git config defines (as
// git-lfs does), asks for CRLF line endings, and turns MEMORY.md's union
// merge off. .git/info/attributes outranks it, so none of that may happen.
func TestSyncIgnoresRemoteGitattributes(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.write(mem("p", "MEMORY.md"), "- one\n")
	a.write(mem("p", "facts.md"), "line 1\nline 2\nline 3\nline 4\nline 5\n")
	a.write(".claude/settings.json", `{"theme":"dark"}`)
	a.sync()
	b.sync()
	a.sync() // takes beta's merge, so the push below is a fast-forward

	g := &git{dir: a.s.Repo}
	hostile := "* filter=evil merge=evil diff=evil text eol=crlf\n" +
		"home/.claude/projects/*/memory/MEMORY.md merge=binary\n"
	if err := os.WriteFile(filepath.Join(a.s.Repo, ".gitattributes"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", ".gitattributes"},
		{"commit", "-q", "-m", "hostile"},
		{"push", "-q", "origin", "HEAD:refs/heads/main"},
	} {
		if err := g.run(args...); err != nil {
			t.Fatal(err)
		}
	}

	// From here on, the user's global config defines every driver the
	// hostile file names, each one a script that leaves a mark when git
	// runs it. (A script, not `sh -c '...; cat'`: in a git config file ';'
	// starts a comment, and a driver cut short fails without a mark.)
	dir := t.TempDir()
	mark := filepath.Join(dir, "ran")
	evil := filepath.Join(dir, "evil")
	if err := os.WriteFile(evil, []byte("#!/bin/sh\necho \"$1\" >> '"+mark+"'\n"+
		"[ \"$1\" = merge ] && exit 1\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte(`[filter "evil"]
	clean = `+evil+` clean
	smudge = `+evil+` smudge
[merge "evil"]
	driver = `+evil+` merge
[diff "evil"]
	command = `+evil+` diff
	textconv = `+evil+` textconv
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)

	a.write(mem("p", "MEMORY.md"), "- one\n- from alpha\n")
	b.write(mem("p", "MEMORY.md"), "- one\n- from beta\n")
	a.write(mem("p", "facts.md"), "line 1 (alpha)\nline 2\nline 3\nline 4\nline 5\n")
	b.write(mem("p", "facts.md"), "line 1\nline 2\nline 3\nline 4\nline 5 (beta)\n")
	b.write(".claude/settings.json", `{"theme":"light"}`)
	a.sync()
	b.sync()
	a.sync()

	if ran, err := os.ReadFile(mark); err == nil {
		t.Errorf("git ran drivers the remote's .gitattributes named:\n%s", ran)
	}
	idx := b.read(mem("p", "MEMORY.md"))
	if !strings.Contains(idx, "from alpha") || !strings.Contains(idx, "from beta") {
		t.Errorf("MEMORY.md lost its union merge:\n%s", idx)
	}
	for _, m := range []*machine{a, b} {
		for p, data := range m.exported() {
			if strings.Contains(data, "\r") {
				t.Errorf("%s: %s gained CRLF line endings", m.s.Host, p)
			}
		}
	}
	if b.exists(".gitattributes") || b.exists(".claude/.gitattributes") {
		t.Error(".gitattributes reached the data dir")
	}
	converged(t, a, b)
}

// The container writes .claude, and could plant links there to the data
// dir's own secrets or to anything on the host: none is read, and what the
// repo had of a path that became one is kept, not deleted everywhere.
func TestSyncRefusesLinksInTheDataDir(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("host file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.write(".claude/.credentials.json", "the claude login\n")
	a.write(mem("p", "kept.md"), "synced before it became a link\n")
	a.write(mem("p", "plain.md"), "plain\n")
	a.sync()
	b.sync()

	dir := func(rel string) string { return a.path(rel) }
	link := func(target, rel string) {
		t.Helper()
		_ = os.Remove(dir(rel))
		if err := os.Symlink(target, dir(rel)); err != nil {
			t.Fatal(err)
		}
	}
	link("../../../.credentials.json", mem("p", "kept.md"))
	link(outside, mem("p", "outside.md"))
	if err := os.Link(dir(".claude/.credentials.json"), dir(mem("p", "hard.md"))); err != nil {
		t.Fatal(err)
	}
	a.write(".claude/skills/real/SKILL.md", "real\n")
	link(filepath.Dir(outside), ".claude/skills/linked")
	if err := os.MkdirAll(dir(".claude/projects/"+ProjectKey("/work/q")), 0o777); err != nil {
		t.Fatal(err)
	}
	link("../"+ProjectKey("/work/p")+"/memory", ".claude/projects/"+ProjectKey("/work/q")+"/memory")
	r := a.sync()
	want := []string{
		".claude/projects/" + ProjectKey("/work/p") + "/memory/hard.md",
		".claude/projects/" + ProjectKey("/work/p") + "/memory/kept.md",
		".claude/projects/" + ProjectKey("/work/p") + "/memory/outside.md",
		".claude/projects/" + ProjectKey("/work/q") + "/memory",
		".claude/skills/linked",
	}
	if !reflect.DeepEqual(r.Refused, want) {
		t.Errorf("refused\n  %q\nwant\n  %q", r.Refused, want)
	}
	b.sync()
	for rel, want := range map[string]string{
		mem("p", "kept.md"):            "synced before it became a link\n",
		mem("p", "plain.md"):           "plain\n",
		".claude/skills/real/SKILL.md": "real\n",
	} {
		if got := b.read(rel); got != want {
			t.Errorf("beta: %s = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{mem("p", "outside.md"), mem("p", "hard.md"), ".claude/skills/linked", mem("q", "x")} {
		if b.exists(rel) {
			t.Errorf("beta got %s", rel)
		}
	}
	for p, data := range b.exported() {
		if strings.Contains(data, "claude login") || strings.Contains(data, "host file") {
			t.Errorf("a secret reached beta, in %s", p)
		}
	}
}

// What a remote sends is written only into plain directories, never
// through a link the data dir has on the way, and never as a link itself.
func TestSyncApplyNeverFollowsLinks(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	outside := t.TempDir()
	secret := filepath.Join(outside, "f.md")
	if err := os.WriteFile(secret, []byte("host file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// beta's memory dir for p is a link out of the data dir, and its
	// settings.json a link to a host file.
	memDir := b.path(filepath.Dir(mem("p", "f.md")))
	if err := os.MkdirAll(filepath.Dir(memDir), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, memDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(b.s.Home, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	a.write(mem("p", "f.md"), "from alpha\n")
	a.write(".claude/settings.json", `{"theme":"dark"}`)
	a.sync()
	r := b.sync()
	if got := string(must(os.ReadFile(secret))); got != "host file\n" {
		t.Errorf("the host file is now %q", got)
	}
	// A link someone made is theirs: left as it is, not replaced.
	if fi, err := os.Lstat(filepath.Join(b.s.Home, ".claude", "settings.json")); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("beta's settings.json link was replaced (err %v)", err)
	}
	for _, want := range []string{mem("p", "f.md"), ".claude/settings.json"} {
		if !slices.Contains(r.Refused, want) {
			t.Errorf("refused %q, want %s among them", r.Refused, want)
		}
	}
	// And the next round does not undo alpha's files.
	b.sync()
	a.sync()
	if got := a.read(mem("p", "f.md")); got != "from alpha\n" {
		t.Errorf("alpha's memory is now %q", got)
	}
	if got := a.read(".claude/settings.json"); got != `{"theme":"dark"}` {
		t.Errorf("alpha's settings are now %q", got)
	}
}

// A symlink committed to the remote (mode 120000) is not checked out as one
// -- core.symlinks=false -- and not applied as anything.
func TestSyncIgnoresRemoteSymlinks(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	target := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(target, []byte("host file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.write(mem("p", "f.md"), "plain\n")
	a.sync()

	// Another client pushes symlinks at synced paths.
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn := func(dir string, stdin string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=x", "-c", "user.email=x@example.invalid"}, args...)...)
		c.Stdin = strings.NewReader(stdin)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "clone", "-q", remote, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	blob := gitIn(clone, target, "hash-object", "-w", "--stdin")
	for _, p := range []string{"home/.claude/settings.json", RepoHome + ".claude/projects/" + ProjectKey("/work/p") + "/memory/link.md"} {
		gitIn(clone, "", "update-index", "--add", "--cacheinfo", "120000,"+blob+","+p)
	}
	gitIn(clone, "", "commit", "-q", "-m", "links")
	gitIn(clone, "", "push", "-q", "origin", "HEAD:refs/heads/"+Branch)

	r := b.sync()
	for _, rel := range []string{".claude/settings.json", mem("p", "link.md")} {
		if b.exists(rel) {
			t.Errorf("beta has %s: %q", rel, b.read(rel))
		}
	}
	if len(r.Ignored) != 2 {
		t.Errorf("ignored %q, want both links", r.Ignored)
	}
	err := filepath.WalkDir(b.s.Repo, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("a symlink in beta's work tree: %s", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	b.write(".claude/settings.json", `{"theme":"light"}`)
	b.sync()
	if got := string(must(os.ReadFile(target))); got != "host file\n" {
		t.Errorf("the host file is now %q", got)
	}
	if got := b.read(mem("p", "f.md")); got != "plain\n" {
		t.Errorf("beta's memory is %q", got)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestMaybeRemote(t *testing.T) {
	needGit(t)
	s := newSyncer(t, "h")
	if MaybeRemote(s.Repo) {
		t.Error("no repo, yet a remote")
	}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if MaybeRemote(s.Repo) {
		t.Error("no remote set, yet a remote")
	}
	if err := s.SetRemote(newRemote(t)); err != nil {
		t.Fatal(err)
	}
	if !MaybeRemote(s.Repo) {
		t.Error("the remote set is not seen")
	}
}

// config writes the machine's sandbox config: the defaults with extra
// appended.
func (m *machine) config(extra string) {
	m.t.Helper()
	m.write(sandboxcfg.Rel, string(sandboxcfg.Default(m.s.Roots))+extra)
}

// A rule added on one machine reaches the others through the sandbox
// config, which syncs: in the same sync, with the files it names.
func TestSyncCarriesNewRules(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.config("")
	a.sync()
	b.sync()

	out, _, err := sandboxcfg.AddSync([]byte(a.read(sandboxcfg.Rel)), "~/.config/git/shared")
	if err != nil {
		t.Fatal(err)
	}
	a.write(sandboxcfg.Rel, string(out))
	a.write(".config/git/shared", "[alias]\n\tst = status\n")
	a.write(".config/git/config", "[user]\n\tsigningkey = alpha's\n")
	a.sync()
	b.write(".config/git/config", "[user]\n\tsigningkey = beta's\n")
	r := b.sync()
	if !r.SandboxConfig {
		t.Error("the sandbox config changing was not reported")
	}
	if got := b.read(".config/git/shared"); got != "[alias]\n\tst = status\n" {
		t.Errorf("beta's git/shared = %q", got)
	}
	if got := b.read(".config/git/config"); !strings.Contains(got, "beta's") {
		t.Errorf("beta's own git config changed: %q", got)
	}
	converged(t, a, b)
}

// A path a machine has only just started syncing is taken from the repo,
// not read as deleted there because the machine does not have it yet.
func TestSyncAdoptsNewlySyncedPaths(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	extra := "\n[[keep]]\npath = \"~/.tool\"\nsync = true\n"
	a.config(extra)
	a.write(".tool/settings", "from alpha\n")
	a.sync()
	// Beta does not sync ~/.tool, nor the sandbox config: it keeps its own.
	b.config("")
	if out, _, err := sandboxcfg.RemoveSync([]byte(b.read(sandboxcfg.Rel)), "~/.config/caboose"); err != nil {
		t.Fatal(err)
	} else {
		b.write(sandboxcfg.Rel, string(out))
	}
	r := b.sync()
	if b.exists(".tool/settings") || !slices.Contains(r.Ignored, "home/.tool/settings") {
		t.Errorf("beta took a path it does not sync (ignored %q)", r.Ignored)
	}
	// Now it does: the file comes over, and stays on the remote.
	cfg := b.read(sandboxcfg.Rel)
	b.write(sandboxcfg.Rel, cfg+extra)
	b.sync()
	if got := b.read(".tool/settings"); got != "from alpha\n" {
		t.Errorf("beta's .tool/settings = %q", got)
	}
	a.sync()
	if got := a.read(".tool/settings"); got != "from alpha\n" {
		t.Errorf("alpha's .tool/settings = %q", got)
	}
	// And a deletion after that is one.
	if err := os.Remove(b.path(".tool/settings")); err != nil {
		t.Fatal(err)
	}
	b.sync()
	a.sync()
	if a.exists(".tool/settings") {
		t.Error("beta's deletion did not reach alpha")
	}
}

// A sandbox config that cannot be used stops the sync before anything is
// committed: its rules are what say what syncs.
func TestSyncRefusesABrokenSandboxConfig(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	for _, bad := range []string{"[[keep]\n", "format = 99\n"} {
		a.write(sandboxcfg.Rel, bad)
		a.s.Sandbox = nil
		if _, err := a.s.Sync(); !errors.Is(err, ErrSandboxConfig) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	g := &git{dir: a.s.Repo}
	if n, _ := g.str("rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("something was committed: %s commits", n)
	}
}

// A rule that only ever matches what an earlier one took is reported.
func TestSyncReportsShadowedRules(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	a.config("\n[[keep]]\npath = \"~/.tool\"\n  [[keep.sync]]\n  path = \"~/.tool\"\n  [[keep.sync]]\n  path = \"~/.tool/state.json\"\n  merge = \"json\"\n")
	a.write(".tool/state.json", "{}")
	r := a.sync()
	if !reflect.DeepEqual(r.Shadowed, []string{"~/.tool/state.json"}) {
		t.Errorf("Shadowed = %q", r.Shadowed)
	}
}

// Project memory follows the sandbox config's roots wherever they are: a
// root with a path of its own syncs its projects' memory, a key that only
// starts like a root's does not, and a machine takes a root's memory once
// its sandbox config lists the root.
func TestSyncFollowsTheRoots(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.s.Roots, a.s.Sandbox = []string{"/opt/x", "/work/dev"}, nil
	b.s.Roots, b.s.Sandbox = []string{"/work/dev"}, nil
	memory := func(project string) string { return ".claude/projects/" + ProjectKey(project) + "/memory/m.md" }
	synced := []string{memory("/opt/x"), memory("/opt/x/app"), memory("/work/dev/p")}
	left := []string{memory("/opt/xy/app"), memory("/workspace/p"), memory("/work/other"), memory("/work")}
	for _, p := range append(slices.Clone(synced), left...) {
		a.write(p, p+"\n")
	}
	exported := a.exported()
	for _, p := range synced {
		if _, ok := exported[RepoHome+p]; !ok {
			t.Errorf("alpha does not export %s", p)
		}
	}
	for _, p := range left {
		if _, ok := exported[RepoHome+p]; ok {
			t.Errorf("alpha exports %s, under none of its roots", p)
		}
	}
	a.sync()

	// Beta has no root at /opt/x: that memory stays in the repo.
	r := b.sync()
	if b.read(memory("/work/dev/p")) != memory("/work/dev/p")+"\n" {
		t.Errorf("beta lacks %s", memory("/work/dev/p"))
	}
	if b.exists(memory("/opt/x/app")) || !slices.Contains(r.Ignored, RepoHome+memory("/opt/x/app")) {
		t.Errorf("beta took memory under a root it does not list (ignored %q)", r.Ignored)
	}

	// Once its sandbox config lists the root, the memory comes over.
	b.s.Roots = a.s.Roots
	b.config("")
	b.sync()
	for _, p := range synced {
		if got := b.read(p); got != p+"\n" {
			t.Errorf("beta's %s = %q", p, got)
		}
	}
	for _, p := range left {
		if b.exists(p) {
			t.Errorf("beta has %s", p)
		}
	}
	a.sync()
	if !a.exists(memory("/opt/x/app")) {
		t.Error("alpha lost memory beta adopted")
	}
}

// A keep entry the sandbox does not mount yet -- added since it was
// created -- syncs nothing: what were written there would be lost with the
// sandbox. Once a restart mounts it, its files come from the repo, and are
// not read as deleted by the machine that had never had them.
func TestSyncSkipsUnmountedEntries(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	extra := "\n[[keep]]\npath = \"~/.tool\"\nsync = true\n"
	a.config(extra)
	a.write(".tool/settings", "from alpha\n")
	a.sync()

	// Beta's sandbox config comes over in this sync, with the new entry
	// in it, which beta's sandbox does not mount.
	b.s.Mounted = []string{".claude", ".claude.json", ".config/caboose"}
	r := b.sync()
	if !r.SandboxConfig {
		t.Error("the sandbox config was not taken")
	}
	if b.exists(".tool/settings") {
		t.Error("written into an entry the sandbox does not mount")
	}
	if !slices.Equal(r.Unmounted, []string{".tool"}) {
		t.Errorf("unmounted %q, want .tool", r.Unmounted)
	}
	if slices.Contains(r.Ignored, "home/.tool/settings") {
		t.Errorf("an unmounted entry's file reported as not synced here: %q", r.Ignored)
	}
	// What lands there meanwhile is not sent either.
	b.write(".tool/other", "beta's, unmounted\n")
	if r := b.sync(); r.Committed || !slices.Equal(r.Unmounted, []string{".tool"}) {
		t.Errorf("committed %v, unmounted %q", r.Committed, r.Unmounted)
	}
	if err := os.RemoveAll(b.path(".tool")); err != nil {
		t.Fatal(err)
	}

	// The restart mounts it, empty.
	b.s.Mounted = append(b.s.Mounted, ".tool")
	r = b.sync()
	if got := b.read(".tool/settings"); got != "from alpha\n" {
		t.Errorf("after the restart beta's .tool/settings = %q", got)
	}
	if len(r.Unmounted) > 0 {
		t.Errorf("unmounted %q after the restart", r.Unmounted)
	}
	a.sync()
	if got := a.read(".tool/settings"); got != "from alpha\n" {
		t.Errorf("alpha's .tool/settings = %q", got)
	}
	converged(t, a, b)
}

// A remote that never answers is given up on when the budget is spent,
// and whatever git started for it is killed with it.
func TestSyncBudgetStopsAHungRemote(t *testing.T) {
	needGit(t)
	a := newMachine(t, "alpha", "ssh://git.example.invalid/state.git")
	pidfile := filepath.Join(t.TempDir(), "pid")
	a.s.Env = []string{"GIT_SSH_COMMAND=echo $$ > '" + pidfile + "'; exec sleep 30;"}
	a.s.Budget = time.Second
	a.s.g = nil // made with the settings above
	start := time.Now()
	_, err := a.s.Sync()
	if !errors.Is(err, ErrNoAnswer) || err.Error() != "no answer from the remote in 1s" {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	data, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	// Gone, or a zombie nobody reaps for a moment: not sleeping on.
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil || strings.Contains(string(stat), ") Z ") {
			break
		}
		if runtime.GOOS != "linux" || time.Now().After(deadline) {
			t.Errorf("the remote's ssh (pid %d) still runs", pid)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A sync stopped after taking the remote's changes and before all were in
// the home has the next sync finish them before it exports: the files not
// written yet would otherwise read as this machine's, and undo the remote's
// everywhere.
func TestSyncFinishesAnInterruptedApply(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "alpha", remote)
	b := newMachine(t, "beta", remote)
	a.write(mem("p", "f.md"), "old\n")
	a.sync()
	b.sync()
	a.write(mem("p", "f.md"), "new\n")
	a.write(mem("q", "g.md"), "added\n")
	a.sync()

	// Beta's sync stopped right after it took them, before writing any:
	// HEAD moved, the marker written, the home as it was.
	g := &git{dir: b.s.Repo}
	old, err := g.str("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.run("fetch", "-q", "origin"); err != nil {
		t.Fatal(err)
	}
	if err := g.run("merge", "-q", "--ff-only", "refs/remotes/origin/"+Branch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.s.Repo, applyingFile), []byte(old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.s.g = nil
	r := b.sync()
	if got := b.read(mem("q", "g.md")); got != "added\n" {
		t.Errorf("beta's g.md = %q", got)
	}
	if got := b.read(mem("p", "f.md")); got != "new\n" {
		t.Errorf("beta's f.md = %q", got)
	}
	if !r.Merged {
		t.Error("the finished apply is not reported")
	}
	a.sync()
	if got := a.read(mem("q", "g.md")); got != "added\n" {
		t.Errorf("alpha's g.md = %q: beta's unfinished apply was sent as a deletion", got)
	}
	if got := a.read(mem("p", "f.md")); got != "new\n" {
		t.Errorf("alpha's f.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(b.s.Repo, applyingFile)); err == nil {
		t.Error("the apply marker was left")
	}
}

// A sync whose Ctx is done stops: a command waiting on the remote is
// killed, with what it started, and nothing more is begun.
func TestSyncStopsWithItsContext(t *testing.T) {
	needGit(t)
	a := newMachine(t, "alpha", "ssh://git.example.invalid/state.git")
	pidfile := filepath.Join(t.TempDir(), "pid")
	a.s.Env = []string{"GIT_SSH_COMMAND=echo $$ > '" + pidfile + "'; exec sleep 30;"}
	ctx, cancel := context.WithCancel(context.Background())
	a.s.Ctx = ctx
	a.s.g = nil
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	if _, err := a.s.Sync(); !errors.Is(err, ErrStopped) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil || strings.Contains(string(stat), ") Z ") {
			break
		}
		if runtime.GOOS != "linux" || time.Now().After(deadline) {
			t.Errorf("the remote's ssh (pid %d) still runs", pid)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}
