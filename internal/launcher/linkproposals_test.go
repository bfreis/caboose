package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/proposal"
)

func writeProposalAt(t *testing.T, dataDir, file, body string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(dataDir, proposal.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func rootBody(title string) string {
	return "title = \"" + title + "\"\nreason = \"r\"\n[roots]\nother = \"~/src/other\"\n"
}

// A proposal found at the start was told of by the launch; one written
// later is told of once it holds still, naming the environment's apply.
func TestProposalWatchTellsOfNewOnes(t *testing.T) {
	dd := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	writeProposalAt(t, dd, "old.toml", rootBody("Old one"), t0)
	w := newProposalWatch(dd, "work")
	now := t0.Add(time.Hour)
	if _, _, ok := w.check(now); ok {
		t.Fatal("told of a proposal that was there at the start")
	}
	writeProposalAt(t, dd, "foo.toml", rootBody("Mount other"), t0.Add(time.Minute))
	if _, _, ok := w.check(now); ok {
		t.Fatal("told of a proposal on the first poll that saw it")
	}
	title, text, ok := w.check(now.Add(time.Second))
	if !ok {
		t.Fatal("not told of a new proposal")
	}
	if !strings.Contains(title, "proposes a change") || !strings.Contains(text, "Mount other") ||
		!strings.Contains(text, "'caboose -e work apply'") || strings.Contains(text, "Old one") {
		t.Errorf("title %q, text %q", title, text)
	}
	if _, _, ok := w.check(now.Add(time.Hour)); ok {
		t.Error("told of the same proposal twice")
	}
	// Rewritten, it is new again.
	writeProposalAt(t, dd, "foo.toml", rootBody("Mount other, again"), t0.Add(2*time.Minute))
	w.check(now.Add(2 * time.Hour))
	if _, text, ok := w.check(now.Add(2*time.Hour + time.Second)); !ok || !strings.Contains(text, "again") {
		t.Errorf("a rewritten proposal: %v %q", ok, text)
	}
}

// However many arrive, one notification per proposalNotifyEvery; what
// came in between is named in the next.
func TestProposalWatchIsRateLimited(t *testing.T) {
	dd := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	w := newProposalWatch(dd, "default")
	now := t0.Add(time.Hour)
	writeProposalAt(t, dd, "a.toml", rootBody("A"), t0)
	w.check(now)
	if _, text, ok := w.check(now.Add(time.Second)); !ok || !strings.Contains(text, "'caboose apply'") {
		t.Fatalf("first: %v %q", ok, text)
	}
	for _, f := range []string{"b", "c", "d", "e"} {
		writeProposalAt(t, dd, f+".toml", rootBody(strings.ToUpper(f)), t0)
	}
	w.check(now.Add(2 * time.Second))
	if _, _, ok := w.check(now.Add(3 * time.Second)); ok {
		t.Fatal("a second notification within proposalNotifyEvery")
	}
	title, text, ok := w.check(now.Add(time.Second + proposalNotifyEvery))
	if !ok || !strings.Contains(title, "4 changes") || !strings.Contains(text, "B; C; D; and 1 more") {
		t.Errorf("after the wait: %v %q %q", ok, title, text)
	}
}

// What the sandbox wrote is never trusted: a file that is no proposal is
// named by its file, and a symlink is not followed.
func TestProposalWatchUntrustedFiles(t *testing.T) {
	dd := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	w := newProposalWatch(dd, "default")
	now := t0.Add(time.Hour)
	writeProposalAt(t, dd, "bad.toml", "title = \"x\x1b[2J\"\n", t0)
	secret := filepath.Join(t.TempDir(), "secret.toml")
	if err := os.WriteFile(secret, []byte(rootBody("Secret title")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dd, proposal.Dir, "link.toml")); err != nil {
		t.Fatal(err)
	}
	w.check(now)
	_, text, ok := w.check(now.Add(time.Second))
	if !ok {
		t.Fatal("not told")
	}
	if strings.Contains(text, "Secret title") || strings.ContainsRune(text, 0x1b) || !strings.Contains(text, "bad (not one caboose can apply)") {
		t.Errorf("text %q", text)
	}
}
