package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A directory inside the store is refused under every second name an
// unprivileged process can give it: a symlink, a chain of them, a ".."
// taken after a symlink (which a lexical clean would get wrong), and, on
// Linux, a /proc/self/fd magic link. What is not inside it is not refused,
// even when its path is spelt like the store's. (A bind mount, which needs
// privileges, is not covered.)
func TestCheckReachAliasesInsideTheStore(t *testing.T) {
	s := newStore(t)
	inner := filepath.Join(s.Dir(), "k-0000aaaa", "20261018T120000Z", TreeName, "sub")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	links := t.TempDir()
	alias := filepath.Join(links, "alias")
	mustSymlink(t, inner, alias)
	chain := filepath.Join(links, "chain")
	mustSymlink(t, alias, chain)
	// Elsewhere, a tree spelt like the store's, and a link to it.
	lookalike := filepath.Join(t.TempDir(), "envs", "default", "checkpoints", "k-0000aaaa")
	if err := os.MkdirAll(lookalike, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(links, "other")
	mustSymlink(t, lookalike, other)

	cases := []struct {
		name    string
		path    string
		exposed bool
	}{
		{"a symlink to a directory inside", alias, true},
		{"a chain of symlinks", chain, true},
		{"below a symlink", filepath.Join(chain, "."), true},
		{"dot-dot after a symlink", filepath.Join(links, "alias") + "/..", true},
		{"two dot-dots after a symlink", filepath.Join(links, "alias") + "/../..", true},
		{"spelt like the store, elsewhere", lookalike, false},
		{"a symlink to that", other, false},
		{"the links' own directory", links, false},
	}
	if runtime.GOOS == "linux" {
		if f, err := os.Open(inner); err == nil {
			defer f.Close()
			cases = append(cases, struct {
				name    string
				path    string
				exposed bool
			}{"a /proc/self/fd magic link", fmt.Sprintf("/proc/self/fd/%d", f.Fd()), true})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.CheckReach([]Reach{{Path: tc.path, What: "root R of environment F"}})
			if got := errors.Is(err, ErrExposed); got != tc.exposed {
				t.Fatalf("%s: exposed %v (%v), want %v", tc.path, got, err, tc.exposed)
			}
		})
	}
}

// A reach path that cannot be looked at is an error, never taken as
// reaching nothing: the check fails closed. One that does not exist
// reaches nothing.
func TestCheckReachFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	s := newStore(t)
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(filepath.Join(locked, "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	err := s.CheckReach([]Reach{{Path: filepath.Join(locked, "root"), What: "root R"}})
	if err == nil || errors.Is(err, ErrExposed) {
		t.Fatalf("an unreadable reach path: %v, want an error that is not ErrExposed", err)
	}
	if err := s.CheckReach([]Reach{{Path: filepath.Join(t.TempDir(), "missing"), What: "root M"}}); err != nil {
		t.Fatalf("a missing reach path: %v", err)
	}
}
