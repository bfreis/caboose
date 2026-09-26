package launcher

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/statesync"
)

// autoEnv is a machine for sync on launch: a data dir, and a fake docker
// for a running container "box" that mounts its sync repo and runs its git
// with the host's git -- through the watchdog, with the host's bash, when
// asked to. Files in ctl steer it:
//
//	procs  processes listed after PID 1, as "PID PPID ARGV0" lines
//	slow   makes every fetch hang
//	hostkey  makes every fetch fail as ssh does on an unknown host key
//	nomount  drops the sync mount
//	log    every docker call, one a line
type autoEnv struct {
	t      *testing.T
	a      *App
	data   string
	ctl    string
	remote string
	stderr *bytes.Buffer
	stdout *bytes.Buffer
}

func newAutoEnv(t *testing.T, remote string) *autoEnv {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	e := &autoEnv{t: t, data: t.TempDir(), ctl: t.TempDir(), remote: remote,
		stderr: &bytes.Buffer{}, stdout: &bytes.Buffer{}}
	repo := filepath.Join(e.data, statesync.Dir)
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	// git as the container runs it, hanging on a fetch when told to.
	wrap := filepath.Join(e.ctl, "git")
	e.write(wrap, `#!/bin/sh
for a; do [ "$a" = fetch ] && [ -e '`+e.ctl+`/slow' ] && exec sleep 30; done
for a; do [ "$a" = fetch ] && [ -e '`+e.ctl+`/hostkey' ] && {
  printf 'Host key verification failed.\r\nfatal: Could not read from remote repository.\n' >&2; exit 128; }; done
exec '`+git+`' "$@"
`)
	if err := os.Chmod(wrap, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(e.ctl, "docker")
	e.write(fake, `#!/bin/sh
echo "$*" >> '`+e.ctl+`/log'
case "$*" in
  "inspect --type=container -f {{.State.Status}} box") echo running; exit 0 ;;
  "exec box bash -c for d in /proc/"*) echo "1 0 sleep"
             [ -e '`+e.ctl+`/procs' ] && cat '`+e.ctl+`/procs'; exit 0 ;;
  "inspect --type=container box --format "*)
             [ -e '`+e.ctl+`/nomount' ] || printf '`+statesync.ContainerDir+`\t%s\n' '`+repo+`'; exit 0 ;;
esac
[ "$1" = exec ] || exit 1
shift
while :; do
  case "$1" in
    -i|-t) shift ;;
    -e) export "$2"; shift 2 ;;
    *) break ;;
  esac
done
[ "$1" = box ] || exit 1
shift
case "$1" in git|bash) ;; *) exit 1 ;; esac
[ "$1" = bash ] && [ "$4" != watchdog ] && exit 1   # only the watchdog runs in bash
n=$#
for a; do
  case "$a" in
    `+statesync.ContainerDir+`) a='`+repo+`' ;;
    git) a='`+wrap+`' ;;
  esac
  set -- "$@" "$a"
done
shift $n
exec "$@"
`)
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	e.a = &App{
		Cfg:    &config.Config{Container: "box", DataDir: e.data, AutoSync: "1"},
		Docker: &docker.CLI{Path: fake},
		Stdout: e.stdout, Stderr: e.stderr,
	}
	return e
}

func (e *autoEnv) write(path, data string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *autoEnv) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(e.data, rel))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// syncer is this machine's sync, run by hand (as `caboose sync` would).
func (e *autoEnv) syncer() *statesync.Syncer {
	s := e.a.newSyncer(e.a.newSyncGit(false))
	return s
}

// setRemote sets this machine's remote, as `caboose sync --remote` does.
func (e *autoEnv) setRemote() {
	e.t.Helper()
	s := e.syncer()
	if err := s.Init(); err != nil {
		e.t.Fatal(err)
	}
	if err := s.SetRemote(e.remote); err != nil {
		e.t.Fatal(err)
	}
}

// launch runs what a launch on a terminal runs before attaching, and
// returns what it said.
func (e *autoEnv) launch() string {
	e.t.Helper()
	e.stderr.Reset()
	_ = os.Remove(filepath.Join(e.ctl, "log"))
	e.a.beforeAttach(true)
	if e.stdout.Len() > 0 {
		e.t.Errorf("stdout: %q", e.stdout)
	}
	return e.stderr.String()
}

// ranGit reports whether the fake docker has run git since last asked.
func (e *autoEnv) ranGit() bool {
	log, _ := os.ReadFile(filepath.Join(e.ctl, "log"))
	_ = os.Remove(filepath.Join(e.ctl, "log"))
	return bytes.Contains(log, []byte(" git "))
}

func newBare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", statesync.Branch, dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	return dir
}

// memRel is a memory file's data dir path, for a project under /work.
func memRel(project, file string) string {
	return "home/.claude/projects/" + statesync.ProjectKey("/work/"+project) + "/memory/" + file
}

// pair is two machines on one remote, the other one's first sync done.
func pair(t *testing.T) (here, there *autoEnv) {
	t.Helper()
	remote := newBare(t)
	here, there = newAutoEnv(t, remote), newAutoEnv(t, remote)
	here.setRemote()
	there.setRemote()
	there.write(filepath.Join(there.data, memRel("p", "base.md")), "base\n")
	if _, err := there.syncer().Sync(); err != nil {
		t.Fatal(err)
	}
	return here, there
}

