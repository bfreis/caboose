package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// goos is runtime.GOOS, a variable so that tests can play the Mac.
var goos = runtime.GOOS

const (
	// hostServicesAgent is the socket OrbStack and Docker Desktop provide
	// inside their Linux VM, forwarding the Mac's SSH agent. It exists only
	// there, never on the Mac, so it cannot be checked for from the host.
	hostServicesAgent = "/run/host-services/ssh-auth.sock"
	// containerAgent is where the container sees the forwarded agent.
	containerAgent = "/ssh-agent.sock"
)

// sshAgentSource is the host's end of the SSH agent to mount at
// containerAgent, or "" for none: forwarding it lets git and ssh in the sandbox authenticate -- and sign commits --
// with keys that never enter it.
//
// On a Mac the engine runs in a VM, and a Mac socket bind-mounted into it
// arrives as an empty directory: OrbStack and Docker Desktop pass through
// only macOS's own launchd agent that way, which is exactly what 1Password
// (or Secretive) is not. Both engines provide hostServicesAgent for this
// instead, so with either of them that is what is mounted, unchecked. On
// Linux the agent's socket is mounted directly.
func (a *App) sshAgentSource() string {
	if goos == "darwin" && a.macEngine() != "" {
		return hostServicesAgent
	}
	if s, _ := a.hostAgent(); s != "" && isSocket(s) {
		return s
	}
	return ""
}

// macEngine names the Docker engine when it is one that provides
// hostServicesAgent, or is "".
func (a *App) macEngine() string {
	os, err := a.Docker.Output("info", "--format", "{{.OperatingSystem}}")
	if err != nil {
		return ""
	}
	for _, e := range []string{"OrbStack", "Docker Desktop"} {
		if strings.Contains(os, e) {
			return e
		}
	}
	return ""
}

// hostAgent is the host's SSH agent socket the sandbox gets, and what
// chose it, for messages; "" when there is none.
func (a *App) hostAgent() (sock, from string) {
	return a.agentFrom(a.Cfg.SSHAgent, sshAgentOrigin(a.Cfg))
}

// agentFrom is hostAgent for a setting of ssh_agent, set, and what set it:
// that socket ("none" for none), else the one ssh on the host would use for
// github.com -- an IdentityAgent from ~/.ssh/config (1Password's documented
// setup), else $SSH_AUTH_SOCK, which a tool like a work login's may have
// taken over without anything on the host noticing.
func (a *App) agentFrom(set, origin string) (sock, from string) {
	env := a.getenv("SSH_AUTH_SOCK")
	if set != "" {
		return a.expandAgent(set, env), origin
	}
	const fromEnv = "$SSH_AUTH_SOCK"
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return env, fromEnv
	}
	out, err := exec.Command(ssh, "-G", "github.com").Output()
	if err != nil {
		return env, fromEnv
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.EqualFold(k, "identityagent") {
			continue
		}
		return a.expandAgent(strings.Trim(strings.TrimSpace(v), `"`), env), "IdentityAgent in ~/.ssh/config"
	}
	return env, fromEnv
}

// sshAgentOrigin says what set ssh_agent, for messages.
func sshAgentOrigin(c *config.Config) string {
	if c.File != nil && c.SSHAgentFrom == c.File.Path {
		return "ssh_agent in " + c.File.Path
	}
	return or(c.SSHAgentFrom, "CABOOSE_SSH_AGENT")
}

// expandAgent resolves an IdentityAgent value as ssh does: "none" is no
// agent, SSH_AUTH_SOCK (bare or as $SSH_AUTH_SOCK) is the environment's,
// $VAR another variable, and ~ the home directory.
func (a *App) expandAgent(v, env string) string {
	switch {
	case v == "none":
		return ""
	case v == "SSH_AUTH_SOCK":
		return env
	case strings.HasPrefix(v, "$"):
		return a.getenv(strings.Trim(v[1:], "{}"))
	case v == "~" || strings.HasPrefix(v, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, strings.TrimPrefix(v, "~"))
	}
	return v
}

// agentStatus is what the sandbox makes of its forwarded agent, as
// caboose status shows it; usable is false when git there cannot use it.
func (a *App) agentStatus() (status string, usable bool) {
	if sock, _ := a.containerEnv("SSH_AUTH_SOCK"); sock == "" {
		return "not forwarded (the host had no agent when the " + a.noun() + " was created)", false
	}
	out, err := backend.Output(a.box(), "ssh-add", "-l")
	switch code := docker.ExitCode(err); {
	case err == nil:
		n := len(strings.Split(strings.TrimSpace(out), "\n"))
		return "forwarded, " + plural(n, "1 key", strconv.Itoa(n)+" keys"), true
	case code == 1:
		return "forwarded, but the agent holds no keys", false
	case code == 2 && strings.Contains(err.Error(), "Permission denied"):
		return "forwarded, but the sandbox's user may not use it (permission denied)", false
	case code == 2:
		return "forwarded, but not reachable from the " + a.noun(), false
	default:
		// No ssh-add in the image: nothing to tell from.
		return "forwarded (not checked: no ssh-add in the image)", true
	}
}

// openAgentSocket lets the sandbox's user reach hostServicesAgent where the
// engine makes it root:root 0660, as Docker Desktop does; OrbStack's is open
// already. It runs on every launch, not only when the launch starts the
// container: the engine makes the socket anew each time it starts, and
// restarts the container itself (--restart unless-stopped), so a launch
// after an engine restart finds it running with the socket closed again.
// It changes nothing when that user can write the socket already: under
// gVisor, where the container runs as root, it never has to. The chmod reaches the socket in the engine's VM, for
// every container there, but so does root in any of them already.
func (a *App) openAgentSocket() {
	mounts, err := a.box().Mounts()
	if err != nil || !slices.Contains(mounts, backend.Mount{Source: hostServicesAgent, Target: containerAgent}) {
		return
	}
	if _, err := backend.Output(a.box(), "test", "-w", containerAgent); err == nil {
		return
	}
	chmod := backend.ExecSpec{Argv: []string{"chmod", "0666", containerAgent}, User: "0"}
	if _, err := backend.Capture(a.box(), chmod, nil); err != nil {
		a.Note("SSH agent: could not open %s to the sandbox's user: %v", containerAgent, err)
	}
}

// warnIfAgentUnusable says so, at container creation, when the forwarded
// agent is empty or unreachable -- the state in which every signed commit
// and every push over SSH in the sandbox fails.
func (a *App) warnIfAgentUnusable() {
	if a.isVM() {
		return // the link carries it, and is not up yet
	}
	status, usable := a.agentStatus()
	if usable || strings.HasPrefix(status, "not forwarded") {
		return
	}
	a.Note("SSH agent: %s.", status)
	if goos == "darwin" && !strings.Contains(status, "permission denied") {
		a.Note("  %s forwards the agent it was started with. For 1Password's, make", or(a.macEngine(), "the Docker engine"))
		a.Note("  SSH_AUTH_SOCK point at it for apps started outside a terminal: the launch")
		a.Note("  agent in 1Password's \"Configure SSH_AUTH_SOCK globally\" (a section of")
		a.Note("  https://www.1password.dev/ssh/agent/compatibility/, not a setting),")
		a.Note("  then restart %s; 'caboose status' shows the agent again.", or(a.macEngine(), "it"))
	}
}
