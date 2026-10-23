package launcher

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

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
// instead, so with either of them that is what is mounted, unchecked --
// unless no agent was asked for outright (agentDeclined): an engine's
// forwarding cannot be pointed at another agent, but it can be left out.
// On Linux the agent's socket is mounted directly. Under vm no engine
// serves the sandbox (the link carries the agent), so docker is never
// asked.
func (a *App) sshAgentSource() string {
	if !a.isVM() && goos == "darwin" && a.macEngine() != "" {
		if _, none := a.agentDeclined(); none {
			return ""
		}
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
// chose it, for messages; "" when there is none. One command asks it
// several times (status, doctor's rows, the mounts), and each answer can
// run ssh -G, which a slow Match exec holds up to sshGTimeout: so it is
// remembered for agentMemoTTL, for the same setting, origin, environment
// and lookup only, which keeps an App that lives long (a daemon's link)
// from answering with what config.toml or ~/.ssh/config said before.
func (a *App) hostAgent() (sock, from string) {
	key := agentMemo{set: a.Cfg.SSHAgent, origin: sshAgentOrigin(a.Cfg), trust: a.Cfg.Trust, env: a.getenv("SSH_AUTH_SOCK"), ssh: a.SSH}
	if m := a.agentSeen; m != nil && m.same(key) && time.Since(m.at) < agentMemoTTL {
		return m.sock, m.from
	}
	key.sock, key.from = a.agentFrom(key.set, key.origin, key.trust)
	key.at = time.Now()
	a.agentSeen = &key
	return key.sock, key.from
}

// agentMemo is one answer of hostAgent's, with what it was asked under.
type agentMemo struct {
	set, origin, trust, env string
	ssh                     *SSHLookup
	at                      time.Time
	sock, from              string
}

func (m *agentMemo) same(k agentMemo) bool {
	return m.set == k.set && m.origin == k.origin && m.trust == k.trust && m.env == k.env && m.ssh == k.ssh
}

// agentMemoTTL is how long hostAgent's answer holds: a command's length,
// not a daemon's.
const agentMemoTTL = 30 * time.Second

// agentFrom is hostAgent for a setting of ssh_agent, set, what set it, and
// trust: that socket ("none" for none); else, under trust = "hostile", no
// agent at all (agentFromHostile), so a sandbox the host does not trust
// gets the keys only when ssh_agent names an agent outright; else the one
// ssh on the host would use by
// default -- an IdentityAgent from ~/.ssh/config (1Password's documented
// setup), else $SSH_AUTH_SOCK, which a tool like a work login's may have
// taken over without anything on the host noticing.
//
// "By default" is for agentProbeHost, which no Host block names: a block
// for one host (github.com with IdentityAgent none, say) does not decide
// the sandbox's agent. An IdentityAgent none that applies to every host
// is no agent, said as such, never $SSH_AUTH_SOCK instead.
//
// ssh -G says which: it reads the config as ssh does. When it fails (no
// ssh, or one that refuses the config) ~/.ssh/config is read for an
// IdentityAgent directly, and what chose the agent says why, for link.log
// and doctor: "$SSH_AUTH_SOCK (ssh -G ... failed: ...)".
func (a *App) agentFrom(set, origin, trust string) (sock, from string) {
	env := a.getenv("SSH_AUTH_SOCK")
	if set != "" {
		return a.expandAgent(set, env), origin
	}
	if trust != config.TrustFallible {
		// Anything but exactly fallible is decided as hostile.
		return "", agentFromHostile
	}
	l := a.sshLookup()
	out, gerr := l.G(agentProbeHost)
	if gerr == nil {
		if v, ok := sshGIdentityAgent(out); ok {
			if v == "none" {
				return "", agentFromNone
			}
			return a.expandAgent(v, env), agentFromConfig
		}
		return env, agentFromEnv
	}
	v, ok, cerr := configIdentityAgent(l)
	switch {
	case ok && v == "none":
		return "", fmt.Sprintf("%s, read directly: %v", agentFromNone, gerr)
	case ok:
		return a.expandAgent(v, env), fmt.Sprintf("%s, read directly: %v", agentFromConfig, gerr)
	case cerr != nil:
		return env, fmt.Sprintf("%s (%v; %v)", agentFromEnv, gerr, cerr)
	}
	return env, fmt.Sprintf("%s (%v)", agentFromEnv, gerr)
}

// agentFromEnv is agentFrom's "what chose it" for the environment's
// SSH_AUTH_SOCK, which a reason may follow; agentFromConfig for an
// IdentityAgent, agentFromNone for an IdentityAgent none.
const (
	agentFromEnv    = "$SSH_AUTH_SOCK"
	agentFromConfig = "IdentityAgent in ~/.ssh/config"
	agentFromNone   = "IdentityAgent none in ~/.ssh/config"
	// agentFromHostile is agentFrom's "what chose it" under hostile with
	// ssh_agent unset.
	agentFromHostile = `trust = "hostile" forwards no SSH agent unless [link] ssh_agent names one`
)

// agentDeclined is what said outright that the sandbox gets no agent, and
// whether something did: ssh_agent = "none", trust = "hostile" with
// ssh_agent unset, or an IdentityAgent none that applies to every host
// (agentFrom). Not an agent that is merely missing
// -- an unset $SSH_AUTH_SOCK, or a $VAR in ssh_agent that names nothing
// -- which on a Mac engine still leaves the engine's own forwarding.
func (a *App) agentDeclined() (why string, none bool) {
	if a.Cfg.SSHAgent == "none" {
		return sshAgentOrigin(a.Cfg) + ` is "none"`, true
	}
	if a.Cfg.SSHAgent != "" {
		return "", false
	}
	if sock, from := a.hostAgent(); sock == "" && (from == agentFromHostile || strings.HasPrefix(from, agentFromNone)) {
		return from, true
	}
	return "", false
}

// sshAgentOrigin says what set ssh_agent, for messages.
func sshAgentOrigin(c *config.Config) string { return c.Origin("link.ssh_agent") }

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
		home, err := a.sshLookup().Home()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, strings.TrimPrefix(v, "~"))
	}
	return v
}

// agentStatus is what the sandbox makes of its forwarded agent, as
// caboose status shows it; usable is false when git there cannot use it.
func (a *App) agentStatus() (status string, usable bool) {
	sock, _ := a.containerEnv("SSH_AUTH_SOCK")
	if why, none := a.agentDeclined(); none && (sock == "" || a.isVM()) {
		// Under vm SSH_AUTH_SOCK is always set: the link's agent listens
		// there, and gives no agent when none was asked for.
		return "not forwarded (" + why + ")", false
	}
	if sock == "" {
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
	case code == 2 && a.isVM():
		// The link carries the agent into a VM: with none running, the
		// agent is out of reach whatever it is.
		if v := a.linkView(); !v.Up {
			if v.Served && v.Daemon == nil {
				return agentUnknownNoDaemon, false
			}
			return "unknown (no link carries it: " + v.Why + ")", false
		}
		return "forwarded, but not reachable from the " + a.noun(), false
	case code == 2:
		return "forwarded, but not reachable from the " + a.noun(), false
	default:
		// No ssh-add in the image: nothing to tell from.
		return "forwarded (not checked: no ssh-add in the image)", true
	}
}

// agentUnknownNoDaemon is agentStatus under vm when the daemon, whose
// link carries the agent, does not answer: as status's other lines say it.
const agentUnknownNoDaemon = "unknown (the daemon is not running or does not answer)"

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
