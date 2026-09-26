package datadir

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestEnsurePlatformLayout(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	dir := t.TempDir()
	if err := EnsurePlatformLayout(dir, "linux-x64-musl"); err != nil {
		t.Fatal(err)
	}
	want := []string{"local/linux-x64-musl/bin", "local/linux-x64-musl/share/claude", "local/linux-x64-musl/cache/claude"}
	if got := PlatformMounts("linux-x64-musl"); !reflect.DeepEqual(got, want) {
		t.Errorf("PlatformMounts = %q", got)
	}
	for _, d := range want {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}
	// Created with the umask, like the rest of the layout.
	if m := perm(t, filepath.Join(dir, "local/linux-x64-musl")); m != 0o775 {
		t.Errorf("mode %v, want 0775", m)
	}
	if err := EnsurePlatformLayout(dir, "../x"); err == nil {
		t.Error("a non-platform was accepted")
	}
	// EnsureLayout, which runs on every launch, creates no platform dir:
	// those are made for the image a container is created from.
	fresh := t.TempDir()
	if err := EnsureLayout(fresh, defKeep()); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"local"} {
		if _, err := os.Lstat(filepath.Join(fresh, d)); err == nil {
			t.Errorf("EnsureLayout created %s", d)
		}
	}
}

func TestPlatformDirsIn(t *testing.T) {
	dir := t.TempDir()
	if got, err := PlatformDirsIn(dir); err != nil || got != nil {
		t.Errorf("no local/: %q, %v", got, err)
	}
	for _, d := range []string{"linux-x64-musl", "linux-arm64", "bin", "share", ".tmp-linux-x64", "linux-sparc"} {
		if err := os.MkdirAll(filepath.Join(dir, "local", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "local/linux-x64"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := PlatformDirsIn(dir); err != nil || !reflect.DeepEqual(got, []string{"linux-arm64", "linux-x64-musl"}) {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestMountedLocalDir(t *testing.T) {
	if d, p, ok := MountedLocalDir("/d/local/linux-arm64/bin"); !ok || d != "/d/local/linux-arm64" || p != "linux-arm64" {
		t.Errorf("platform dir: %q, %q, %v", d, p, ok)
	}
	for _, src := range []string{"/d/local/bin", "/elsewhere/bin"} {
		if d, p, ok := MountedLocalDir(src); ok || d != "" || p != "" {
			t.Errorf("MountedLocalDir(%q) = %q, %q, %v; want no platform dir", src, d, p, ok)
		}
	}
}

// HasInstall sees bin/claude as the entrypoint will from inside: the
// installer's symlink names a container path, looked up under the dir.
func TestHasInstall(t *testing.T) {
	mk := func(t *testing.T, files []string, link string) string {
		t.Helper()
		local := t.TempDir()
		for _, f := range files {
			p := filepath.Join(local, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if link != "" {
			if err := os.MkdirAll(filepath.Join(local, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(link, filepath.Join(local, "bin/claude")); err != nil {
				t.Fatal(err)
			}
		}
		return local
	}
	for _, tc := range []struct {
		name  string
		files []string
		link  string
		want  bool
	}{
		{"empty", nil, "", false},
		{"only the layout", []string{"share/claude/.keep", "cache/claude/.keep"}, "", false},
		{"the installer's symlink", []string{"share/claude/versions/2.1.9"}, "/home/agent/.local/share/claude/versions/2.1.9", true},
		{"a dangling symlink", []string{"share/claude/versions/2.1.8"}, "/home/agent/.local/share/claude/versions/2.1.9", false},
		{"a relative symlink", []string{"share/claude/versions/2.1.9"}, "../share/claude/versions/2.1.9", true},
		{"a plain file", []string{"bin/claude"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasInstall(mk(t, tc.files, tc.link)); got != tc.want {
				t.Errorf("HasInstall = %v, want %v", got, tc.want)
			}
		})
	}
}
