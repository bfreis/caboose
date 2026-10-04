package launcher

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// What keeps the sandbox from the host is config.toml's isolation key
// (config.Isolation), the host's alone: never the sandbox config's, which
// a session writes and a sync brings. docker is runc, as caboose has always
// run; gvisor is the same container under runsc, a kernel of its own in
// user space between the sandbox and the host's.
//
// The runtime is caboose's flag, not one of docker_run_args (ownedFlags),
// because it decides who can write the mounts. An engine that shows
// mounted files as owned by whoever looks (OrbStack; probably Docker
// Desktop) shows them, under gVisor, as owned by gVisor's own process --
// root -- so the agent gets EACCES on its own home. There the container
// runs as root instead, which gVisor keeps inside its kernel; files it
// writes on the mounts still land on the host as the host's user. Which
// case an engine is, is probed, not guessed from its name.

// The values of config.Isolation.
const (
	isolationDocker = "docker"
	isolationGVisor = "gvisor"
)

// isolationRuntimes are the docker runtime each isolation runs under, ""
// for docker's default.
var isolationRuntimes = map[string]string{isolationDocker: "", isolationGVisor: runscName}

// rootUser is LabelUser for a container that runs as root; "" is the
// image's agent.
const rootUser = "0:0"

// isolationOf is c's isolation, docker when it names none.
func isolationOf(c *config.Config) string { return or(c.Isolation, isolationDocker) }

// checkIsolation refuses an isolation caboose does not know.
func checkIsolation(c *config.Config) error {
	if _, ok := isolationRuntimes[isolationOf(c)]; !ok {
		return fmt.Errorf("isolation %q is not %q or %q (%s)", c.Isolation, isolationDocker, isolationGVisor, isolationOrigin(c))
	}
	return nil
}

func isolationOrigin(c *config.Config) string {
	if c.Getenv != nil && c.Getenv("CABOOSE_ISOLATION") != "" {
		return "CABOOSE_ISOLATION"
	}
	if c.File != nil {
		return "isolation in " + c.File.Path
	}
	return "the default"
}

// engineRuntimes are the runtimes docker has registered.
func (a *App) engineRuntimes() ([]string, error) {
	out, err := a.Docker.Output("info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return nil, fmt.Errorf("docker info: runtimes: %v", err)
	}
	var names []string
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}

// engineRuntime is the runtime docker's engine has loaded as name (its
// path and flags, as docker info says them), and whether it has one.
func (a *App) engineRuntime(name string) (daemonRuntime, bool, error) {
	out, err := a.Docker.Output("info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return daemonRuntime{}, false, err
	}
	var m map[string]daemonRuntime
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return daemonRuntime{}, false, fmt.Errorf("docker info: runtimes: %v", err)
	}
	rt, ok := m[name]
	return rt, ok, nil
}

// checkRuntime refuses an isolation whose runtime docker does not have.
// Registering runsc is not a launch's to do: it is the engine's daemon
// config, which only setup edits, and only after asking.
func (a *App) checkRuntime() error {
	if err := checkIsolation(a.Cfg); err != nil {
		return Die("%v", err)
	}
	rt := isolationRuntimes[isolationOf(a.Cfg)]
	if rt == "" {
		return nil
	}
	have, err := a.engineRuntimes()
	if err != nil {
		return dockerFailed(err)
	}
	if !slices.Contains(have, rt) {
		return Die("isolation is %s, but docker has no %s runtime (it has %s): '%s' registers it where it can, or set isolation = %q (%s)",
			isolationOf(a.Cfg), rt, strings.Join(have, ", "), SetupCommand(a.Cfg.Env, "isolation"), isolationDocker, isolationOrigin(a.Cfg))
	}
	return nil
}

