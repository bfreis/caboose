package launcher

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/backend"
)

// lockOfURLs writes a lock at path of a package at each url.
func lockOfURLs(t *testing.T, path string, urls ...string) {
	t.Helper()
	type pkg struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		Version      string `json:"version"`
		Architecture string `json:"architecture"`
	}
	var pkgs []pkg
	for _, u := range urls {
		pkgs = append(pkgs, pkg{"p", u, "1-r0", "aarch64"})
	}
	b, err := json.Marshal(map[string]any{
		"version":  "v1",
		"config":   map[string]string{"name": apkobuild.ConfigName(apkobuild.Spec{Packages: []string{"p"}}, []string{"p"})},
		"contents": map[string]any{"packages": pkgs},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// caboose prune removes from the package cache, on the host, the packages
// no environment's lock names, and says what that freed in a line; a lock
// it cannot read keeps the whole cache, and says why. The versions are
// pruned either way.
func TestPruneApkCache(t *testing.T) {
	b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
	b.mountLocal("local/linux-arm64")
	mkdirs(t, b.data, "local/linux-arm64/share/claude", "local/linux-arm64/bin")
	home := filepath.Join(b.tmp, "caboose-home")
	b.Cfg.CabooseHome, b.Cfg.Home = home, b.tmp
	cache := filepath.Join(home, "cache", "apk")
	arch := filepath.Join(cache, "https%3A%2F%2Fpackages.example.invalid%2Fos", "aarch64")
	for name, content := range map[string]string{"kept-1-r0/x.dat.tar.gz": "kept", "mine-2-r0/x.dat.tar.gz": "mine", "gone-1-r3/x.dat.tar.gz": "12345"} {
		p := filepath.Join(arch, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * apkCacheRecent)
	if err := filepath.WalkDir(cache, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, old, old)
	}); err != nil {
		t.Fatal(err)
	}
	// Another environment's lock counts as much as this one's.
	lockOfURLs(t, filepath.Join(home, "envs", "other", "apko-default.lock.json"), "https://packages.example.invalid/os/aarch64/kept-1-r0.apk")
	lockOfURLs(t, filepath.Join(home, "envs", "default", "apko-min.lock.json"), "https://packages.example.invalid/os/aarch64/mine-2-r0.apk")
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(arch, name))
		return err == nil
	}

	bad := filepath.Join(home, "envs", "broken", "apko-default.lock.json")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Prune(nil); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	_, e := b.said()
	if !strings.Contains(e, "caboose: not pruning the package cache ~/caboose-home/cache/apk: reading lock "+bad) ||
		!strings.Contains(e, "'caboose -e broken build --pull'") || !exists("gone-1-r3") {
		t.Errorf("an unreadable lock:\n%s", e)
	}

	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := b.Prune(nil); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	_, e = b.said()
	if !strings.Contains(e, "caboose: package cache ~/caboose-home/cache/apk: freed 5 B (1 package no lock names, 0 old indexes)\n") {
		t.Errorf("stderr:\n%s", e)
	}
	if exists("gone-1-r3") || !exists("kept-1-r0") || !exists("mine-2-r0") {
		t.Errorf("gone %v, kept %v, mine %v", exists("gone-1-r3"), exists("kept-1-r0"), exists("mine-2-r0"))
	}

	if err := b.Prune(nil); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	if _, e = b.said(); !strings.Contains(e, "caboose: package cache ~/caboose-home/cache/apk: nothing to free\n") {
		t.Errorf("again:\n%s", e)
	}
}

// While a build uses the package cache, caboose prune leaves it, says so
// in a line, and prunes the versions all the same.
func TestPruneApkCacheInUse(t *testing.T) {
	b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
	b.mountLocal("local/linux-arm64")
	mkdirs(t, b.data, "local/linux-arm64/share/claude", "local/linux-arm64/bin")
	home := filepath.Join(b.tmp, "caboose-home")
	b.Cfg.CabooseHome, b.Cfg.Home = home, b.tmp
	gone := filepath.Join(home, "cache", "apk", "https%3A%2F%2Fpackages.example.invalid%2Fos", "aarch64", "gone-1-r3")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * apkCacheRecent)
	if err := os.Chtimes(gone, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := apkobuild.LockCache(filepath.Join(home, "cache", "apk"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := b.Prune(nil); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	if _, e := b.said(); !strings.Contains(e, "caboose: package cache ~/caboose-home/cache/apk: a build is using the package cache; not pruned\n") {
		t.Errorf("stderr:\n%s", e)
	}
	if _, err := os.Lstat(gone); err != nil {
		t.Errorf("pruned while in use: %v", err)
	}
	if !slices.ContainsFunc(b.box.Execs, func(s backend.ExecSpec) bool { return slices.Equal(s.Argv, []string{Entrypoint, "--cc-prune"}) }) {
		t.Errorf("the versions were not pruned: %+v", b.box.Execs)
	}
}
