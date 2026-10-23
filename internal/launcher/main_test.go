package launcher

import (
	"fmt"
	"net/netip"
	"os"
	"testing"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/nodocker"
	"github.com/bfreis/caboose/internal/syncagent"
)

// The tests' fake docker runs caboose-agent's sync as this test binary,
// on the home and sync repo these name (autoEnv).
const (
	testSyncHome = "CABOOSE_TEST_SYNC_HOME"
	testSyncRepo = "CABOOSE_TEST_SYNC_REPO"
)

func TestMain(m *testing.M) {
	// A daemon of the tests never serves its page on the real ports,
	// where the user's own daemon serves theirs.
	daemonWeb = func(bool) *daemon.WebOptions {
		return &daemon.WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:0")}
	}
	// No test runs this machine's ssh or reads its ~/.ssh: one that looks
	// for an agent sets App.SSH.
	hostSSH = noSSH
	if len(os.Args) > 1 && os.Args[1] == "askpass" && os.Getenv(syncagent.AskpassEnv) != "" {
		os.Exit(syncagent.Askpass(os.Args[2:], os.Stdout, os.Stderr))
	}
	if home := os.Getenv(testSyncHome); home != "" && len(os.Args) == 3 && os.Args[1] == "sync" {
		exe, _ := os.Executable()
		os.Exit(syncagent.Serve(os.Args[2], os.Stdin, os.Stdout, os.Stderr,
			syncagent.Paths{Home: home, Repo: os.Getenv(testSyncRepo), Exe: exe}))
	}
	if os.Getenv(confPrintEnv) == "1" {
		// A child of the conformance tests: what it inherited.
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		os.Exit(0)
	}
	if os.Getenv(brokerDaemonEnv) != "" {
		os.Exit(brokerDaemonMain())
	}
	if dir := os.Getenv(heldLockEnv); dir != "" {
		holdLinkLock(dir)
	}
	os.Exit(nodocker.Run(m))
}