// agentCanWrite reports whether the agent, under runtime, can write the
// data dir's home/.claude as it is mounted -- false on an engine that
// shows mounted files as owned by whoever looks, since under gVisor that
// is gVisor's own process. An error is a probe that did not run at all.
func (a *App) agentCanWrite(runtime string) (bool, error) {
	out, err := a.Docker.Output("run", "--rm", "--runtime", runtime, "--network", "none",
		"--user", "agent", "--entrypoint", "sh",
		"-v", filepath.Join(a.Cfg.DataDir, datadir.HomeDir, ".claude")+":/p",
		a.Cfg.Image, "-c", "if test -w /p; then echo yes; else echo no; fi")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// runAs are the docker run arguments for the user the container runs as,
// and that user as LabelUser has it: none and "" -- the image's agent --
// where the agent can write its mounts; else root, with IS_SANDBOX=1 so
// Claude Code allows --dangerously-skip-permissions as root. Only ever
// under gVisor, whose root is inside its own kernel.
func runAs(agentWrites bool) (args []string, user string) {
	if agentWrites {
		return nil, ""
	}
	return []string{"--user", rootUser, "-e", "IS_SANDBOX=1"}, rootUser
}

// isolationArgs are createContainer's docker run arguments for the
// isolation: the runtime, the user, and the labels that record both.
func (a *App) isolationArgs() ([]string, error) {
	iso := isolationOf(a.Cfg)
	var args []string
	user := ""
	if rt := isolationRuntimes[iso]; rt != "" {
		writes, err := a.agentCanWrite(rt)
		if err != nil {
			return nil, Die("isolation is %s, but a container under %s did not run: %v", iso, rt, err)
		}
		args = append(args, "--runtime", rt)
		var as []string
		as, user = runAs(writes)
		args = append(args, as...)
		if user == rootUser {
			a.Note("under %s the agent user cannot write its mounts on this engine, so the", rt)
			a.Note("sandbox runs as root inside gVisor (files it writes are still yours here)")
		}
	}
	return append(args,
		"--label", assets.LabelIsolation+"="+iso,
		"--label", assets.LabelUser+"="+user), nil
}

// createdIsolation is the isolation the container was created with, and
// the user it runs as; ok is false when that cannot be told. A container
// from before the labels ran as docker, as the agent.
func (a *App) createdIsolation() (iso, user string, ok bool) {
	labels, err := a.Docker.ContainerLabels(a.Cfg.Container)
	if err != nil {
		return "", "", false
	}
	return or(labels[assets.LabelIsolation], isolationDocker), labels[assets.LabelUser], true
}

// isolationDrift says how the container's isolation differs from the
// configuration's, or "" when it does not, or it cannot be told.
func (a *App) isolationDrift() string {
	iso, _, ok := a.createdIsolation()
	if !ok || iso == isolationOf(a.Cfg) {
		return ""
	}
	return fmt.Sprintf("the container was created with isolation %s; the configuration says %s", iso, isolationOf(a.Cfg))
}

// warnIfIsolationDrifted is warnIfRunArgsDrifted for the isolation.
func (a *App) warnIfIsolationDrifted() {
	if d := a.isolationDrift(); d != "" {
		a.Note("%s.", d)
		a.Note("run 'caboose restart' to recreate it with it (this kills running sessions).")
	}
}

// doctorIsolation checks that the isolation's runtime is there, and names
// docker as the weakest, saying when gvisor would work.
func (a *App) doctorIsolation(c *checkup) {
	cfg := a.Cfg
	if checkIsolation(cfg) != nil {
		return // the configuration's row says why
	}
	have, err := a.engineRuntimes()
	if err != nil {
		c.unchecked("isolation", "%s; docker info did not list its runtimes: %v", isolationOf(cfg), err)
		return
	}
	rt := isolationRuntimes[isolationOf(cfg)]
	switch {
	case rt != "" && !slices.Contains(have, rt):
		c.problem("isolation", fmt.Sprintf("%s, or set isolation = %q (%s)", SetupCommand(cfg.Env, "isolation"), isolationDocker, isolationOrigin(cfg)),
			"%s needs docker's %s runtime, which it does not have (it has %s)", isolationOf(cfg), rt, strings.Join(have, ", "))
	case rt != "":
		c.ok("isolation", "%s (%s)", isolationOf(cfg), rt)
	case slices.Contains(have, isolationRuntimes[isolationGVisor]):
		c.note("isolation", "%s, which shares this machine's kernel; docker has runsc, so isolation = %q would give the sandbox a kernel of its own",
			isolationOf(cfg), isolationGVisor)
	default:
		c.note("isolation", "%s, the weakest: the sandbox shares this machine's kernel (%s offers gVisor where it can)",
			isolationOf(cfg), SetupCommand(cfg.Env, "isolation"))
	}
}

// doctorContainerIsolation checks the container's isolation against the
// configuration's, and says when it runs as root, and why.
func (a *App) doctorContainerIsolation(c *checkup) {
	if rt := a.runtimeGone(); rt != "" {
		c.problem("isolation", "the next launch recreates it with isolation "+isolationOf(a.Cfg)+", or caboose restart",
			"the container was created under %s, which docker no longer has: it cannot start again", rt)
		return
	}
	if d := a.isolationDrift(); d != "" {
		c.problem("isolation", "caboose restart"+endsSessions, "%s", d)
		return
	}
	if iso, user, ok := a.createdIsolation(); ok && user == rootUser {
		c.note("isolation", "the container runs as root inside %s: on this engine the agent user cannot write its mounts under it", iso)
	}
}

// runtimeGone is the runtime the container was created under when docker
// no longer has it, else "". The runtime is fixed at creation, so such a
// container can never start again, whatever config.toml says now: docker
// start fails with "unknown or invalid runtime name". It happens when runsc
// is unregistered while the container is stopped -- and an engine restart,
// which registering needs, stops it.
func (a *App) runtimeGone() string {
	iso, _, ok := a.createdIsolation()
	if !ok {
		return ""
	}
	rt := isolationRuntimes[iso]
	if rt == "" {
		return ""
	}
	if have, err := a.engineRuntimes(); err != nil || slices.Contains(have, rt) {
		return ""
	}
	return rt
}

// recreateForRuntime replaces a stopped container whose runtime is gone
// with one of the configured isolation: it holds no sessions, and could
// never start again. When the configured isolation cannot work either,
// nothing is removed, and the error says both what happened and what fixes
// it.
func (a *App) recreateForRuntime(rt string, mayBuild bool) error {
	name := a.Cfg.Container
	if err := a.checkRuntime(); err != nil {
		return Die("container %s was created under %s, which docker no longer has, so it cannot start.\n"+
			"       A new one cannot be created either: %v", name, rt, err)
	}
	a.Note("container %s was created under %s, which docker no longer has, so it cannot start again.", name, rt)
	a.Note("it is stopped, so no sessions are lost: recreating it with isolation %s (%s).", isolationOf(a.Cfg), isolationOrigin(a.Cfg))
	if err := a.removeContainer(); err != nil {
		return err
	}
	return a.createContainer(mayBuild)
}

// startFailed is a docker start that failed, said with what to do: the
// container is stopped, so recreating it loses no session.
func startFailed(name string, err error) error {
	return Die("container %s did not start: %s\n"+
		"       'caboose restart' recreates it; it is stopped, so no sessions are lost.", name, firstLine(err.Error()))
}
