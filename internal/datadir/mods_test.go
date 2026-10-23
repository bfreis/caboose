package datadir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modManifest = "mods/caboose/.claude-plugin/plugin.json"

// A mod goes into the managed dir under mods/, its dot directory too, and
// what the set no longer has goes from there, files and directories alike:
// a stale mod file left behind would still be loaded.
func TestInstallManagedMods(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"CLAUDE.md":                     []byte("c\n"),
		modManifest:                     []byte("{}\n"),
		"mods/caboose/hooks/hooks.json": []byte("{}\n"),
		"mods/caboose/hooks/old.ts":     []byte("old\n"),
		"mods/other/x/y.ts":             []byte("other\n"),
	}
	if changed, err := InstallManaged(files, dir); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for name, want := range files {
		if got := read(t, filepath.Join(dir, ManagedDir, filepath.FromSlash(name))); got != string(want) {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	delete(files, "mods/caboose/hooks/old.ts")
	delete(files, "mods/other/x/y.ts")
	if changed, err := InstallManaged(files, dir); err != nil || !changed {
		t.Fatalf("prune: changed=%v err=%v", changed, err)
	}
	for _, gone := range []string{"mods/caboose/hooks/old.ts", "mods/other"} {
		if _, err := os.Lstat(filepath.Join(dir, ManagedDir, filepath.FromSlash(gone))); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	if read(t, filepath.Join(dir, ManagedDir, modManifest)) != "{}\n" {
		t.Error("a kept mod file was lost")
	}
	// Nothing outside skills and mods is pruned.
	other := filepath.Join(dir, ManagedDir, "other.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := InstallManaged(files, dir); err != nil || changed || read(t, other) != "x" {
		t.Errorf("changed=%v err=%v", changed, err)
	}
	// A set with no mods at all takes them all away.
	delete(files, modManifest)
	delete(files, "mods/caboose/hooks/hooks.json")
	if _, err := InstallManaged(files, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ManagedMods, "caboose")); err == nil {
		t.Error("a mod the set no longer has is still installed")
	}
}

// Links planted in the mods tree are not followed, written through or
// removed through.
func TestInstallManagedModsNeverFollowLinks(t *testing.T) {
	host := hostFile(t, "host file\n")
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{modManifest: []byte("{}\n")}

	dir := t.TempDir()
	plugin := filepath.Join(dir, ManagedDir, "mods", "caboose", ".claude-plugin")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(plugin, "plugin.json")); err != nil {
		t.Fatal(err)
	}
	// A stale link to a directory with a file in it, in the tree.
	if err := os.Symlink(hostDir, filepath.Join(dir, ManagedDir, "mods", "stale")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManaged(files, dir); err != nil {
		t.Fatal(err)
	}
	if read(t, host) != "host file\n" || read(t, filepath.Join(plugin, "plugin.json")) != "{}\n" {
		t.Error("the link was written through, or not replaced")
	}
	if _, err := os.Stat(filepath.Join(hostDir, "keep.txt")); err != nil {
		t.Errorf("removed through a link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ManagedDir, "mods", "stale")); err == nil {
		t.Error("the stale link is still there")
	}
}

// A mod is code: a placeholder-shaped string in it is its own, and no file
// under mods/ is filled in, whatever its extension.
func TestExpandTreeLeavesModsAlone(t *testing.T) {
	in := map[string][]byte{
		"CLAUDE.md":              []byte("@@CABOOSE_ENV@@\n"),
		"mods/caboose/README.md": []byte("@@CABOOSE_ENV@@ @@NOT_A_THING@@\n"),
		"mods/caboose/x.ts":      []byte("@@CABOOSE_ENV@@\n"),
	}
	out, err := ExpandTree(in, Facts{Env: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out["CLAUDE.md"]), "@@") {
		t.Errorf("CLAUDE.md not expanded: %q", out["CLAUDE.md"])
	}
	for _, k := range []string{"mods/caboose/README.md", "mods/caboose/x.ts"} {
		if string(out[k]) != string(in[k]) {
			t.Errorf("%s changed: %q", k, out[k])
		}
	}
}
