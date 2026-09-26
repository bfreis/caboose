package launcher

import (
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/tty"
)

// syncBudget is how long an automatic sync may spend, all told, on what
// reaches the remote: the most a slow or unreachable remote adds to a
// launch. A variable for the tests.
var syncBudget = 20 * time.Second

// watchdog runs a command ("$2"...) for at most "$1" seconds, and exits 124
// saying so when it had to stop it. It runs in the container, since killing
// the `docker exec` client would leave the git it started running there. Only
// bash and sleep, both image requirements. Job control (set -m) puts the
// command in a process group of its own, so the kill takes git's ssh with
// it, and the timer in another, killed whole as soon as git is done: its
// sleep would otherwise hold docker exec's output open for the rest of the
// budget. It holds none of those descriptors either, in case. With job
// control on, some bash versions (5.2, Debian's) report each job that ends
// ("[1]- Terminated") on the shell's stderr, which would stand in for the
// error the caller shows: the shell's own stderr goes to /dev/null, and the
// command and the timeout message write to the real one, kept as fd 3.
const watchdog = `set -m
exec 3>&2 2>/dev/null
t=$1; shift
"$@" 2>&3 3>&- & p=$!
( sleep "$t"; kill -TERM -- "-$p" ) </dev/null >/dev/null 2>&1 3>&- & w=$!
wait "$p"; s=$?
kill -- "-$w"
if [ "$s" = 143 ]; then echo "no answer from the remote in ${t}s" >&3; exit 124; fi
exit "$s"`

// knownHosts is where the sync's ssh keeps host keys: in the sync repo,
// which lives in the data dir, so a remote's key is asked about once, not
// again each time the container is recreated (its ~/.ssh is not kept).
// The container's own ~/.ssh/known_hosts is read too.
const knownHosts = statesync.ContainerDir + "/.git/known_hosts"

// syncGit runs the sync's git in the container.
type syncGit struct {
	a *App
	// auto is a sync nobody is watching: nothing may prompt, and what
	// reaches the remote must be done by deadline.
	auto     bool
	deadline time.Time
	// ssh is GIT_SSH_COMMAND for every call, "" to leave ssh alone.
	ssh string
}

// newSyncGit is the git for one sync: auto for one run at launch.
func (a *App) newSyncGit(auto bool) *syncGit {
	g := &syncGit{a: a, auto: auto, deadline: time.Now().Add(syncBudget)}
	// The sandbox's own ssh command, when it has one, is the user's to
	// keep; git would take GIT_SSH_COMMAND over it.
	own, _ := a.Docker.Output("exec", a.Cfg.Container, "bash", "-c",
		`printf '%s' "${GIT_SSH_COMMAND-}${GIT_SSH-}"; git config --get core.sshCommand`)
	if own == "" {
		g.ssh = "ssh -o 'UserKnownHostsFile=" + knownHosts + " ~/.ssh/known_hosts'"
		if auto {
			// No host key, passphrase or password prompt: fail instead.
			g.ssh += " -o BatchMode=yes"
		}
	}
	return g
}

// command is a statesync.Command. -t only for a command that may prompt,
// when there is a terminal to give it and someone at it.
func (g *syncGit) command(remote, interactive bool, args ...string) *exec.Cmd {
	argv := []string{"exec", "-i"}
	if !g.auto && interactive && tty.IsTerminal(os.Stdin.Fd()) {
		argv = append(argv, "-t")
	}
	if g.auto {
		argv = append(argv, "-e", "GIT_TERMINAL_PROMPT=0")
	}
	if g.ssh != "" {
		argv = append(argv, "-e", "GIT_SSH_COMMAND="+g.ssh)
	}
	argv = append(argv, g.a.Cfg.Container)
	if g.auto && remote {
		argv = append(argv, "bash", "-c", watchdog, "watchdog", strconv.Itoa(g.remaining()))
	}
	argv = append(append(argv, "git"), args...)
	return exec.Command(g.a.Docker.Path, argv...)
}

// remaining is the budget left, in whole seconds, at least one: a call
// after the deadline still gets a moment rather than none.
func (g *syncGit) remaining() int {
	s := int((time.Until(g.deadline) + time.Second - 1) / time.Second)
	return max(s, 1)
}
