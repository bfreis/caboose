package launcher

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
)

// withCkFS gives the tests a machine of their own: every path is on
// device 1 of type fstype, on goos.
func withCkFS(t *testing.T, goos, fstype string) {
	t.Helper()
	old := hostCkFS
	t.Cleanup(func() { hostCkFS = old })
	hostCkFS = ckFS{goos: goos, info: func(string) (uint64, string, error) { return 1, fstype, nil }, statfs: old.statfs}
}

// setupWithRoots is a setup environment whose roots are ~/dev, with a/b and
// c writable, and ~/all, wholly writable.
func setupWithRoots(t *testing.T) *setupEnv {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/a/b")
	e.mkdir("dev/c")
	e.mkdir("all")
	e.writeConfig("[roots]\ndev = { host = \"~/dev\", writable = [\"a/b\", \"c\"] }\nall = { host = \"~/all\", writable = [\".\"] }\n")
	return e
}

func TestSetupCheckpointsCandidates(t *testing.T) {
	withCkFS(t, "darwin", "apfs")
	e := setupWithRoots(t)
	got := e.a.checkpointCandidates()
	var paths []string
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	if want := []string{"/work/all", "/work/dev/a/b", "/work/dev/c", "~/.claude"}; !slices.Equal(paths, want) {
		t.Errorf("candidates = %q, want %q", paths, want)
	}
	if got[1].Host != filepath.Join(e.a.Cfg.Home, "dev", "a", "b") {
		t.Errorf("host = %s", got[1].Host)
	}
	if want := filepath.Join(e.a.Cfg.DataDir, "home", ".claude"); got[3].Host != want {
		t.Errorf("host = %s, want %s", got[3].Host, want)
	}
}

