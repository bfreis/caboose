package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bfreis/caboose/internal/saferm"
)

// A removal of the layout's goes only inside Versions: a path that
// escapes it, by .. or through a symlinked directory, is refused and
// nothing is removed.
func TestRemoveStaysInVersions(t *testing.T) {
	base := t.TempDir()
	l := Layout{Versions: filepath.Join(base, "versions")}
	outside := filepath.Join(base, "outside")
	for _, d := range []string{l.Versions, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.Versions, "v1.0.0")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(l.Versions, "..", "outside"),
		filepath.Join(l.Versions, "v1.0.0", "f"),
		l.Versions,
	} {
		if err := l.remove(p); !errors.Is(err, saferm.ErrOutside) {
			t.Errorf("remove(%s): %v, want refused", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "f")); err != nil {
		t.Errorf("removed outside versions/: %v", err)
	}
}
