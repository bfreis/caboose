package launcher

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// What keeps the sandbox from the host is config.toml's isolation profile
// (config.Isolation, its kind), the host's alone: never the sandbox
// config's, which a session writes and a sync brings. container is runc,
// as caboose has always run; gvisor is the same container under runsc, a
// kernel of its own in user space between the sandbox and the host's.
//
// The runtime is caboose's flag, not one of run_args (ownedFlags),
// because it decides who can write the mounts. An engine that shows
// mounted files as owned by whoever looks (OrbStack; probably Docker
// Desktop) shows them, under gVisor, as owned by gVisor's own process --
// root -- so the agent gets EACCES on its own home. There the container
// runs as root instead, which gVisor keeps inside its kernel; files it
// writes on the mounts still land on the host as the host's user. Which
// case an engine is, is probed, not guessed from its name.

// The isolation kinds, config.Isolation's values.
const (
	isolationContainer = config.KindContainer
	isolationGVisor    = config.KindGVisor
	isolationVM        = config.KindVM
)

// isolationRuntimes are the docker runtime each isolation runs under, ""
// for docker's default.
var isolationRuntimes = map[string]string{isolationContainer: "", isolationGVisor: runscName}

// rootUser is LabelUser for a container that runs as root; "" is the
// image's agent.
const rootUser = "0:0"

// isolationOf is c's isolation kind, container when it names none.
func isolationOf(c *config.Config) string { return or(c.Isolation, isolationContainer) }

// profileOf is c's isolation profile as LabelProfile has it: "<kind>.<name>",
// or the bare kind when config.toml defines no profile.
func profileLabel(c *config.Config) string { return or(c.Profile, isolationOf(c)) }

// isolationSummary is c's isolation for status: the kind, and the profile
// when one is defined.
func isolationSummary(c *config.Config) string {
	if c.Profile == "" {
		return isolationOf(c) + " (the default: config.toml defines no profile)"
	}
	return isolationOf(c) + " (profile " + c.Profile + ")"
}

// otherProfile is how to move off kind: 'caboose setup isolation', which
// writes the profile, and where the current one came from.
func otherProfile(c *config.Config) string {
	return fmt.Sprintf("'%s' chooses %s or %s (%s)", SetupCommand(c.Env, "isolation"), isolationContainer, isolationGVisor, isolationOrigin(c))
}

