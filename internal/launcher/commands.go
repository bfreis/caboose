package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
// CABOOSE_PROJECT naming a file changes nothing.
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
	mounts, err := a.Docker.Mounts(a.Cfg.Container)
	if err != nil {
		return "", "", false
	}
	for _, m := range mounts {
		if m.Destination == containerBin {
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
	fmt.Fprintf(out, "container : %s (%s)\n", c.Container, state)
	fmt.Fprintf(out, "image     : %s\n", c.Image)
	for _, r := range c.Roots {
		fmt.Fprintf(out, "repo root : %s -> %s\n", r.Host, r.Container)
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
	if len(c.DockerRunArgs) > 0 {
		fmt.Fprintf(out, "run args  : %s (%s)\n", describeRunArgs(c.DockerRunArgs), runArgsOrigin(c))
	}
	fmt.Fprintf(out, "isolation : %s\n", isolationOf(c))
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

	claude, err := a.Docker.RawOutput("exec", c.Container, "claude", "--version")
	if err != nil {
		claude += "unknown\n"
	}
	fmt.Fprintf(out, "claude    : %s\n", trimNL(claude))

	hostTZ := a.hostTimezone()
	containerTZ, _ := a.containerEnv("TZ")
	fmt.Fprintf(out, "timezone  : %s (host: %s)\n", or(containerTZ, "UTC"), or(hostTZ, "unknown"))
	if a.Docker.Quiet("exec", c.Container, "test", "-S", "/var/run/docker.sock") == nil {
		fmt.Fprintf(out, "docker    : socket MOUNTED - root-equivalent access to this host\n")
	} else {
		fmt.Fprintf(out, "docker    : cli only, no socket (isolated)\n")
	}
	localDir, platform, _ := a.mountedLocal()
	fmt.Fprintf(out, "platform  : %s\n", or(platform, "unknown"))
	fmt.Fprintf(out, "local dir : %s\n", or(localDir, "unknown"))
	agent, _ := a.agentStatus()
	fmt.Fprintf(out, "ssh agent : %s\n", agent)
	if hostTZ != "" && containerTZ != hostTZ {
		a.Note("container was created with a different timezone than the host has now.")
		a.Note("new sessions get '%s' anyway; caboose restart realigns the container.", hostTZ)
	}

	fmt.Fprintf(out, "\ntmux sessions:\n")
	sessions, err := a.Docker.RawOutput("exec", c.Container, "tmux", "list-sessions")
	fmt.Fprint(out, indent(sessions, "  "))
	if err != nil {
		fmt.Fprintf(out, "  (none)\n")
	}

	// Runs outside tmux are plain docker execs: nothing above lists them.
	if l, err := a.liveWork(); err == nil && l.others > 0 {
		fmt.Fprintf(out, "\nClaude Code outside tmux: %d %s (CABOOSE_NO_TMUX, caboose claude -p, background agents)\n",
			l.others, plural(l.others, "process", "processes"))
	}

	fmt.Fprintf(out, "\nregistered Claude Code sessions:\n")
	registered, err := a.Docker.RawOutput("exec", c.Container, "bash", "-c", registeredSessionsScript)
	fmt.Fprint(out, registered)
	if err != nil {
		fmt.Fprintf(out, "  (unavailable)\n")
	}

	liveKeep, _ := a.containerEnv("CABOOSE_KEEP_VERSIONS")
	if liveKeep != "" && liveKeep != c.KeepVersions {
		a.Note("container was created with CABOOSE_KEEP_VERSIONS=%s, shell has %s.", liveKeep, c.KeepVersions)
		a.Note("start-up pruning uses %s until caboose restart; caboose prune uses %s.", liveKeep, c.KeepVersions)
	}
	fmt.Fprintf(out, "\ndisk used by installed claude versions (retaining %s):\n", or(liveKeep, c.KeepVersions))
	usage, _ := du(localDir + "/" + datadir.PlatformShare) // best effort
	fmt.Fprint(out, indent(usage, "  "))
	// A glob, not ls: bash is an image requirement, ls is not.
	versions, _ := a.Docker.RawOutput("exec", c.Container, "bash", "-c",
		`for v in ~/.local/share/claude/versions/*; do [ -e "$v" ] && printf '  %s\n' "${v##*/}"; done; true`)
	fmt.Fprint(out, versions)

	// Each platform keeps its own CABOOSE_KEEP_VERSIONS versions, so a data
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
	return a.Docker.Output("exec", a.Cfg.Container, "bash", "-c", `printf '%s' "${`+name+`-}"`)
}

// Stop stops the container, after confirmSessionLoss.
func (a *App) Stop() error {
	if a.state() != "running" {
		a.Note("container is not running")
		return nil
	}
	if err := a.confirmSessionLoss("stop"); err != nil {
		return err
	}
	if err := a.Docker.Run("stop", a.Cfg.Container); err != nil {
		return dockerFailed(err)
	}
	a.Note("container stopped")
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
	if err := checkRunArgs(a.Cfg.DockerRunArgs, nil, a.Cfg.Roots); err != nil {
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
	a.Note("container recreated")
	return nil
}

// Prune deletes old installed versions now, with the current
// CABOOSE_KEEP_VERSIONS rather than the one baked in at creation. Like
// Logs, it creates a missing container but does not build a missing image.
func (a *App) Prune() error {
	if err := a.ensureRunning(false); err != nil {
		return err
	}
	if err := a.Docker.Stream(a.Stdout, a.Stderr, "exec", "-e", "CABOOSE_KEEP_VERSIONS="+a.Cfg.KeepVersions,
		a.Cfg.Container, Entrypoint, "--cc-prune"); err != nil {
		return dockerFailed(err)
	}
	localDir, platform, ok := a.mountedLocal()
	if !ok {
		return Die("cannot tell which platform dir the container mounts as ~/.local")
	}
	if err := a.reportUsage("caboose: now using ", localDir); err != nil {
		return err
	}
	// Other platforms' dirs are only pruned by a container that mounts them,
	// and one no image uses any more is never pruned at all. Deleting it is
	// the user's call: an image may come back to it.
	for _, r := range a.platformUsage() {
		if r[1] != platform {
			a.Note("%s/%s also holds %s, for a platform this container does not use;", a.Cfg.DataDir, datadir.PlatformDir(r[1]), r[0])
			a.Note("  delete it by hand if no image of yours needs it any more.")
		}
	}
	return nil
}

func (a *App) clientsOn(name string) int {
	out, _ := a.Docker.RawOutput("exec", a.Cfg.Container, "tmux", "list-clients", "-t", "="+name)
	return session.CountLines(out)
}

// Detach detaches any client attached to this project's sessions.
func (a *App) Detach() error {
	if a.state() != "running" {
		a.Note("container is not running")
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
		list, _ := a.Docker.RawOutput("exec", a.Cfg.Container, "tmux", "list-sessions", "-F", "#{session_name}")
		targets = session.ProjectSessions(list, target)
	}
	detached := 0
	for _, t := range targets {
		if a.clientsOn(t) == 0 {
			continue
		}
		if a.Docker.Quiet("exec", a.Cfg.Container, "tmux", "detach-client", "-s", "="+t) == nil {
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
	return a.exec(append(append([]string{"logs"}, args...), a.Cfg.Container)...)
}

// Shell execs a bash prompt inside the container, at the cwd's container
// path. The cwd is checked first, as Attach does: one outside the repo root
// can only fail, and must not do so after building an image and creating a
// container.
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
	// Asked again: the container may only now exist, mounting the roots
	// that were configured.
	workdir, err := a.containerDir(dir)
	if err != nil {
		return err
	}
	argv := append([]string{"exec", "-it"}, a.execEnvArgs()...)
	argv = append(argv, "-w", workdir, a.Cfg.Container, "bash")
	return a.exec(append(argv, args...)...)
}

// Attach is the default: attach a Claude Code session for the current
// directory (or CABOOSE_PROJECT), passing args through to claude.
func (a *App) Attach(args []string) error {
	c := a.Cfg
	projectDir := c.Project
	if projectDir == "" {
		projectDir = "."
	}
	projectDir, err := enterProjectDir(projectDir)
	if err != nil {
		return err
	}
	if _, err := a.containerDir(projectDir); err != nil {
		return err
	}
	if err := a.ensureRunning(true); err != nil {
		return err
	}
	// Asked again: the container may only now exist, mounting the roots
	// that were configured.
	workdir, err := a.containerDir(projectDir)
	if err != nil {
		return err
	}

	name := session.NameFor(projectDir, a.Suffix)

	// Built once, before the branches: a piped invocation wants the timezone
	// just as much as an interactive one does.
	env := a.execEnvArgs()

	terminal := tty.IsTerminal(os.Stdin.Fd()) && tty.IsTerminal(os.Stdout.Fd())
	a.beforeAttach(terminal)
	// The host's end of caboose-agent's link, detached: it outlives this
	// launch, and serves the container while it runs.
	a.startLink()

	if !terminal {
		// No tty to attach a tmux client to — run claude directly so piped
		// and scripted invocations still work.
		argv := append(append([]string{"exec", "-i"}, env...), "-w", workdir, c.Container, Entrypoint)
		return a.exec(append(argv, args...)...)
	}

	// Escape hatch: run Claude Code directly, with no tmux in the way.
	// Rendering is then identical to the host terminal, at the cost of
	// losing detach/reattach -- closing the terminal kills the session.
	if c.NoTmux != "" {
		argv := append(append([]string{"exec", "-it"}, env...), "-w", workdir, c.Container, Entrypoint)
		return a.exec(append(argv, args...)...)
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
	if len(args) > 0 && a.Docker.Quiet("exec", c.Container, "tmux", "has-session", "-t", "="+name) == nil {
		a.Note("reattaching to existing session '%s' — arguments ignored", name)
	}

	// A tmux session inherits the SERVER's environment, not the attaching
	// client's: a TZ on this docker exec does not reach the pane (verified --
	// a session made by a TZ-carrying client still sees TZ unset).
	// `new-session -e` sets it per session, so a session started now is right
	// even against a server started under another zone.
	// Sessions that already exist keep whatever they were created with; -e is
	// ignored on the attach path, harmlessly.
	var tmuxEnv []string
	for _, e := range env {
		if len(e) >= 3 && e[:3] == "TZ=" {
			tmuxEnv = append(tmuxEnv, "-e", e)
		}
	}

	// -u forces UTF-8 regardless of the container's locale, which is
	// otherwise POSIX/C and turns box-drawing characters into mojibake.
	argv := append(append([]string{"exec", "-it"}, env...), c.Container,
		"tmux", "-u", "new-session", "-A", "-s", name, "-c", workdir)
	argv = append(append(argv, tmuxEnv...), Entrypoint)
	// This process becomes the docker exec, and ends with the terminal
	// (attach.go).
	a.recordAttach(name)
	return a.exec(append(argv, args...)...)
}

func (a *App) exec(argv ...string) error {
	if err := a.Docker.Exec(argv...); err != nil {
		return &ExitError{Code: 127, Msg: fmt.Sprintf("cannot run docker: %v", err)}
	}
	return nil
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
