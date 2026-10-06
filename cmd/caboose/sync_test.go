package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/statesync"
)

// syncContainer is a running, ready container "caboose-default" with no sessions,
// mounting the sync repo at mount (the data dir's, unless a test says
// otherwise), whose `docker exec caboose-default git ...` runs the real git on the host
// copy, translating the container path. git is called by absolute path: the
// launcher's own PATH has none, since the host needs no git.
func syncContainer(git, data, mount string) string {
	return containerRunning + `
case "$*" in
  "exec caboose-default test -f /tmp/.caboose-ready") exit 0 ;;
  "exec caboose-default bash -c for d in /proc/"*) echo "` + procRow("7", "1", "sleep") + `"; exit 0 ;;
  "inspect --type=container caboose-default --format "*) printf '` + statesync.ContainerDir + `\t%s\n' "` + mount + `"; exit 0 ;;
esac
if [ "$1" = exec ]; then
  shift
  while :; do
    case "$1" in
      -i|-t) shift ;;
      -e) echo "env $2" >> "` + filepath.Join(data, "..", "exec-env") + `"; shift 2 ;;
      *) break ;;
    esac
  done
  # git, or git under the watchdog: bash -c SCRIPT NAME SECS git ...
  if [ "$1" = caboose-default ] && { [ "$2" = git ] || { [ "$2" = bash ] && [ "$3" = -c ] && [ "$5" = watchdog ]; }; }; then
    shift
    n=$#
    for a; do
      case "$a" in
        ` + statesync.ContainerDir + `) a="` + filepath.Join(data, statesync.Dir) + `" ;;
        git) a=` + git + ` ;;
      esac
      set -- "$@" "$a"
    done
    shift $n
    exec "$@"
  fi
fi`
}

// procRow is one process as the container lists it: pid, ppid, argv[0].
func procRow(pid, ppid, arg0 string) string {
	return pid + " " + ppid + " " + arg0
}

// procs answers the container's process listing with rows.
func procs(rows ...string) string {
	return `case "$*" in "exec caboose-default bash -c for d in /proc/"*) printf '%s\n' "` + strings.Join(rows, `" "`) + `"; exit 0 ;; esac
`
}

