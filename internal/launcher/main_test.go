package launcher

import (
	"os"
	"testing"

	"github.com/bfreis/caboose/internal/syncagent"
)

// The tests' fake docker runs caboose-agent's sync as this test binary,
// on the home and sync repo these name (autoEnv).
const (
	testSyncHome = "CABOOSE_TEST_SYNC_HOME"
	testSyncRepo = "CABOOSE_TEST_SYNC_REPO"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "askpass" && os.Getenv(syncagent.AskpassEnv) != "" {
		os.Exit(syncagent.Askpass(os.Args[2:], os.Stdout, os.Stderr))
	}
	if home := os.Getenv(testSyncHome); home != "" && len(os.Args) == 3 && os.Args[1] == "sync" {
		exe, _ := os.Executable()
		os.Exit(syncagent.Serve(os.Args[2], os.Stdin, os.Stdout, os.Stderr,
			syncagent.Paths{Home: home, Repo: os.Getenv(testSyncRepo), Exe: exe}))
	}
	os.Exit(m.Run())
}