// Numbers and paths, mixed: written, read back, and the store, the
// schedule and the method said first.
func TestSetupCheckpointsWrites(t *testing.T) {
	withCkFS(t, "darwin", "apfs")
	e := setupWithRoots(t)
	if err := e.run("2 /work/dev/c ~/.claude\n", "checkpoints"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Take checkpoints of which? Numbers or paths separated by spaces; empty for none",
		"every 10 minutes while the sandbox runs", "hourly for 48 hours, daily for 7 days",
		"envs/default/checkpoints", "1 /work/all", "(clonefile)", "Wrote 3 paths")
	if got, want := e.file().Checkpoints, []string{"/work/dev/a/b", "/work/dev/c", "~/.claude"}; !slices.Equal(got, want) {
		t.Errorf("checkpoints = %q, want %q", got, want)
	}
	data, _ := os.ReadFile(e.configPath())
	if !strings.Contains(string(data), "[roots]") {
		t.Errorf("roots lost:\n%s", data)
	}
	// Again: the current paths are the default, and Enter keeps them.
	if err := e.run("\n", "checkpoints"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("; - for none: [/work/dev/a/b /work/dev/c ~/.claude]", "Nothing changed")
	// "-" clears them: the line is commented out, and they read back as none.
	if err := e.run("-\n", "checkpoints"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if got := e.file().Checkpoints; len(got) != 0 {
		t.Errorf("checkpoints = %q", got)
	}
	if err := e.run("\n", "checkpoints"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("Nothing changed")
}

// A path off the store's APFS volume, or any path where the filesystem
// has no clone, is said to copy.
func TestSetupCheckpointsSaysTheMethod(t *testing.T) {
	for _, tc := range []struct{ goos, fstype, want string }{
		{"darwin", "hfs", "(copy)"},
		{"linux", "ext4", "reflink if the filesystem supports it, else copy"},
		{"linux", "btrfs", "(reflink)"},
	} {
		withCkFS(t, tc.goos, tc.fstype)
		e := setupWithRoots(t)
		if err := e.run("\n", "checkpoints"); err != nil {
			t.Fatalf("%v\n%s", err, e.errb)
		}
		e.wantOut(tc.want)
	}
}

// An answer that is not a list config.toml takes is asked again, and
// nothing is written for it.
func TestSetupCheckpointsRefusesBadAnswers(t *testing.T) {
	withCkFS(t, "darwin", "apfs")
	e := setupWithRoots(t)
	before, _ := os.ReadFile(e.configPath())
	if err := e.run("9\nrelative/x\n/work/dev/c /work/dev/c\n/work/dev/a /work/dev/a/b\n~\n\n", "checkpoints"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("There is no candidate 9: 1 to 4", "✗ ", "Nothing changed")
	if n := strings.Count(e.errb.String(), "✗"); n != 5 {
		t.Errorf("%d refusals in:\n%s", n, e.errb)
	}
	if after, _ := os.ReadFile(e.configPath()); string(after) != string(before) {
		t.Errorf("config.toml changed:\n%s", after)
	}
	// More than the limit is refused too.
	var many []string
	for i := 0; i <= config.MaxCheckpointPaths; i++ {
		many = append(many, "/work/x"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if err := e.run(strings.Join(many, " ")+"\n\n", "checkpoints"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("at most 32")
	if after, _ := os.ReadFile(e.configPath()); string(after) != string(before) {
		t.Errorf("config.toml changed:\n%s", after)
	}
}

// Setup writes the paths into a config.toml made from the template, and
// the store is never made by it.
func TestSetupCheckpointsFromTemplateMakesNoStore(t *testing.T) {
	withCkFS(t, "linux", "ext4")
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("~/.claude\n", "checkpoints"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	data, _ := os.ReadFile(e.configPath())
	if !strings.Contains(string(data), "[checkpoints]\npaths = [\"~/.claude\"]") || !strings.Contains(string(data), "# Paths of the sandbox") {
		t.Errorf("config.toml:\n%s", data)
	}
	if _, err := os.Lstat(checkpoint.StoreDir(e.a.Cfg.EnvDir)); err == nil {
		t.Error("setup made the store")
	}
}

func TestCkFSMethod(t *testing.T) {
	info := func(m map[string]uint64, types map[string]string) func(string) (uint64, string, error) {
		return func(p string) (uint64, string, error) {
			for k, d := range m {
				if strings.HasPrefix(p, k) {
					return d, types[k], nil
				}
			}
			return 0, "", os.ErrNotExist
		}
	}
	dir := t.TempDir()
	src, store := filepath.Join(dir, "src"), filepath.Join(dir, "store")
	for _, tc := range []struct {
		name   string
		f      ckFS
		method string
		slow   bool
	}{
		{"mac, one apfs volume", ckFS{goos: "darwin", info: info(map[string]uint64{src: 1, store: 1}, map[string]string{src: "apfs", store: "apfs"})}, "clonefile", false},
		{"mac, another volume", ckFS{goos: "darwin", info: info(map[string]uint64{src: 2, store: 1}, map[string]string{src: "apfs", store: "apfs"})}, "copy", true},
		{"mac, not apfs", ckFS{goos: "darwin", info: info(map[string]uint64{src: 1, store: 1}, map[string]string{src: "hfs", store: "hfs"})}, "copy", true},
		{"linux, xfs", ckFS{goos: "linux", info: info(map[string]uint64{src: 1, store: 1}, map[string]string{src: "xfs", store: "xfs"})}, "reflink", false},
		{"linux, two xfs volumes", ckFS{goos: "linux", info: info(map[string]uint64{src: 1, store: 2}, map[string]string{src: "xfs", store: "xfs"})}, "copy", true},
		{"linux, ext4", ckFS{goos: "linux", info: info(map[string]uint64{src: 1, store: 1}, map[string]string{src: "ext4", store: "ext4"})}, "copy", true},
		{"unreadable", ckFS{goos: "darwin", info: func(string) (uint64, string, error) { return 0, "", os.ErrPermission }}, "copy", true},
	} {
		f := tc.f
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(store, 0o755); err != nil {
			t.Fatal(err)
		}
		m := f.method(src, store)
		if m.Name != tc.method || m.Slow != tc.slow || m.Why == "" {
			t.Errorf("%s: %+v", tc.name, m)
		}
	}
}