func TestAutoSyncTakesAndSends(t *testing.T) {
	here, there := pair(t)
	// The memory, and the sandbox config the other machine's first sync
	// wrote from the defaults.
	if got := here.launch(); got != "caboose: synced: took 2 changes from the remote\n" {
		t.Errorf("first launch said %q", got)
	}
	if here.read(memRel("p", "base.md")) != "base\n" {
		t.Errorf("not applied: %q", here.read(memRel("p", "base.md")))
	}
	if got := here.launch(); got != "" {
		t.Errorf("in sync, a launch said %q", got)
	}
	here.write(filepath.Join(here.data, memRel("p", "here.md")), "from here\n")
	if got := here.launch(); got != "caboose: synced: sent this machine's\n" {
		t.Errorf("with a change here, a launch said %q", got)
	}
	if _, err := there.syncer().Sync(); err != nil {
		t.Fatal(err)
	}
	if there.read(memRel("p", "here.md")) != "from here\n" {
		t.Error("the change did not reach the other machine")
	}
}

// Off unless asked for; and never without a terminal, or with no remote.
func TestAutoSyncOnlyWhenItShould(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		here, _ := pair(t)
		here.a.Cfg.AutoSync = ""
		if got := here.launch(); got != "" || here.ranGit() {
			t.Errorf("said %q, or ran git", got)
		}
	})
	t.Run("no terminal", func(t *testing.T) {
		here, _ := pair(t)
		here.ranGit()
		here.a.beforeAttach(false)
		if here.stderr.Len() > 0 || here.ranGit() {
			t.Errorf("said %q", here.stderr)
		}
	})
	t.Run("no remote", func(t *testing.T) {
		e := newAutoEnv(t, newBare(t))
		if got := e.launch(); got != "" || e.ranGit() {
			t.Errorf("said %q", got)
		}
	})
	t.Run("something runs", func(t *testing.T) {
		here, _ := pair(t)
		here.write(filepath.Join(here.ctl, "procs"), "20 0 /home/agent/.local/bin/claude\n")
		if got := here.launch(); got != "" || here.ranGit() {
			t.Errorf("said %q", got)
		}
		if here.read(memRel("p", "base.md")) == "base\n" {
			t.Error("synced under a live run")
		}
	})
}

// Whatever goes wrong is one line, ending in what to run, and the launch
// goes on.
func TestAutoSyncFailuresAreNotes(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		here, there := pair(t)
		here.launch()
		there.write(filepath.Join(there.data, memRel("p", "base.md")), "there\n")
		if _, err := there.syncer().Sync(); err != nil {
			t.Fatal(err)
		}
		here.write(filepath.Join(here.data, memRel("p", "base.md")), "here\n")
		got := here.launch()
		if !strings.HasPrefix(got, "caboose: not synced: ") || !strings.HasSuffix(got, "changed on both sides; run 'caboose sync' to settle it\n") ||
			strings.Count(got, "\n") != 1 {
			t.Errorf("said %q", got)
		}
		if here.read(memRel("p", "base.md")) != "here\n" {
			t.Errorf("the conflict changed the data dir: %q", here.read(memRel("p", "base.md")))
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		e := newAutoEnv(t, filepath.Join(t.TempDir(), "gone.git"))
		e.setRemote()
		got := e.launch()
		if !strings.HasPrefix(got, "caboose: not synced: git fetch") || !strings.HasSuffix(got, "; run 'caboose sync' to see to it\n") ||
			strings.Count(got, "\n") != 1 {
			t.Errorf("said %q", got)
		}
	})
	t.Run("no mount", func(t *testing.T) {
		here, _ := pair(t)
		here.write(filepath.Join(here.ctl, "nomount"), "")
		if got := here.launch(); got != "caboose: not synced: the container has no sync mount for this data dir; 'caboose restart' gives it one\n" {
			t.Errorf("said %q", got)
		}
	})
}

// A remote that does not answer costs a launch the budget, not more, and
// says it is syncing when it takes a while.
func TestAutoSyncSlowRemote(t *testing.T) {
	budget, slow := syncBudget, slowSync
	syncBudget, slowSync = 2*time.Second, 200*time.Millisecond
	defer func() { syncBudget, slowSync = budget, slow }()
	here, _ := pair(t)
	here.write(filepath.Join(here.ctl, "slow"), "")
	// While it waits on the remote, it holds the lock: a caboose sync, or
	// another launch, waits for it.
	locked := make(chan error, 1)
	go func() {
		time.Sleep(time.Second)
		unlock, err := here.syncer().Lock()
		if err == nil {
			unlock()
		}
		locked <- err
	}()
	start := time.Now()
	got := here.launch()
	if err := <-locked; !errors.Is(err, statesync.ErrLocked) {
		t.Errorf("mid-sync, the lock was free: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
	want := "caboose: syncing with the remote\n" +
		"caboose: not synced: git fetch -q origin: no answer from the remote in 2s; run 'caboose sync' to see to it\n"
	if got != want {
		t.Errorf("said\n%s\nwant\n%s", got, want)
	}
}

// A launch that finds a sync running waits for it, and then does not sync
// again: that one just did.
func TestAutoSyncWaitsForARunningSync(t *testing.T) {
	for _, auto := range []string{"1", ""} {
		here, _ := pair(t)
		here.a.Cfg.AutoSync = auto
		unlock, err := here.syncer().Lock()
		if err != nil {
			t.Fatal(err)
		}
		released := make(chan time.Time, 1)
		go func() {
			time.Sleep(500 * time.Millisecond)
			released <- time.Now()
			unlock()
		}()
		got := here.launch()
		finished := time.Now()
		if at := <-released; finished.Before(at) {
			t.Error("did not wait")
		}
		if got != "caboose: waiting for a sync to finish\n" || here.ranGit() {
			t.Errorf("auto_sync=%q: said %q", auto, got)
		}
	}
}
