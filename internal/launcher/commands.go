package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/session"
	"github.com/bfreis/caboose/internal/tty"
	"github.com/bfreis/caboose/internal/version"
)

// registeredSessionsScript lists the Claude Code sessions registered in the
// container, for caboose status.
const registeredSessionsScript = `
        shopt -s nullglob
        files=(~/.claude/sessions/*.json)
        [ ${#files[@]} -gt 0 ] || { echo "  (none)"; exit 0; }
        for f in "${files[@]}"; do
            jq -r "\"  \(.name // \"?\")  [\(.status // \"?\")]  \(.cwd // \"?\")\"" "$f" 2>/dev/null
        done`

// du is `du -sh path` on the host, "" on any failure.
func du(path string) (string, error) {
	out, err := exec.Command("du", "-sh", path).Output()
	return string(out), err
}

// enterProjectDir is `cd "$dir" && pwd -P`: the physical path of dir, which
// must be a directory. It runs before anything has side effects, so a
// directory that is gone changes nothing.
func enterProjectDir(dir string) (string, error) {
	p, err := config.Physical(dir)
	if err != nil {
		return "", Die("cannot enter the project dir: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", Die("cannot enter the project dir: %v", err)
	}
	if !fi.IsDir() {
		return "", Die("cd: %s: Not a directory", dir)
	}
	return p, nil
}

// reportUsage prints what the installed versions in localDir (the dir the
// container has mounted at ~/.local, see mountedLocal) now take up,
// prefixed. A failing du fails the command, its own stderr discarded.
func (a *App) reportUsage(prefix, localDir string) error {
	out, err := du(localDir + "/" + datadir.PlatformShare)
	fmt.Fprint(a.Stdout, indent(out, prefix))
	if err != nil {
		return &ExitError{Code: 1}
	}
	return nil
}

// containerBin is where the container has the Claude Code launcher symlink.
const containerBin = "/home/agent/.local/bin"

// mountedLocal is the platform dir the container has mounted as its
// ~/.local (bin/ and share/claude/ in it), and its platform. Asked of the
// container rather than worked out from the image, since only a caboose
// restart makes a container pick up another image's dir. ok is false when
// the container cannot be asked, or mounts no platform dir there.
func (a *App) mountedLocal() (dir, platform string, ok bool) {
	mounts, err := a.box().Mounts()
	if err != nil {
		return "", "", false
	}
	for _, m := range mounts {
		if m.Target == containerBin {
			return datadir.MountedLocalDir(m.Source)
		}
	}
	return "", "", false
}

// platformUsage is `du -sh` of each platform dir in the data dir, as
// "SIZE<TAB>platform" pairs; a dir du cannot size shows as "?".
func (a *App) platformUsage() [][2]string {
	platforms, _ := datadir.PlatformDirsIn(a.Cfg.DataDir)
	var rows [][2]string
	for _, p := range platforms {
		size := "?"
		if out, err := du(filepath.Join(a.Cfg.DataDir, datadir.PlatformDir(p))); err == nil {
			if f := strings.Fields(out); len(f) > 0 {
				size = f[0]
			}
		}
		rows = append(rows, [2]string{size, p})
	}
	return rows
}

// Status prints the container, version and live session summary. Its first
// four lines are parsed by tests/run.sh, so their format is fixed; the
// launcher's version follows them, so it shows whether or not the container
// runs. caboose version says more about it, and about the image. tests/run.sh
// also reads "local dir :", the dir the running container has mounted as
// ~/.local, so that line's format is fixed too.
func (a *App) Status() error {
	c, out := a.Cfg, a.Stdout
	state := a.state()
	fmt.Fprintf(out, "env       : %s\n", c.Env)
	fmt.Fprintf(out, "%s %s (%s)\n", a.nounLabel(), c.Container, state)
	fmt.Fprintf(out, "image     : %s\n", c.Image)
	for _, r := range c.Roots {
		fmt.Fprintf(out, "root      : %s -> %s\n", r.Host, r.Container)
	}
	fmt.Fprintf(out, "data dir  : %s\n", c.DataDir)
	switch {
	case c.File != nil:
		fmt.Fprintf(out, "config    : %s\n", c.File.Path)
	case c.EnvDir != "":
		fmt.Fprintf(out, "config    : none (%s/%s would be read)\n", c.EnvDir, config.FileName)
	}
	if sb, err := a.sandboxConfig(); err == nil {
		var kept []string
		for _, k := range sb.Keep {
			kept = append(kept, "~/"+k.Rel)
		}
		fmt.Fprintf(out, "keeps     : %s (in %s/%s)\n", strings.Join(kept, ", "), c.DataDir, datadir.HomeDir)
	}
	if len(c.RunArgs) > 0 {
		fmt.Fprintf(out, "run args  : %s (%s)\n", describeRunArgs(c.RunArgs), runArgsOrigin(c))
	}
	fmt.Fprintf(out, "isolation : %s\n", isolationSummary(c))
	if a.isVM() {
		fmt.Fprintf(out, "outbound  : %s\n", a.egressSummary())
	}
	if h := a.hostExecSummary(); h != "" {
		fmt.Fprintf(out, "host exec : %s\n", h)
	}
	fmt.Fprintf(out, "version   : %s\n", version.Get().Version)
	if state != "running" {
		fmt.Fprintf(out, "\nnot running — start it by running caboose in a repo.\n")
		return nil
	}
	if p := a.compatProblem(a.containerCompat()); p != "" {
		a.Note("%s; run 'caboose restart' (this kills running sessions).", p)
	}
	a.warnIfImageDrifted()
	a.warnIfKeepDrifted()
	a.warnIfRunArgsDrifted()
	a.warnIfIsolationDrifted()
	a.warnIfEgressDrifted()
	a.warnIfHostnameDrifted()

	claude, err := backend.RawOutput(a.box(), "claude", "--version")
	if err != nil {
		claude += "unknown\n"
	}
	fmt.Fprintf(out, "claude    : %s\n", trimNL(claude))

	hostTZ := a.hostTimezone()
	containerTZ, _ := a.containerEnv("TZ")
	fmt.Fprintf(out, "timezone  : %s (host: %s)\n", or(containerTZ, "UTC"), or(hostTZ, "unknown"))
	// Under docker and gvisor the line is about this machine's engine: the
	// sandbox has its CLI, and its socket only when mounted. Under vm no
	// engine of this machine's is involved, and a dockerd in the VM is a
	// feature of the sandbox's, so it is a line of its own name.
	sock := backend.Quiet(a.box(), "test", "-S", "/var/run/docker.sock") == nil
	switch {
	case a.isVM() && sock:
		fmt.Fprintf(out, "dockerd   : in the VM, its own, not this machine's (images kept in %s)\n", a.dockerDisk())
	case a.isVM():
		fmt.Fprintf(out, "dockerd   : none in the VM (the image has none, as caboose check-image says, or it did not start: caboose logs)\n")
	case sock:
		fmt.Fprintf(out, "docker    : socket MOUNTED - root-equivalent access to this host\n")
	default:
		fmt.Fprintf(out, "docker    : cli only, no socket (isolated)\n")
	}
	localDir, platform, _ := a.mountedLocal()
	fmt.Fprintf(out, "platform  : %s\n", or(platform, "unknown"))
	fmt.Fprintf(out, "local dir : %s\n", or(localDir, "unknown"))
	agent, _ := a.agentStatus()
	fmt.Fprintf(out, "ssh agent : %s\n", agent)
	if hostTZ != "" && containerTZ != hostTZ {
		a.Note("%s was created with a different timezone than the host has now.", a.noun())
		a.Note("new sessions get '%s' anyway; caboose restart realigns the %s.", hostTZ, a.noun())
	}

	fmt.Fprintf(out, "\ntmux sessions:\n")
	sessions, err := backend.RawOutput(a.box(), "tmux", "list-sessions")
	fmt.Fprint(out, indent(sessions, "  "))
	if err != nil {
		fmt.Fprintf(out, "  (none)\n")
	}

	// Runs outside tmux are plain docker execs: nothing above lists them.
	if l, err := a.liveWork(); err == nil && l.others > 0 {
		fmt.Fprintf(out, "\nClaude Code outside tmux: %d %s (tmux = false, caboose claude -p, background agents)\n",
			l.others, plural(l.others, "process", "processes"))
	}

	fmt.Fprintf(out, "\nregistered Claude Code sessions:\n")
	registered, err := backend.RawOutput(a.box(), "bash", "-c", registeredSessionsScript)
	fmt.Fprint(out, registered)
	if err != nil {
		fmt.Fprintf(out, "  (unavailable)\n")
	}

	keep := strconv.Itoa(c.KeepVersions)
	liveKeep, _ := a.containerEnv("CABOOSE_KEEP_VERSIONS")
	if liveKeep != "" && liveKeep != keep {
		a.Note("%s was created with keep_versions = %s, config.toml has %s.", a.noun(), liveKeep, keep)
		a.Note("start-up pruning uses %s until caboose restart; caboose prune uses %s.", liveKeep, keep)
	}
	fmt.Fprintf(out, "\ndisk used by installed claude versions (retaining %s):\n", or(liveKeep, keep))
	usage, _ := du(localDir + "/" + datadir.PlatformShare) // best effort
	fmt.Fprint(out, indent(usage, "  "))
	// A glob, not ls: bash is an image requirement, ls is not.
	versions, _ := backend.RawOutput(a.box(), "bash", "-c",
		`for v in ~/.local/share/claude/versions/*; do [ -e "$v" ] && printf '  %s\n' "${v##*/}"; done; true`)
	fmt.Fprint(out, versions)

	// Each platform keeps its own keep_versions versions, so a data
	// dir used with more than one image holds more than one set.
	if rows := a.platformUsage(); len(rows) > 0 {
		fmt.Fprintf(out, "\ndisk used per platform (%s/<platform>, each with its own versions):\n", datadir.LocalRoot)
		for _, r := range rows {
			mark := ""
			if r[1] == platform {
				mark = "  (mounted)"
			}
			fmt.Fprintf(out, "  %s\t%s%s\n", r[0], r[1], mark)
		}
	}
	return nil
}

// containerEnv is a variable of the container's environment, "" when unset,
// read with bash's printf rather than printenv: bash is an image
// requirement, printenv (coreutils, or a BusyBox applet) is not.
func (a *App) containerEnv(name string) (string, error) {
	return backend.Output(a.box(), "bash", "-c", `printf '%s' "${`+name+`-}"`)
}

// Stop stops the container, after confirmSessionLoss.
func (a *App) Stop() error {
	if a.state() != "running" {
		a.Note("%s is not running", a.noun())
		return nil
	}
	if err := a.confirmSessionLoss("stop"); err != nil {
		return err
	}
	if err := a.box().Stop(); err != nil {
		return a.boxFailed(err)
	}
	a.Note("%s stopped", a.noun())
	return nil
}

// Restart recreates the container, after confirmSessionLoss.
//
// The image the new container needs is seen to before the old one goes:
// built when missing, rebuilt for a changed base, or refused (see
// ensureImage). A build takes minutes, and the sessions keep running
// through it; one that fails, or a refusal, leaves them running.
func (a *App) Restart() error {
	if err := a.Cfg.CheckImages(); err != nil {
		return Die("%v", err)
	}
	// Refused now, not once the container is gone.
	if err := checkRunArgs(a.Cfg.RunArgs, nil, a.Cfg.Roots); err != nil {
		return a.runArgsError(err)
	}
	if err := a.checkRuntime(); err != nil {
		return err
	}
	if err := a.confirmSessionLoss("restart"); err != nil {
		return err
	}
	if _, err := a.ensureImage(true); err != nil {
		return err
	}
	if err := a.removeContainer(); err != nil {
		return err
	}
	if err := a.ensureRunning(true); err != nil {
		return err
	}
	a.Note("%s recreated", a.noun())
	return nil
}

// Prune deletes old installed versions now, with the current
// keep_versions rather than the one baked in at creation. Like
// Logs, it creates a missing container but does not build a missing image.
// --docker deletes the vm sandbox's docker disk instead (pruneDocker).
func (a *App) Prune(args []string) error {
	switch {
	case len(args) == 1 && args[0] == "--docker":
		return a.pruneDocker()
	case len(args) > 0:
		return &ExitError{Code: 2, Msg: fmt.Sprintf("usage: caboose prune [--docker] (got %q)", args[0])}
	}
	if err := a.ensureRunning(false); err != nil {
		return err
	}
	cmd := a.box().Command(backend.ExecSpec{
		Argv: []string{Entrypoint, "--cc-prune"}, Env: []string{"CABOOSE_KEEP_VERSIONS=" + strconv.Itoa(a.Cfg.KeepVersions)},
	})
	cmd.Stdout, cmd.Stderr = a.Stdout, a.Stderr
	if err := cmd.Run(); err != nil {
		return a.boxFailed(err)
	}
	localDir, platform, ok := a.mountedLocal()
	if !ok {
		return Die("cannot tell which platform dir the %s mounts as ~/.local", a.noun())
	}
	if err := a.reportUsage("caboose: now using ", localDir); err != nil {
		return err
	}
	// Other platforms' dirs are only pruned by a container that mounts them,
	// and one no image uses any more is never pruned at all. Deleting it is
	// the user's call: an image may come back to it.
	for _, r := range a.platformUsage() {
		if r[1] != platform {
			a.Note("%s/%s also holds %s, for a platform this %s does not use;", a.Cfg.DataDir, datadir.PlatformDir(r[1]), r[0], a.noun())
			a.Note("  delete it by hand if no image of yours needs it any more.")
		}
	}
	return nil
}

// dockerDisk is the disk the vm sandbox's dockerd keeps its images on.
func (a *App) dockerDisk() string {
	return filepath.Join(a.vmRoot(), "volumes", dockerVolume+".img")
}

// pruneDocker deletes the disk the vm sandbox's dockerd keeps everything
// on -- images, containers, volumes, build cache -- for an empty one, after
// saying what it frees and asking (CABOOSE_FORCE=1 does not ask). The disk is in
// use while the VM runs, so a running one is stopped first, ending its
// sessions: they are listed, and the one question covers them too. The
// empty disk is made at once, cloned from the template a build made, and
// a start makes one too when it is missing. Under docker and gvisor the
// sandbox has no dockerd of its own: what it builds is the engine's.
func (a *App) pruneDocker() error {
	if !a.isVM() {
		return Die("prune --docker is for isolation vm, whose sandbox runs a dockerd of its own; under %s it has none: "+
			"what docker does in it is this machine's engine's, and 'docker system prune' here cleans that up", isolationOf(a.Cfg))
	}
	disk := a.dockerDisk()
	if !isFile(disk) {
		a.Note("no docker disk at %s: nothing to delete (the sandbox's next start makes an empty one)", disk)
		return nil
	}
	size := "?"
	if out, err := du(disk); err == nil {
		if f := strings.Fields(out); len(f) > 0 {
			size = f[0]
		}
	}
	a.Note("this deletes %s (%s on disk): every image, container, volume and", disk, size)
	a.Note("  build cache of the sandbox's dockerd, for an empty disk")
	running := a.state() == "running"
	if running {
		a.Note("the VM runs on it, so it is stopped first, which ends any session in it")
		if sessions, _ := backend.Output(a.box(), "tmux", "list-sessions", "-F", "#{session_name}"); sessions != "" {
			a.Note("these live session(s) end:")
			fmt.Fprint(a.Stderr, indent(sessions+"\n", "  "))
		}
	}
	if a.force() {
		a.Note("CABOOSE_FORCE=1 set, continuing.")
	} else {
		yes, asked := a.askYes("delete it?")
		switch {
		case !asked:
			return Die("refusing to delete it non-interactively (set CABOOSE_FORCE=1 to delete it)")
		case !yes:
			return Die("aborted; nothing was changed")
		}
	}
	if running {
		if err := a.box().Stop(); err != nil {
			return a.boxFailed(err)
		}
		a.Note("%s stopped", a.noun())
	}
	if err := os.Remove(disk); err != nil {
		return Die("cannot delete %s: %v", disk, err)
	}
	a.Note("deleted %s, freeing %s", disk, size)
	if _, err := (&vmHost{a}).Volume(dockerVolume); err != nil {
		a.Note("no empty disk made yet (%v); the sandbox's next start makes it", err)
	}
	if running {
		a.Note("'caboose' starts the %s again, its dockerd empty", a.noun())
	}
	return nil
}

func (a *App) clientsOn(name string) int {
	out, _ := backend.RawOutput(a.box(), "tmux", "list-clients", "-t", "="+name)
	return session.CountLines(out)
}

// Detach detaches any client attached to this project's sessions.
func (a *App) Detach() error {
	if a.state() != "running" {
		a.Note("%s is not running", a.noun())
		return nil
	}
	cwd, err := config.Cwd()
	if err != nil {
		return Die("%v", err)
	}
	target := session.NameFor(cwd, a.Suffix)
	// An explicit name means that session and no other. Without one, the
	// project may own a whole series, and detaching only the base would leave
	// the terminal that asked still staring at a mirrored pane.
	var targets []string
	if a.Suffix != "" {
		targets = []string{target}
	} else {
		list, _ := backend.RawOutput(a.box(), "tmux", "list-sessions", "-F", "#{session_name}")
		targets = session.ProjectSessions(list, target)
	}
	detached := 0
	for _, t := range targets {
		if a.clientsOn(t) == 0 {
			continue
		}
		if backend.Quiet(a.box(), "tmux", "detach-client", "-s", "="+t) == nil {
			a.Note("detached clients from '%s' (session still running)", t)
			detached++
		}
	}
	if detached == 0 {
		a.Note("no attached client on session '%s'", target)
	}
	return nil
}

// Logs execs `docker logs [args] CONTAINER` for the supervisor log. It
// brings the container up first (tests/run.sh leans on that to exercise the
// data dir set-up), but will not build a missing image
// for it: minutes of building to read a log nobody has written yet.
func (a *App) Logs(args []string) error {
	if err := a.ensureRunning(false); err != nil {
		return err
	}
	if a.isVM() {
		return a.vmLogs(args)
	}
	// docker's own logs, flags and all: the one command that is the
	// engine's rather than the sandbox's.
	return a.exec(a.Docker.Command(append(append([]string{"logs"}, args...), a.Cfg.Container)...))
}

// vmLogs is caboose logs under vm: the VM's console, which holds what the
// entrypoint said, its last 100 lines or --tail N's.
func (a *App) vmLogs(args []string) error {
	lines := 100
	switch {
	case len(args) == 0:
	case len(args) == 2 && (args[0] == "--tail" || args[0] == "-n"):
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return Die("--tail takes a number of lines, not %q", args[1])
		}
		lines = n
	default:
		return Die("under isolation vm, caboose logs takes only --tail N: there is no docker logs to pass %q to", strings.Join(args, " "))
	}
	return a.box().Logs(a.Stdout, a.Stderr, lines)
}