// isolationOrigin says where c's isolation came from, for messages.
func isolationOrigin(c *config.Config) string {
	switch {
	case c.File.Has("isolation"):
		return "isolation in " + c.File.Path
	case c.Profile != "":
		return "[" + c.Profile + "], the only profile in " + c.File.Path
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
	if a.isVM() {
		return a.checkVM()
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
		return Die("isolation is %s, but docker has no %s runtime (it has %s): '%s' registers it where it can, or chooses another (%s)",
			isolationOf(a.Cfg), rt, strings.Join(have, ", "), SetupCommand(a.Cfg.Env, "isolation"), isolationOrigin(a.Cfg))
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

// runAs is the user the container runs as, as the Spec and LabelUser have
// it, and the environment that goes with it: "" and none -- the image's
// agent -- where the agent can write its mounts; else root, with
// IS_SANDBOX=1 so
// Claude Code allows --dangerously-skip-permissions as root. Only ever
// under gVisor, whose root is inside its own kernel.
func runAs(agentWrites bool) (user string, env []string) {
	if agentWrites {
		return "", nil
	}
	return rootUser, []string{"IS_SANDBOX=1"}
}

// isolate sets the isolation in spec: the runtime, the user, and the
// labels that record both.
func (a *App) isolate(spec *backend.Spec) error {
	iso := isolationOf(a.Cfg)
	user := ""
	if iso == isolationVM {
		// Root in the guest, which the VM bounds: the shares show every
		// file as root's, so the agent user could not tell its own
		// (spike 2).
		var env []string
		user, env = runAs(false)
		spec.User = user
		spec.Env = append(spec.Env, env...)
	} else if rt := isolationRuntimes[iso]; rt != "" {
		writes, err := a.agentCanWrite(rt)
		if err != nil {
			return Die("isolation is %s, but a container under %s did not run: %v", iso, rt, err)
		}
		spec.Runtime = rt
		var env []string
		user, env = runAs(writes)
		spec.User = user
		spec.Env = append(spec.Env, env...)
		if user == rootUser {
			a.Note("under %s the agent user cannot write its mounts on this engine, so the", rt)
			a.Note("sandbox runs as root inside gVisor (files it writes are still yours here)")
		}
	}
	spec.Labels = append(spec.Labels, assets.LabelIsolation+"="+iso, assets.LabelUser+"="+user,
		assets.LabelProfile+"="+profileLabel(a.Cfg))
	return nil
}

// createdIsolation is the isolation the container was created with, and
// the user it runs as; ok is false when that cannot be told, or the
// container records none.
func (a *App) createdIsolation() (iso, user string, ok bool) {
	labels, err := a.box().Labels()
	if err != nil {
		return "", "", false
	}
	iso, ok = labels[assets.LabelIsolation]
	return iso, labels[assets.LabelUser], ok
}

// isolationDrift says how the container's isolation differs from the
// configuration's, or "" when it does not, or it cannot be told.
func (a *App) isolationDrift() string {
	if d := a.missingLabel(assets.LabelIsolation, "isolation"); d != "" {
		return d
	}
	iso, _, ok := a.createdIsolation()
	if !ok {
		return ""
	}
	if iso != isolationOf(a.Cfg) {
		return fmt.Sprintf("the container was created with isolation %s; the configuration says %s", iso, isolationOf(a.Cfg))
	}
	if d := a.missingLabel(assets.LabelProfile, "isolation profile"); d != "" {
		return d
	}
	labels, err := a.box().Labels()
	if err != nil || labels[assets.LabelProfile] == profileLabel(a.Cfg) {
		return ""
	}
	return fmt.Sprintf("the container was created with profile %s; the configuration says %s", labels[assets.LabelProfile], profileLabel(a.Cfg))
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
	if a.isVM() {
		a.doctorVM(c)
		a.doctorEgress(c)
		a.doctorMaxVnodes(c)
		return
	}
	have, err := a.engineRuntimes()
	if err != nil {
		c.unchecked("isolation", "%s; docker info did not list its runtimes: %v", isolationOf(cfg), err)
		return
	}
	rt := isolationRuntimes[isolationOf(cfg)]
	switch {
	case rt != "" && !slices.Contains(have, rt):
		c.problem("isolation", fmt.Sprintf("%s, which registers it or chooses another (%s)", SetupCommand(cfg.Env, "isolation"), isolationOrigin(cfg)),
			"%s needs docker's %s runtime, which it does not have (it has %s)", isolationOf(cfg), rt, strings.Join(have, ", "))
	case rt != "":
		c.ok("isolation", "%s (%s)", isolationOf(cfg), rt)
		a.doctorRunsc(c)
	case slices.Contains(have, isolationRuntimes[isolationGVisor]):
		c.note("isolation", "%s, which shares this machine's kernel; docker has runsc, so a %s profile ('%s') would give the sandbox a kernel of its own",
			isolationOf(cfg), isolationGVisor, SetupCommand(cfg.Env, "isolation"))
	default:
		c.note("isolation", "%s, the weakest: the sandbox shares this machine's kernel (%s offers gVisor where it can)",
			isolationOf(cfg), SetupCommand(cfg.Env, "isolation"))
	}
}

// doctorRunsc checks the runsc docker has loaded. Any runsc needs
// --host-uds=open for the forwarded SSH agent, which one registered by
// gVisor's own install lacks; caboose registers its own with it, so the fix
// said for that one is setup's. One caboose downloaded
// never updates by itself, so doctor says when it is old, or of a release
// caboose did not record; setup isolation checks it against gVisor's
// latest and offers the update. Doctor fetches nothing.
func (a *App) doctorRunsc(c *checkup) {
	loaded, ok, err := a.engineRuntime(runscName)
	if err != nil || !ok {
		return
	}
	dir := a.cabooseRunscDir(loaded.Path)
	setup := SetupCommand(a.Cfg.Env, "isolation")
	if !runscHostUDS(loaded.RuntimeArgs) {
		fix := hostUDSFix()
		if dir != "" {
			fix = setup
		}
		const why = "runsc runs without --host-uds=open, so the sandbox cannot reach the SSH agent caboose forwards"
		if a.sshAgentSource() != "" {
			c.problem("runsc", fix, "%s", why)
		} else {
			c.note("runsc", "%s, once there is one: %s", why, fix)
		}
	}
	if dir == "" {
		return
	}
	rec, known := readRunscRelease(dir)
	switch age := a.now().Sub(rec.Downloaded); {
	case !known:
		c.note("runsc", "caboose's gVisor in %s has no readable record of its release: %s checks it for a newer one",
			a.short(dir), setup)
	case age > runscStale:
		c.note("runsc", "caboose's gVisor in %s was downloaded %d days ago, and gVisor releases about weekly: %s checks it for a newer one",
			a.short(dir), int(age.Hours()/24), setup)
	default:
		c.ok("runsc", "caboose's gVisor, downloaded %s", rec.Downloaded.Local().Format(time.DateOnly))
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
	if iso, user, ok := a.createdIsolation(); ok && user == rootUser && iso != isolationVM {
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

// startFailed is a start of the sandbox (noun name) that failed, said with
// what to do: it is stopped, so recreating it loses no session.
func startFailed(noun, name string, err error) error {
	return Die("%s %s did not start: %s\n"+
		"       'caboose restart' recreates it; it is stopped, so no sessions are lost.", noun, name, firstLine(err.Error()))
}
