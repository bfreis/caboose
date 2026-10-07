package statesync

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func (m *machine) pending() *Pending {
	m.t.Helper()
	p, err := m.s.Pending()
	if err != nil {
		m.t.Fatal(err)
	}
	return p
}

func (m *machine) divergence() Divergence {
	m.t.Helper()
	d, err := m.s.Divergence()
	if err != nil {
		m.t.Fatal(err)
	}
	return d
}

func TestPendingAgainstTheLastSync(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "a", remote)
	a.write(mem("p", "MEMORY.md"), "- one\n")
	a.write(mem("p", "gone.md"), "old\n")
	a.write(".claude/skills/s/run.sh", "echo\n")
	a.write(".claude/settings.json", `{"x":1}`)

	// Before any sync, everything live is waiting to go.
	if p := a.pending(); len(p.Changed) != 4 || len(p.Deleted) != 0 {
		t.Fatalf("before the first sync: changed %v, deleted %v", p.Changed, p.Deleted)
	}
	a.sync()
	if p := a.pending(); p.Any() {
		t.Fatalf("right after a sync: changed %v, deleted %v", p.Changed, p.Deleted)
	}

	a.write(mem("p", "MEMORY.md"), "- one\n- two\n")
	a.write(mem("q", "new.md"), "new\n")
	if err := os.Remove(a.path(mem("p", "gone.md"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(a.s.Home, ".claude/skills/s/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A path another machine (or a newer caboose) put in the repo is not
	// this machine's to delete, and does not read as deleted.
	if err := a.s.repo().WriteFile("future/thing", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := a.pending()
	wantChanged := []string{
		"home/.claude/projects/" + ProjectKey("/work/p") + "/memory/MEMORY.md",
		"home/.claude/projects/" + ProjectKey("/work/q") + "/memory/new.md",
		"home/.claude/skills/s/run.sh",
	}
	wantDeleted := []string{"home/.claude/projects/" + ProjectKey("/work/p") + "/memory/gone.md"}
	if !reflect.DeepEqual(p.Changed, wantChanged) || !reflect.DeepEqual(p.Deleted, wantDeleted) {
		t.Fatalf("changed %v, deleted %v; want %v, %v", p.Changed, p.Deleted, wantChanged, wantDeleted)
	}
	if p.Export == nil || p.Export.Files[wantChanged[0]].Data == nil {
		t.Fatal("Pending does not hand back the export it compared")
	}
}

func TestPendingReadsNoLinkInTheRepo(t *testing.T) {
	needGit(t)
	a := newMachine(t, "a", newRemote(t))
	a.write(".claude/settings.json", `{"x":1}`)
	a.sync()
	// The container writes the repo: a symlink standing in for a synced
	// file is not followed, and reads as changed.
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(a.s.Repo, "home/.claude/settings.json")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, target); err != nil {
		t.Fatal(err)
	}
	if p := a.pending(); !reflect.DeepEqual(p.Changed, []string{"home/.claude/settings.json"}) {
		t.Fatalf("changed %v, want the linked file", p.Changed)
	}
}

func TestPendingKeepsWhatItCannotRead(t *testing.T) {
	needGit(t)
	a := newMachine(t, "a", newRemote(t))
	a.write(mem("r", "MEMORY.md"), "- one\n")
	a.sync()
	// The memory dir is now a symlink: refused, never read, and what the
	// repo has of it is kept -- not a deletion waiting to be sent.
	dir := a.path(filepath.Dir(mem("r", "MEMORY.md")))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	p := a.pending()
	if p.Any() || len(p.Export.Refused) != 1 {
		t.Fatalf("changed %v, deleted %v, refused %v", p.Changed, p.Deleted, p.Export.Refused)
	}
}

func TestDivergence(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "a", remote)
	b := newMachine(t, "b", remote)

	if d := a.divergence(); d.Remote {
		t.Fatalf("an empty remote: %+v", d)
	}
	a.write(".claude/settings.json", `{"a":1}`)
	a.sync()
	b.sync()
	if d := a.divergence(); d != (Divergence{Remote: true}) {
		t.Fatalf("after a sync: %+v", d)
	}

	b.write(".claude/settings.json", `{"a":2}`)
	b.sync()
	// Not known until fetched: Divergence itself never reaches the remote.
	if d := a.divergence(); d.Behind != 0 {
		t.Fatalf("before a fetch: %+v", d)
	}
	if err := a.s.Fetch(); err != nil {
		t.Fatal(err)
	}
	// b's commit, and the ones it merged: Behind counts commits.
	behind := a.divergence()
	if !behind.Remote || behind.Ahead != 0 || behind.Behind == 0 {
		t.Fatalf("after b's sync and a fetch: %+v", behind)
	}

	// A commit the remote never got: a push that failed.
	if err := a.s.git().run("commit", "-q", "--allow-empty", "-m", "unsent"); err != nil {
		t.Fatal(err)
	}
	if d := a.divergence(); d != (Divergence{Remote: true, Ahead: 1, Behind: behind.Behind}) {
		t.Fatalf("with a commit not pushed: %+v", d)
	}
}

// Unsent is what a failed push left here: nothing after a sync that got
// through, and the files of a commit that did not; while the remote has no
// branch at all, all of HEAD.
func TestUnsent(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "a", remote)
	unsent := func() Unsent {
		t.Helper()
		u, err := a.s.Unsent()
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// Init's commit, never pushed: no files in it.
	if u := unsent(); len(u.Paths) != 0 {
		t.Fatalf("before any sync: %+v", u)
	}
	// A commit with files, the remote never having had the branch: a first
	// push that failed.
	commit := func(rel, data string) {
		t.Helper()
		p := filepath.Join(a.s.Repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := a.s.git().run("add", "-A"); err != nil {
			t.Fatal(err)
		}
		if err := a.s.git().run("commit", "-q", "-m", "unsent"); err != nil {
			t.Fatal(err)
		}
	}
	commit("home/.claude/agents/early.md", "x")
	if u := unsent(); u.Commits != 2 || strings.Join(u.Paths, ",") != "home/.claude/agents/early.md" {
		t.Fatalf("with no remote branch: %+v", u)
	}

	a.write(".claude/settings.json", `{"a":1}`)
	a.sync()
	if u := unsent(); u.Commits != 0 || len(u.Paths) != 0 {
		t.Fatalf("after a sync that pushed: %+v", u)
	}

	commit("home/.claude/agents/x.md", "x")
	commit("home/.claude/agents/y.md", "y")
	if u := unsent(); u.Commits != 2 || strings.Join(u.Paths, ",") != "home/.claude/agents/x.md,home/.claude/agents/y.md" {
		t.Fatalf("with two commits not pushed: %+v", u)
	}
}

func TestDirty(t *testing.T) {
	needGit(t)
	a := newMachine(t, "a", newRemote(t))
	a.write(".claude/settings.json", `{"x":1}`)
	a.sync()
	if dirty, err := a.s.Dirty(); err != nil || dirty {
		t.Fatalf("after a sync: dirty %v, %v", dirty, err)
	}
	// A sync that died after writing the export, before committing it.
	if err := a.s.repo().WriteFile("home/.claude/settings.json", []byte(`{"x":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty, err := a.s.Dirty(); err != nil || !dirty {
		t.Fatalf("with the work tree written: dirty %v, %v", dirty, err)
	}
}

func TestRemoteHint(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "a", remote)
	if got := RemoteHint(a.s.Repo); got != remote {
		t.Errorf("RemoteHint = %q, want %q", got, remote)
	}
	// Another remote's url is not origin's.
	if err := a.s.git().run("remote", "add", "zzz", "https://example.invalid/other.git"); err != nil {
		t.Fatal(err)
	}
	if err := a.s.git().run("remote", "remove", "origin"); err != nil {
		t.Fatal(err)
	}
	if got := RemoteHint(a.s.Repo); got != "" {
		t.Errorf("with no origin: RemoteHint = %q", got)
	}
	if got := RemoteHint(t.TempDir()); got != "" {
		t.Errorf("with no repo: RemoteHint = %q", got)
	}
}

// A machine that joins a remote others have synced with has a history of
// its own, unrelated to the remote's until its first sync merges them:
// everything the remote holds is what it would take.
func TestIncomingOnAMachineThatJustJoined(t *testing.T) {
	needGit(t)
	remote := newRemote(t)
	a := newMachine(t, "a", remote)
	a.write(".claude/settings.json", `{"a":1}`)
	a.sync()

	b := newMachine(t, "b", remote)
	if err := b.s.Fetch(); err != nil {
		t.Fatal(err)
	}
	taken, _, err := b.s.Incoming()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(taken, "home/.claude/settings.json") {
		t.Fatalf("taken %v, want a's settings.json", taken)
	}

	// Once joined, only what changed since they parted.
	b.sync()
	a.write(mem("p", "MEMORY.md"), "- one\n")
	a.sync()
	if err := b.s.Fetch(); err != nil {
		t.Fatal(err)
	}
	if taken, _, err = b.s.Incoming(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"home/" + mem("p", "MEMORY.md")}; !reflect.DeepEqual(taken, want) {
		t.Fatalf("taken %v, want %v", taken, want)
	}
}
