package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// sparse makes the file at p of size bytes, holding head first, without
// writing the rest: what the sandbox can make at no cost to itself.
func sparse(t *testing.T, p, head string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, size); err != nil {
		t.Fatal(err)
	}
}

// A sandbox config over its limit is refused before it is read, and the
// last good copy stands in.
func TestLoadSandboxConfigTooLarge(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureLayout(dir, defKeep()); err != nil {
		t.Fatal(err)
	}
	good := sandboxcfg.Default([]string{"/work"})
	sparse(t, filepath.Join(dir, SandboxConfig), string(good), int64(len(good)))
	if sb, err := LoadSandboxConfig(dir, nil); err != nil || sb.Err != nil {
		t.Fatalf("a good one: %+v, %v", sb, err)
	}
	sparse(t, filepath.Join(dir, SandboxConfig), string(good), sandboxcfg.MaxSize+1)
	sb, err := LoadSandboxConfig(dir, nil)
	if err != nil || !errors.Is(sb.Err, nofollow.ErrTooLarge) || !sb.LastGood {
		t.Fatalf("%+v, %v", sb, err)
	}
}

// A file in ~/.ssh far larger than any key is not read: it is no key ssh
// would use.
func TestPrivateKeysInSkipsLargeFiles(t *testing.T) {
	dir := t.TempDir()
	sparse(t, filepath.Join(dir, SSHDir, "big"), "-----BEGIN OPENSSH PRIVATE KEY-----\n", maxKeyFile+1)
	sparse(t, filepath.Join(dir, SSHDir, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\n", maxKeyFile)
	keys, err := PrivateKeysIn(dir)
	if err != nil || len(keys) != 1 || keys[0] != "id_ed25519" {
		t.Errorf("keys %q, %v", keys, err)
	}
}

// A sandbox git config over its limit is not read, nor written.
func TestSandboxGitTooLarge(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureLayout(dir, defKeep()); err != nil {
		t.Fatal(err)
	}
	sparse(t, filepath.Join(dir, GitConfig), "[user]\n\tname = x\n", MaxGitConfig+1)
	called := false
	err := editConfig(dir, func(string) error { called = true; return nil })
	if !errors.Is(err, nofollow.ErrTooLarge) || called {
		t.Errorf("editConfig = %v, called %v", err, called)
	}
}
