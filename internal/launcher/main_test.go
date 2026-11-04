package launcher

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bfreis/caboose/internal/config"

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
	// A daemon started from this binary would run the whole suite again:
	// every start goes through startDetached, which refuses unless a test
	// stubs it.
	startDetached = refuseDetached
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		fmt.Fprintln(os.Stderr, "launcher test binary started as `daemon`: a test spawned the test binary as a caboose daemon; stub startDetached or set App.Spawn")
		os.Exit(3)
	}
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
	code := nodocker.Run(m)
	if n := refusedDetached.Load(); n > 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d test(s) tried to start a detached process (a daemon) from the test binary; stub startDetached or set App.Spawn\n", n)
		code = 1
	}
	os.Exit(code)
}

var refusedDetached atomic.Int32

// refuseDetached is startDetached in every test that does not stub it.
func refuseDetached(exe string, args, env []string) (*exec.Cmd, error) {
	refusedDetached.Add(1)
	return nil, fmt.Errorf("test binary refuses to start %s %v detached", exe, args)
}

// A test binary run as `daemon` would run the suite: TestMain exits instead.
func TestMainRefusesDaemonArgument(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(exe, "daemon").CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || !strings.Contains(string(out), "started as `daemon`") {
		t.Fatalf("binary run as daemon: %v\n%s", err, out)
	}
}

// Without a stub, startDetached is the refusal, and App.startDaemon reports it.
func TestStartDaemonIsRefusedInTests(t *testing.T) {
	if reflect.ValueOf(startDetached).Pointer() != reflect.ValueOf(refuseDetached).Pointer() {
		t.Skip("another test's stub is in place")
	}
	a := &App{Cfg: &config.Config{CabooseHome: t.TempDir(), Getenv: func(string) string { return "" }}}
	before := refusedDetached.Load()
	if err := a.startDaemon("/bin/true"); err == nil {
		t.Fatal("startDaemon started a process")
	}
	refusedDetached.Store(before) // this refusal is the one under test
}
