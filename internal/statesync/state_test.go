package statesync

import (
	"os"
	"path/filepath"
	"reflect"
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
	if err := os.Remove(filepath.Join(a.s.DataDir, filepath.FromSlash(mem("p", "gone.md")))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(a.s.DataDir, ".claude/skills/s/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A path another machine (or a newer caboose) put in the repo is not
	// this machine's to delete, and does not read as deleted.
	if err := a.s.repo().WriteFile("future/thing", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := a.pending()
	wantChanged := []string{
		"claude/projects/" + ProjectKey("/work/p") + "/memory/MEMORY.md",
		"claude/projects/" + ProjectKey("/work/q") + "/memory/new.md",
		"claude/skills/s/run.sh",
	}
	wantDeleted := []string{"claude/projects/" + ProjectKey("/work/p") + "/memory/gone.md"}
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
	target := filepath.Join(a.s.RepoDir(), "claude/settings.json")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, target); err != nil {
		t.Fatal(err)
	}
	if p := a.pending(); !reflect.DeepEqual(p.Changed, []string{"claude/settings.json"}) {
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
	dir := filepath.Join(a.s.DataDir, filepath.FromSlash(filepath.Dir(mem("r", "MEMORY.md"))))
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

func TestDirty(t *testing.T) {
	needGit(t)
	a := newMachine(t, "a", newRemote(t))
	a.write(".claude/settings.json", `{"x":1}`)
	a.sync()
	if dirty, err := a.s.Dirty(); err != nil || dirty {
		t.Fatalf("after a sync: dirty %v, %v", dirty, err)
	}
	// A sync that died after writing the export, before committing it.
	if err := a.s.repo().WriteFile("claude/settings.json", []byte(`{"x":2}`), 0o644); err != nil {
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
	if got := a.s.RemoteHint(); got != remote {
		t.Errorf("RemoteHint = %q, want %q", got, remote)
	}
	// Another remote's url is not origin's.
	if err := a.s.git().run("remote", "add", "zzz", "https://example.invalid/other.git"); err != nil {
		t.Fatal(err)
	}
	if err := a.s.git().run("remote", "remove", "origin"); err != nil {
		t.Fatal(err)
	}
	if got := a.s.RemoteHint(); got != "" {
		t.Errorf("with no origin: RemoteHint = %q", got)
	}
	if got := (&Syncer{DataDir: t.TempDir()}).RemoteHint(); got != "" {
		t.Errorf("with no repo: RemoteHint = %q", got)
	}
}
