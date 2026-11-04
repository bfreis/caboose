package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDevLinks(t *testing.T) {
	dir := t.TempDir()
	// A stale entry is replaced, as /dev/ptmx's is.
	if err := os.WriteFile(filepath.Join(dir, "stdin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := devLinks(dir); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
	}
	for name, target := range want {
		got, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || got != target {
			t.Errorf("%s -> %q, %v; want %q", name, got, err, target)
		}
	}
}
