package launcher

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
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
//
// It never reads the sandbox's global git config (GIT_CONFIG_GLOBAL is
// /dev/null): that file can arrive by sync from another machine, and an
// url.*.insteadOf, a core.sshCommand or a credential helper in it would
// redirect the sync's own push. What the sync needs of it is passed on the
// command line instead: gh's credential helper, when gh is in the image,
// for an HTTPS remote. Identity is the sync repo's own (statesync.Init).
type syncGit struct {
	a *App
	// auto is a sync nobody is watching: nothing may prompt, and what
	// reaches the remote must be done by deadline.
	auto     bool
	deadline time.Time
	// ssh is GIT_SSH_COMMAND for every call, "" to leave ssh alone.
	ssh string
	// gh is set when the container has gh, whose credential helper an
	// HTTPS remote then uses.
	gh bool
}

// syncGitProbe prints the container's own ssh command for git, from its
// environment, and then "gh" when gh is on its PATH: one exec, bash
// builtins only.
const syncGitProbe = `printf '%s\n' "${GIT_SSH_COMMAND-}${GIT_SSH-}"; command -v gh >/dev/null && printf gh; true`

// ghHelper is gh's credential helper, as git config spells it.
const ghHelper = "!gh auth git-credential"

// newSyncGit is the git for one sync: auto for one run at launch.
func (a *App) newSyncGit(auto bool) *syncGit {
	g := &syncGit{a: a, auto: auto, deadline: time.Now().Add(syncBudget)}
	out, _ := a.Docker.RawOutput("exec", a.Cfg.Container, "bash", "-c", syncGitProbe)
	own, rest, _ := strings.Cut(out, "\n")
	g.gh = strings.TrimSpace(rest) == "gh"
	// An ssh command the container's environment sets is the user's to
	// keep; git would take GIT_SSH_COMMAND over it.
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
	argv = append(argv, "-e", "GIT_CONFIG_GLOBAL=/dev/null")
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
	// Docker Desktop's file sharing reports a mount's root as owned by
	// root:root for a moment after the host changes something under it,
	// and the host writes the sync repo between git calls: git then refuses
	// the repo as "dubious ownership". The check guards against a repo of
	// another user's, and this one's .git is the sandbox's user's to write
	// anyway, so it adds nothing here. Only on the command line: that is
	// protected config, and the global config is not read.
	argv = append(argv, "git", "-c", "safe.directory="+statesync.ContainerDir)
	if g.gh {
		// The empty one first: it clears any helper the image's system
		// config names, so gh's is the only one asked.
		argv = append(argv, "-c", "credential.helper=", "-c", "credential.helper="+ghHelper)
	}
	argv = append(argv, args...)
	return exec.Command(g.a.Docker.Path, argv...)
}

// remaining is the budget left, in whole seconds, at least one: a call
// after the deadline still gets a moment rather than none.
func (g *syncGit) remaining() int {
	s := int((time.Until(g.deadline) + time.Second - 1) / time.Second)
	return max(s, 1)
}
