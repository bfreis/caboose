package launcher

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// runWatchdog runs the watchdog script with the host's bash, as the
// container's would: budget seconds for command.
func runWatchdog(t *testing.T, budget string, command ...string) (code int, out string, took time.Duration) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	var buf bytes.Buffer
	c := exec.Command("bash", append([]string{"-c", watchdog, "watchdog", budget}, command...)...)
	// Through a pipe, as docker exec reads it: whatever holds the pipe
	// open holds the call up.
	c.Stdout, c.Stderr = &buf, &buf
	start := time.Now()
	err := c.Run()
	took = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, buf.String(), took
}

func TestWatchdogStopsWhatHangs(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-alive")
	// A command whose child (ssh, for git) would outlive it, and write
	// the marker if it were not stopped with it.
	code, out, took := runWatchdog(t, "1", "sh", "-c", "(sleep 2; touch '"+marker+"') & sleep 30")
	if code != 124 || !strings.Contains(out, "no answer from the remote in 1s") {
		t.Errorf("exit %d, %q", code, out)
	}
	if took > 3*time.Second {
		t.Errorf("took %v", took)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command's child outlived the timeout")
	}
}

func TestWatchdogPassesThrough(t *testing.T) {
	code, out, took := runWatchdog(t, "10", "sh", "-c", "echo done; exit 3")
	if code != 3 || out != "done\n" {
		t.Errorf("exit %d, %q", code, out)
	}
	// The timer's sleep must not keep the pipe open.
	if took > 2*time.Second {
		t.Errorf("took %v: the timer held the output open", took)
	}
}

// syncGitApp is an App whose docker answers the sandbox's ssh command
// question with own.
func syncGitApp(t *testing.T, own string) *App {
	t.Helper()
	bin := t.TempDir()
	fake := filepath.Join(bin, "docker")
	script := "#!/bin/sh\ncase \"$*\" in *core.sshCommand*) printf '%s' '" + own + "'; exit 0 ;; esac\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &App{Cfg: &config.Config{Container: "box"}, Docker: &docker.CLI{Path: fake}}
}

func TestSyncGitCommand(t *testing.T) {
	args := func(g *syncGit, remote bool) []string {
		return g.command(remote, true, "-C", "/r", "fetch").Args[1:]
	}
	has := func(argv []string, seq ...string) bool {
		for i := range argv {
			if i+len(seq) <= len(argv) && slices.Equal(argv[i:i+len(seq)], seq) {
				return true
			}
		}
		return false
	}
	ssh := "GIT_SSH_COMMAND=ssh -o 'UserKnownHostsFile=" + knownHosts + " ~/.ssh/known_hosts'"

	auto := syncGitApp(t, "").newSyncGit(true)
	remote := args(auto, true)
	for _, want := range [][]string{
		{"-e", "GIT_TERMINAL_PROMPT=0"},
		{"-e", ssh + " -o BatchMode=yes"},
		{"box", "bash", "-c", watchdog, "watchdog", "20", "git", "-C", "/r", "fetch"},
	} {
		if !has(remote, want...) {
			t.Errorf("auto, remote: %q lacks %q", remote, want)
		}
	}
	if slices.Contains(remote, "-t") {
		t.Errorf("auto asks for a terminal: %q", remote)
	}
	if local := args(auto, false); !has(local, "box", "git", "-C") || slices.Contains(local, watchdog) {
		t.Errorf("auto, local: %q", local)
	}

	manual := syncGitApp(t, "").newSyncGit(false)
	m := args(manual, true)
	if !has(m, "-e", ssh) || has(m, "-e", "GIT_TERMINAL_PROMPT=0") || slices.Contains(m, watchdog) {
		t.Errorf("caboose sync: %q", m)
	}

	own := syncGitApp(t, "ssh -i /k").newSyncGit(true)
	if o := args(own, true); slices.ContainsFunc(o, func(s string) bool { return strings.HasPrefix(s, "GIT_SSH_COMMAND=") }) {
		t.Errorf("overrode the sandbox's own ssh command: %q", o)
	}
}

func TestSyncGitBudgetIsShared(t *testing.T) {
	g := syncGitApp(t, "").newSyncGit(true)
	g.deadline = time.Now().Add(7500 * time.Millisecond)
	if r := g.remaining(); r != 8 {
		t.Errorf("remaining = %d, want 8", r)
	}
	g.deadline = time.Now().Add(-time.Minute)
	if r := g.remaining(); r != 1 {
		t.Errorf("past the deadline: remaining = %d, want 1", r)
	}
}

// The process listing, run by the host's bash on the host's /proc as the
// container's would be: a renamed argv[0] shows as it is, and a command
// name with ") " in it does not throw the ppid off.
func TestProcessesScript(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	// A script: the kernel names the process after it, "a) b", and its
	// argv[0] is the interpreter's.
	odd := filepath.Join(t.TempDir(), "a) b")
	if err := os.WriteFile(odd, []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var started []*exec.Cmd
	for _, c := range []*exec.Cmd{
		exec.Command("bash", "-c", "exec -a /home/agent/.local/bin/claude sleep 10"),
		exec.Command(odd, "10"),
	} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		started = append(started, c)
	}
	defer func() {
		for _, c := range started {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	}()
	time.Sleep(200 * time.Millisecond) // bash to exec
	if stat, _ := os.ReadFile("/proc/" + strconv.Itoa(started[1].Process.Pid) + "/stat"); !strings.Contains(string(stat), "(a) b)") {
		t.Fatalf("the script's process is not named after it: %q", stat)
	}
	out, err := exec.Command("bash", "-c", processesScript).Output()
	if err != nil {
		t.Fatal(err)
	}
	self := strconv.Itoa(os.Getpid())
	for i, want := range []string{"/home/agent/.local/bin/claude", "/bin/sh"} {
		line := strconv.Itoa(started[i].Process.Pid) + " " + self + " " + want + "\n"
		if !strings.Contains(string(out), line) {
			t.Errorf("no %q in:\n%s", line, out)
		}
	}
}