// Shell execs a bash prompt inside the container, at the cwd's container
// path. The cwd is checked first, as Attach does: one outside the repo root
// can only fail, and must not do so after building an image and creating a
// container.
//
// It has a terminal only when caboose has one, as Attach does: docker exec
// -t refuses a stdin that is no terminal, and a script or a pipe running
// `caboose shell -c CMD` wants CMD's output as it is. Without one, a bash
// given no -c reads its commands from stdin, which is what
// `caboose shell < script` means.
func (a *App) Shell(args []string) error {
	dir, err := config.Cwd()
	if err != nil {
		return Die("%v", err)
	}
	if _, err := a.containerDir(dir); err != nil {
		return err
	}
	if err := a.ensureRunning(true); err != nil {
		return err
	}
	a.awaitProxy()
	// Asked again: the container may only now exist, mounting the roots
	// that were configured.
	workdir, err := a.containerDir(dir)
	if err != nil {
		return err
	}
	return a.exec(a.box().Command(backend.ExecSpec{
		Argv: append([]string{"bash"}, args...), Env: a.execEnv(), Dir: workdir, Stdin: true, TTY: a.isTerminal(),
	}))
}

// Attach is the default: attach a Claude Code session for the current
// directory, passing args through to claude.
func (a *App) Attach(args []string) error {
	c := a.Cfg
	projectDir, err := enterProjectDir(".")
	if err != nil {
		return err
	}
	if _, err := a.containerDir(projectDir); err != nil {
		return err
	}
	if err := a.ensureRunning(true); err != nil {
		return err
	}
	a.awaitProxy()
	// Asked again: the container may only now exist, mounting the roots
	// that were configured.
	workdir, err := a.containerDir(projectDir)
	if err != nil {
		return err
	}

	name := session.NameFor(projectDir, a.Suffix)

	// Built once, before the branches: a piped invocation wants the timezone
	// just as much as an interactive one does.
	env := a.execEnv()

	terminal := a.isTerminal()
	a.beforeAttach(terminal)
	// The host's end of caboose-agent's link, detached: it outlives this
	// launch, and serves the container while it runs.
	a.startLink()

	if !terminal {
		// No tty to attach a tmux client to — run claude directly so piped
		// and scripted invocations still work.
		return a.exec(a.box().Command(backend.ExecSpec{
			Argv: append([]string{Entrypoint}, args...), Env: env, Dir: workdir, Stdin: true,
		}))
	}

	// Escape hatch: run Claude Code directly, with no tmux in the way.
	// Rendering is then identical to the host terminal, at the cost of
	// losing detach/reattach -- closing the terminal kills the session.
	if !c.Tmux {
		return a.exec(a.box().Command(backend.ExecSpec{
			Argv: append([]string{Entrypoint}, args...), Env: env, Dir: workdir, Stdin: true, TTY: true,
		}))
	}

	// Without an explicit name, step past whatever another terminal is
	// holding. This is why a second `caboose` in one project does not open a
	// second view of the first: the name it would collide on is in use, so
	// it takes the next one in the series instead.
	if a.Suffix == "" {
		base := name
		if name, err = session.FirstFree(base, a.clientsInUse); err != nil {
			return Die("%v", err)
		}
		if name != base {
			a.Note("'%s' is open in another terminal; starting '%s' here", base, name)
		}
	}

	// -A attaches to the session if it already exists, which is the
	// detach/reattach path; note that `claude` args only apply when the
	// session is first created.
	if len(args) > 0 && backend.Quiet(a.box(), "tmux", "has-session", "-t", "="+name) == nil {
		a.Note("reattaching to existing session '%s' — arguments ignored", name)
	}

	argv := tmuxSessionArgv(name, workdir, env, args)
	// This process becomes the docker exec, and ends with the terminal
	// (attach.go).
	a.recordAttach(name)
	return a.exec(a.box().Command(backend.ExecSpec{Argv: argv, Env: env, Stdin: true, TTY: true}))
}

