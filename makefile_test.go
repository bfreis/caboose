package caboose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVersionStampSettlesWithoutCommits runs the Makefile's version stamp
// rule, and only that rule (no Go build), where git has nothing to say: no
// repo at all, and a repo with no commits yet. The stamp must be written once
// and then left alone -- a stamp ending in empty lines, which `$(cat)`
// strips, would never compare equal, and ./caboose would relink on every
// make. And an
// empty repo must not stamp the literal "HEAD" that `git rev-parse HEAD`
// echoes there.
func TestVersionStampSettlesWithoutCommits(t *testing.T) {
	for _, tool := range []string{"make", "git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		init bool
	}{{"no git", false}, {"no commits", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Keep git from finding a repo above the temp dir, or one a
			// GIT_DIR in the environment points at.
			env := []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "GIT_") && !strings.HasPrefix(e, "MAKEFLAGS=") {
					env = append(env, e)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.init {
				cmd := exec.Command("git", "-C", dir, "init", "-q")
				cmd.Env = env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v\n%s", err, out)
				}
			}
			launcher := filepath.Join(dir, "c")
			stamp := launcher + ".version"
			makeStamp := func() {
				t.Helper()
				cmd := exec.Command("make", "-s", "LAUNCHER="+launcher, stamp)
				cmd.Dir, cmd.Env = dir, env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("make: %v\n%s", err, out)
				}
			}

			makeStamp()
			if b, err := os.ReadFile(stamp); err != nil || string(b) != "dev\n" {
				t.Fatalf("stamp = %q, %v; want \"dev\\n\"", b, err)
			}
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(stamp, old, old); err != nil {
				t.Fatal(err)
			}
			makeStamp()
			if fi, err := os.Stat(stamp); err != nil || !fi.ModTime().Equal(old) {
				t.Errorf("an unchanged version rewrote the stamp, which relinks ./caboose")
			}
		})
	}
}
