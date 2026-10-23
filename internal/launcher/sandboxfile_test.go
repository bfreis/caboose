package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// caboose sync add and rm read the sandbox config through its limit: one
// the sandbox made larger is refused before it is read.
func TestReadSandboxFileTooLarge(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	p := filepath.Join(e.a.Cfg.DataDir, datadir.SandboxConfig)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, sandboxcfg.MaxSize+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.a.readSandboxFile(); !errors.Is(err, nofollow.ErrTooLarge) {
		t.Errorf("readSandboxFile = %v", err)
	}
}
