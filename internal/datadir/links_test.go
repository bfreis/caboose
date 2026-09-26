package datadir

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
)

// The container writes .claude and dot_config/git, and can leave a link
// where the launcher writes on every launch. Through one, the launcher would
// overwrite a host file -- or the data dir's own credential -- with the
// instructions or a git config.

func hostFile(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallInstructionsNeverFollowsLinks(t *testing.T) {
	roots := []config.Root{{Host: "/r", Container: "/work"}}
	for name, plant := range map[string]func(t *testing.T, dir, dst string) string{
		"symlink": func(t *testing.T, dir, dst string) string {
			host := hostFile(t, "host file\n")
			if err := os.Symlink(host, dst); err != nil {
				t.Fatal(err)
			}
			return host
		},
		"hard link": func(t *testing.T, dir, dst string) string {
			cred := filepath.Join(dir, ".claude", ".credentials.json")
			if err := os.WriteFile(cred, []byte("host file\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(cred, dst); err != nil {
				t.Fatal(err)
			}
			return cred
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := EnsureLayout(dir); err != nil {
				t.Fatal(err)
			}
			target := plant(t, dir, filepath.Join(dir, ".claude", "CLAUDE.md"))
			changed, err := InstallInstructions([]byte("instructions\n"), "", roots, dir)
			if changed || !errors.Is(err, nofollow.ErrNotPlain) {
				t.Errorf("InstallInstructions = %v, %v; want ErrNotPlain", changed, err)
			}
			if got := read(t, target); got != "host file\n" {
				t.Errorf("the linked file is now %q", got)
			}
		})
	}
}

// Against the real git: `git config --file` writes through a symlink.
func TestWriteSandboxGitNeverFollowsLinks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, GitConfig)
	host := hostFile(t, "[user]\n\tname = Host File\n")
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(host, dst); err != nil {
		t.Fatal(err)
	}
	change := []Change{{Key: "user.email", Value: "someone@example.invalid"}}
	if err := WriteSandboxGit(dir, FindGit(), change); !errors.Is(err, nofollow.ErrNotPlain) {
		t.Errorf("WriteSandboxGit = %v; want ErrNotPlain", err)
	}
	if s, err := ReadSandboxGit(dir, FindGit()); s.Name != "" || !errors.Is(err, nofollow.ErrNotPlain) {
		t.Errorf("ReadSandboxGit = %+v, %v; want ErrNotPlain", s, err)
	}
	if got := read(t, host); got != "[user]\n\tname = Host File\n" {
		t.Errorf("the host file is now %q", got)
	}

	// And a plain one is written, in place of the link's absence.
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := WriteSandboxGit(dir, FindGit(), change); err != nil {
		t.Fatal(err)
	}
	if got := (ExecGit{Path: "git"}).Get(dst, "user.email"); got != "someone@example.invalid" {
		t.Errorf("user.email = %q", got)
	}
}