// isTerminal says whether caboose runs on a terminal, stdin and stdout
// both: what a command run in the sandbox gets a terminal of its own for.
func (a *App) isTerminal() bool {
	if a.terminal != nil {
		return a.terminal()
	}
	return tty.IsTerminal(os.Stdin.Fd()) && tty.IsTerminal(os.Stdout.Fd())
}

// exec replaces this process with cmd, so signals, the TTY and the exit
// code belong to it and not to a Go parent process. It returns only on
// failure.
func (a *App) exec(cmd *exec.Cmd) error {
	if a.replace != nil {
		return a.replace(cmd)
	}
	err := cmd.Err
	if err == nil {
		err = syscall.Exec(cmd.Path, cmd.Args, cmd.Environ())
	}
	what := "docker"
	if a.isVM() {
		what = "the command in the VM"
	}
	return &ExitError{Code: 127, Msg: fmt.Sprintf("cannot run %s: %v", what, err)}
}

func trimNL(s string) string {
	for len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// tmuxSessionArgv is the tmux command that attaches the session name, or
// creates it in workdir running claude with args, given execEnv's env.
func tmuxSessionArgv(name, workdir string, env, args []string) []string {
	// A tmux session inherits the SERVER's environment, not the attaching
	// client's: a TZ on this exec does not reach the pane (verified -- a
	// session made by a TZ-carrying client still sees TZ unset).
	// `new-session -e` sets it per session, so a session started now is right
	// even against a server started under another zone.
	//
	// The terminal's name is the exception: tmux sets TERM_PROGRAM and
	// TERM_PROGRAM_VERSION to its own in every pane, over -e. Only the
	// command can set them back, so claude starts under env with the
	// host's -- what lets it tell which terminal it draws on, past tmux, and
	// so how to notify there.
	//
	// Sessions that already exist keep whatever they were created with; -e
	// and the command are ignored on the attach path, harmlessly.
	var tmuxEnv, cmdEnv []string
	for _, e := range env {
		switch k, _, _ := strings.Cut(e, "="); k {
		case "TZ":
			tmuxEnv = append(tmuxEnv, "-e", e)
		case "TERM_PROGRAM", "TERM_PROGRAM_VERSION":
			cmdEnv = append(cmdEnv, e)
		}
	}
	cmd := []string{Entrypoint}
	if len(cmdEnv) > 0 {
		cmd = append(append([]string{"/usr/bin/env"}, cmdEnv...), Entrypoint)
	}
	// -u forces UTF-8 regardless of the container's locale, which is
	// otherwise POSIX/C and turns box-drawing characters into mojibake.
	argv := append([]string{"tmux", "-u", "new-session", "-A", "-s", name, "-c", workdir}, tmuxEnv...)
	return append(append(argv, cmd...), args...)
}