// syncEnv is a project under a sandboxed repo root, a fake docker scripted
// with body(git, data dir), and an empty bare remote.
func syncEnv(t *testing.T, body func(git, data string) string) (data, remote, git string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	remote = filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command(git, "init", "-q", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	home := sandboxEnv(t, "CABOOSE_READY_TIMEOUT", "1")
	data = filepath.Join(home, ".caboose", "envs", "default", "data")
	scriptedDocker(t, body(git, data))
	inProject(t, home)
	return data, remote, git
}

func mountedHere(git, data string) string {
	return syncContainer(git, data, filepath.Join(data, statesync.Dir))
}

func TestSyncCommand(t *testing.T) {
	data, remote, git := syncEnv(t, mountedHere)
	// The project's key as the sandbox has it: under /work, whatever HOME is.
	key := statesync.ProjectKey("/work/dev/proj")
	mem := filepath.Join(data, "home", ".claude", "projects", key, "memory")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "m.md"), []byte("a fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errs := runIt("sync")
	if code != 1 || !strings.Contains(errs, "no sync remote yet") || !strings.Contains(errs, "caboose sync --remote URL") {
		t.Errorf("with no remote: exit %d\n%s", code, errs)
	}

	code, out, errs := runIt("sync", "--remote", remote)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	if out != "" {
		t.Errorf("stdout %q", out)
	}
	for _, w := range []string{"syncing " + data + " with " + remote, "sent this machine's changes", "pushed"} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	got, err := exec.Command(git, "--git-dir", remote, "show", "main:home/.claude/projects/"+key+"/memory/m.md").CombinedOutput()
	if err != nil || string(got) != "a fact\n" {
		t.Errorf("the remote holds %q (%v)", got, err)
	}

	code, _, errs = runIt("sync")
	if code != 0 || !strings.Contains(errs, "already in sync") {
		t.Errorf("second sync: exit %d\n%s", code, errs)
	}

	// Its ssh keeps host keys in the sync repo, and may ask about one:
	// someone is at the terminal.
	env, _ := os.ReadFile(filepath.Join(data, "..", "exec-env"))
	if !strings.Contains(string(env), "env GIT_SSH_COMMAND=ssh -o 'UserKnownHostsFile="+statesync.ContainerDir+"/.git/known_hosts ~/.ssh/known_hosts'\n") ||
		strings.Contains(string(env), "BatchMode") || strings.Contains(string(env), "GIT_TERMINAL_PROMPT") {
		t.Errorf("git's environment:\n%s", env)
	}
}

func TestSyncRefusesLiveSessions(t *testing.T) {
	data, _, _ := syncEnv(t, func(git, data string) string {
		return `[ "$*" = "exec caboose-default tmux list-sessions -F #{session_name}" ] && { echo proj; echo proj-2; exit 0; }
` + mountedHere(git, data)
	})
	code, _, errs := runIt("sync")
	if code != 1 || !strings.Contains(errs, "  proj\n  proj-2\n") || !strings.Contains(errs, "refusing to sync") {
		t.Errorf("exit %d\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(data, statesync.Dir, ".git")); err == nil {
		t.Error("the refused sync created the sync repo")
	}
}

// Claude Code runs outside tmux -- a tmux = false session, a `caboose claude -p`
// -- write the data dir just as much, and only the process list shows them. One
// under a tmux session, or started by another, is that one's.
func TestSyncRefusesClaudeOutsideTmux(t *testing.T) {
	for name, tc := range map[string]struct {
		rows    []string
		refused string
	}{
		"no tmux": {[]string{
			procRow("1", "0", "/sbin/docker-init"),
			procRow("20", "0", "/home/agent/.local/bin/claude"),
		}, "  1 Claude Code process outside tmux"},
		"a version binary": {[]string{
			procRow("20", "0", "/home/agent/.local/share/claude/versions/2.1.0"),
		}, "  1 Claude Code process outside tmux"},
		"its children are its own": {[]string{
			procRow("20", "0", "/home/agent/.local/bin/claude"),
			procRow("21", "20", "claude"),
			procRow("22", "20", "node"),
		}, "  1 Claude Code process outside tmux"},
		"in tmux, with no session listed": {[]string{
			procRow("30", "1", "tmux"),
			procRow("31", "30", "/home/agent/.local/bin/claude"),
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			syncEnv(t, func(git, data string) string { return procs(tc.rows...) + mountedHere(git, data) })
			code, _, errs := runIt("sync")
			if tc.refused == "" {
				if strings.Contains(errs, "refusing") {
					t.Errorf("refused:\n%s", errs)
				}
				return
			}
			if code != 1 || !strings.Contains(errs, tc.refused+" ") || !strings.Contains(errs, "refusing to sync") {
				t.Errorf("exit %d\n%s", code, errs)
			}
		})
	}
}

// With no way to tell what runs, a sync does not guess.
func TestSyncRefusesWhenProcessesCannotBeListed(t *testing.T) {
	syncEnv(t, func(git, data string) string {
		return `case "$*" in "exec caboose-default bash -c for d in /proc/"*) echo "bash: /proc: nope" >&2; exit 1 ;; esac
` + mountedHere(git, data)
	})
	code, _, errs := runIt("sync")
	if code != 1 || !strings.Contains(errs, "cannot list what runs in the container") {
		t.Errorf("exit %d\n%s", code, errs)
	}
}

// A container without the sync mount is refused, and so is one created on
// another data dir, which mounts that one's.
func TestSyncNeedsTheMount(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		syncEnv(t, func(git, data string) string {
			return strings.Replace(mountedHere(git, data), statesync.ContainerDir+`\t`, `/other\t`, 1)
		})
		code, _, errs := runIt("sync")
		if code != 1 || !strings.Contains(errs, "where the sync's git runs") || !strings.Contains(errs, "caboose restart") {
			t.Errorf("exit %d\n%s", code, errs)
		}
	})
	t.Run("another data dir", func(t *testing.T) {
		syncEnv(t, func(git, data string) string {
			return syncContainer(git, data, "/elsewhere/sync")
		})
		code, _, errs := runIt("sync")
		if code != 1 || !strings.Contains(errs, "mounts /elsewhere/sync at "+statesync.ContainerDir) {
			t.Errorf("exit %d\n%s", code, errs)
		}
	})
}

func TestSyncNeedsGitInTheContainer(t *testing.T) {
	syncEnv(t, func(git, data string) string {
		return `[ "$*" = "exec caboose-default git --version" ] && { echo 'OCI runtime exec failed: "git": executable file not found' >&2; exit 127; }
` + mountedHere(git, data)
	})
	code, _, errs := runIt("sync")
	if code != 1 || !strings.Contains(errs, "git does not run in the container") || !strings.Contains(errs, "check-image") {
		t.Errorf("exit %d\n%s", code, errs)
	}
}

func TestSyncUsage(t *testing.T) {
	syncEnv(t, mountedHere)
	for _, argv := range [][]string{
		{"sync", "now"},
		{"sync", "--remote"},
		{"sync", "--remote="},
		{"sync", "--remote", "u", "extra"},
	} {
		code, _, errs := runIt(argv...)
		if code != 1 || !strings.Contains(errs, "usage: caboose sync [--remote URL]") {
			t.Errorf("%v: exit %d\n%s", argv, code, errs)
		}
	}
}

// status lists tmux sessions; the runs outside tmux get a line of their own,
// and only when there are some.
func TestStatusCountsClaudeOutsideTmux(t *testing.T) {
	for _, tc := range []struct {
		rows []string
		want bool
	}{
		{[]string{procRow("20", "0", "/home/agent/.local/bin/claude")}, true},
		{[]string{procRow("7", "1", "sleep")}, false},
	} {
		sandboxEnv(t)
		scriptedDocker(t, procs(tc.rows...)+containerRunning)
		_, out, _ := runIt("status")
		line := "\nClaude Code outside tmux: 1 process (tmux = false, caboose claude -p, background agents)\n"
		if strings.Contains(out, line) != tc.want {
			t.Errorf("%q: want the line %v; stdout:\n%s", tc.rows, tc.want, out)
		}
	}
}
