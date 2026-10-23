package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/saferm"
)

// TestMain points the socket Main dials at a path in a fresh directory where
// nothing listens, so that no test of this package can reach a real link, even
// one that calls Main with host, open, notify or ports. The directory is made
// here and removed by that exact path.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-nolink-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent tests:", err)
		os.Exit(1)
	}
	linkSocket = filepath.Join(dir, "none.sock")
	code := m.Run()
	saferm.RemoveRoot(dir)
	os.Exit(code)
}

// TestLinkSocketIsNotTheRealOne holds the override: with it gone, a test that
// reaches Main would dial the real link.
func TestLinkSocketIsNotTheRealOne(t *testing.T) {
	if filepath.Base(linkSocket) != "none.sock" || !strings.HasPrefix(linkSocket, os.TempDir()) {
		t.Fatalf("tests must not dial a real link socket, got %q", linkSocket)
	}
	if _, err := os.Lstat(linkSocket); !os.IsNotExist(err) {
		t.Fatalf("the test socket path must not exist, got %v", err)
	}
}
