package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

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

// sshAgentArgs are the `docker run` arguments that forward the host's SSH
// agent, so git and ssh in the sandbox authenticate -- and sign commits --
// with keys that never enter it.
//
// On a Mac the engine runs in a VM, and a Mac socket bind-mounted into it
// arrives as an empty directory: OrbStack and Docker Desktop pass through
// only macOS's own launchd agent that way, which is exactly what 1Password
// (or Secretive) is not. Both engines provide hostServicesAgent for this
// instead, so with either of them that is what is mounted, unchecked. On
// Linux the agent's socket is mounted directly.
func (a *App) sshAgentArgs() []string {
	if goos == "darwin" && a.macEngine() != "" {
		return []string{"-v", hostServicesAgent + ":" + containerAgent, "-e", "SSH_AUTH_SOCK=" + containerAgent}
	}
	if s := a.hostAgent(); s != "" && isSocket(s) {
		return []string{"-v", s + ":" + containerAgent, "-e", "SSH_AUTH_SOCK=" + containerAgent}
	}
	return nil
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

// hostAgent is the agent socket ssh on the host would use for github.com:
// an IdentityAgent from ~/.ssh/config (1Password's documented setup), else
// $SSH_AUTH_SOCK. "" when there is none.
func (a *App) hostAgent() string {
	env := a.getenv("SSH_AUTH_SOCK")
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return env
	}
	out, err := exec.Command(ssh, "-G", "github.com").Output()
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.EqualFold(k, "identityagent") {
			continue
		}
		return a.expandAgent(strings.Trim(strings.TrimSpace(v), `"`), env)
	}
	return env
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
		return "not forwarded (the host had no agent when the container was created)", false
	}
	out, err := a.Docker.Output("exec", a.Cfg.Container, "ssh-add", "-l")
	switch code := docker.ExitCode(err); {
	case err == nil:
		n := len(strings.Split(strings.TrimSpace(out), "\n"))
		return "forwarded, " + plural(n, "1 key", strconv.Itoa(n)+" keys"), true
	case code == 1:
		return "forwarded, but the agent holds no keys", false
	case code == 2:
		return "forwarded, but not reachable from the container", false
	default:
		// No ssh-add in the image: nothing to tell from.
		return "forwarded (not checked: no ssh-add in the image)", true
	}
}

// warnIfAgentUnusable says so, at container creation, when the forwarded
// agent is empty or unreachable -- the state in which every signed commit
// and every push over SSH in the sandbox fails.
func (a *App) warnIfAgentUnusable() {
	status, usable := a.agentStatus()
	if usable || strings.HasPrefix(status, "not forwarded") {
		return
	}
	a.Note("SSH agent: %s.", status)
	if goos == "darwin" {
		a.Note("  %s forwards the agent it was started with. For 1Password's, make", or(a.macEngine(), "the Docker engine"))
		a.Note("  SSH_AUTH_SOCK point at it for apps started outside a terminal (1Password:")
		a.Note("  \"Configure SSH_AUTH_SOCK globally\", https://developer.1password.com/docs/ssh/agent/compatibility/),")
		a.Note("  then restart %s; 'caboose status' shows the agent again.", or(a.macEngine(), "it"))
	}
}
