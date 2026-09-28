package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/tty"
)

// ReadyMarker is the file the entrypoint creates once Claude Code is usable.
const ReadyMarker = "/tmp/.caboose-ready"

func (a *App) state() string { return a.Docker.ContainerState(a.Cfg.Container) }

// warnIfImageDrifted compares the image the container was created from
// against the image that tag points at now. Deliberately only a warning:
// auto-recreating would silently kill background agents, which is the thing
// this design exists to protect.
func (a *App) warnIfImageDrifted() {
	running := a.Docker.ContainerImage(a.Cfg.Container)
	current := a.Docker.ImageID(a.Cfg.Image)
	if running != "" && current != "" && running != current {
		a.Note("container is running an older image than '%s'.", a.Cfg.Image)
		a.Note("run 'caboose restart' to pick it up (this kills running sessions).")
	}
}

// compatOf is assets.Compat as the launcher that built an image had it, from
// the image's labels. ok is false when that cannot be told: no label (an
// image caboose build did not make) or one that is not a number.
func compatOf(labels map[string]string) (compat int, ok bool) {
	n, err := strconv.Atoi(labels[assets.LabelCompat])
	return n, err == nil
}

// containerCompat is compatOf the container, whose labels are its image's
// as it was created from it; ok is false when that cannot be told (no
// container, docker not answering).
func (a *App) containerCompat() (compat int, ok bool) {
	labels, err := a.Docker.ContainerLabels(a.Cfg.Container)
	if err != nil {
		return 0, false
	}
	return compatOf(labels)
}

// compatProblem says why this launcher cannot work with a container whose
// image is compat n, or "": a launcher updates itself, and may meet a
// container another one created, which works on as it is unless
// assets.Compat says it cannot.
func (a *App) compatProblem(n int, ok bool) string {
	if !ok || n == assets.Compat {
		return ""
	}
	which := "an older"
	if n > assets.Compat {
		which = "a newer"
	}
	return fmt.Sprintf("%s was created by %s caboose, which this one cannot work with (compat %d, this caboose's %d)",
		a.Cfg.Container, which, n, assets.Compat)
}

// compatRefusal is the error a launch stops at for a container whose image
// is compat n.
func (a *App) compatRefusal(n int, ok bool) error {
	if p := a.compatProblem(n, ok); p != "" {
		return Die("%s;\n       run 'caboose restart' to recreate it from this caboose's image (this ends running sessions)", p)
	}
	return nil
}

// checkExisting is what a launch checks of a container that exists, before
// using it: refused when this launcher cannot work with it, and otherwise
// warned about when it runs an older image than the tag, or the tag's image
// is stale.
func (a *App) checkExisting() error {
	if err := a.compatRefusal(a.containerCompat()); err != nil {
		return err
	}
	a.warnIfImageDrifted()
	a.warnIfLocalImageStale()
	return nil
}

// mountedRoots are the roots the container actually has mounted under
// /work, or nil when the container does not exist yet -- in which case the
// configured ones are about to become the truth anyway.
//
// The roots are bind-mounted when the container is created, so changing
// them afterwards does not move the mounts. Ask the container what it
// actually has rather than trusting this process's values: read as it is,
// that puts its sessions where it has their projects.
func (a *App) mountedRoots() []config.Root {
	mounts, err := a.Docker.Mounts(a.Cfg.Container)
	if err != nil {
		return nil
	}
	var roots []config.Root
	for _, m := range mounts {
		if m.Destination == config.WorkDir || strings.HasPrefix(m.Destination, config.WorkDir+"/") {
			roots = append(roots, config.Root{Host: m.Source, Container: m.Destination})
		}
	}
	return roots
}

// visibleRoots are the roots as seen from inside: mounted, or configured
// when nothing is mounted yet.
func (a *App) visibleRoots() []config.Root {
	if m := a.mountedRoots(); len(m) > 0 {
		return m
	}
	return a.Cfg.Roots
}

// CheckInsideRoot finds where the container sees host path dir, under the
// roots it can actually see (mounted, or configured when nothing is mounted
// yet). Checking against the configured roots instead would wave through a
// path that is not mounted -- the session then dies inside tmux with an
// opaque "no such directory" for a container path the user never typed.
//
// origin is config.RootsOrigin for configured. The messages say what the
// repo root is and where its value came from, because the default is one
// person's layout and a new user meets this having never heard of it.
func CheckInsideRoot(dir string, mounted, configured []config.Root, origin string) (string, error) {
	roots := mounted
	if len(roots) == 0 {
		roots = configured
	}
	if p, ok := config.ContainerPath(roots, dir); ok {
		return p, nil
	}
	if !config.SameRoots(roots, configured) {
		msg := fmt.Sprintf(`%s is outside the %s this container has mounted.
       mounted: %s (fixed when the container was created)
       current: %s (%s)
       Bind mounts cannot change under a live container: 'caboose restart'
       remounts %s at the current value (this kills running sessions).`,
			dir, plural(len(roots), "root", "roots"), config.DescribeRoots(roots),
			config.DescribeRoots(configured), origin, plural(len(roots), "it", "them"))
		if _, ok := config.ContainerPath(configured, dir); !ok {
			msg += "\n       That alone would not do: " + dir + " is outside the current value too.\n" + config.RepoRootHelp
		}
		return "", Die("%s", msg)
	}
	what := "the mounted repo root"
	if len(roots) > 1 || roots[0].Container != config.WorkDir {
		what = "every mounted root"
	}
	return "", Die("%s is outside %s, %s (%s).\n%s", dir, what, config.DescribeRoots(roots), origin, config.RepoRootHelp)
}

// containerDir is where the container sees host dir, or the error saying
// why it cannot.
func (a *App) containerDir(dir string) (string, error) {
	return CheckInsideRoot(dir, a.mountedRoots(), a.Cfg.Roots, a.Cfg.RootsOrigin())
}

// mountList is roots as "host at container, ...", always saying where:
// in a drift, that may be all that differs.
func mountList(roots []config.Root) string {
	var parts []string
	for _, r := range roots {
		parts = append(parts, r.Host+" at "+r.Container)
	}
	return strings.Join(parts, ", ")
}

// sshDir is where the container has the sandbox's ~/.ssh.
const sshDir = config.ContainerHome + "/.ssh"

// warnIfRootsDrifted says when the running container mounts other roots, or
// mounts them elsewhere, than the configuration now says: a root changed,
// added or removed since the container was created. Only a warning, as for
// a drifted image: the fix, a restart, kills sessions.
func (a *App) warnIfRootsDrifted() {
	mounted := a.mountedRoots()
	if len(mounted) == 0 || config.SameRoots(mounted, a.Cfg.Roots) {
		return
	}
	a.Note("the container mounts %s; the configuration says %s.", mountList(mounted), mountList(a.Cfg.Roots))
	a.Note("run 'caboose restart' to remount (this kills running sessions).")
}

// prepareDataDir and syncSandboxInstructions are cheap and idempotent, so
// they run on every launch and also repair a data dir that lost a piece --
// not just freshly created ones. What is kept, and so created and mounted,
// is the sandbox config's (keep.go), whose defaults are written here when
// there is none and no sync could bring one.
func (a *App) prepareDataDir() error {
	if err := a.writeSandboxDefaults(); err != nil {
		return Die("%v", err)
	}
	sb, err := a.sandboxConfig()
	if err != nil {
		return Die("%v", err)
	}
	if err := datadir.EnsureLayout(a.Cfg.DataDir, sb.Keep); err != nil {
		return Die("preparing %s: %v", a.Cfg.DataDir, err)
	}
	return nil
}

// noteSetup says, in one line, when the environment has not been set up
// (no config.toml) or the sandbox has no git identity to commit with. A
// launch never copies the host's identity and signing in: caboose setup
// asks, and a launch only says so, every time until it is done. Reading the identity is best-effort: no git on
// the host, or a config that is a link (doctor says that), is quiet.
func (a *App) noteSetup() {
	c := a.Cfg
	if a.inSetup {
		return
	}
	setUp := c.File != nil || c.EnvDir == ""
	hasID := true
	if git := datadir.FindGit(); git != nil {
		if g, err := datadir.ReadSandboxGit(c.DataDir, git); err == nil {
			hasID = g.Complete()
		}
	}
	switch {
	case !setUp && !hasID:
		a.Note("environment '%s' is not set up, and the sandbox has no git identity to commit with: run '%s'",
			c.Env, SetupCommand(c.Env, ""))
	case !setUp:
		a.Note("environment '%s' is not set up, and runs on defaults: '%s' asks for its settings",
			c.Env, SetupCommand(c.Env, ""))
	case !hasID:
		a.Note("the sandbox has no git identity (user.name, user.email), so commits in it fail: run '%s'",
			SetupCommand(c.Env, "git"))
	}
}

func (a *App) syncSandboxInstructions() error {
	src, err := SandboxInstructions(a.Checkout)
	if err != nil {
		return nil // nothing to install, as when the tracked file is missing
	}
	// The roots the container really has, as for containerDir: the file
	// describes what is visible from inside, which a root changed since
	// creation does not alter.
	roots := a.visibleRoots()
	// The checkout's copy wins so that an edit needs no rebuild -- but the
	// placeholders are this binary's to fill, and a pull can bring a copy
	// with one it predates. Installing @@SOMETHING@@ literally would mislead
	// every session, so this binary's own copy, which it can fill, goes in
	// instead until the launcher is rebuilt.
	if a.Checkout != "" {
		if unknown := datadir.UnknownPlaceholders(datadir.ExpandInstructions(src, a.Checkout, roots)); len(unknown) > 0 {
			if embedded, err := assets.SandboxInstructions(); err == nil {
				a.Note("%s in the checkout uses placeholders this launcher doesn't know (%s);",
					assets.SandboxInstructionsPath, strings.Join(unknown, ", "))
				a.Note("installing the copy built into it instead. Rebuild it: make launcher (on the host).")
				src = embedded
			}
		}
	}
	changed, err := datadir.InstallInstructions(src, a.Checkout, roots, a.Cfg.DataDir)
	if errors.Is(err, nofollow.ErrNotPlain) {
		a.Note("not installing sandbox CLAUDE.md: %v; delete it and relaunch", err)
		return nil
	}
	if err != nil {
		return Die("installing sandbox CLAUDE.md: %v", err)
	}
	if changed {
		a.Note("installed sandbox CLAUDE.md into %s/%s/CLAUDE.md", a.Cfg.DataDir, datadir.ClaudeDir)
	}
	return nil
}

// createContainer creates the container, building the image first when
// there is none and mayBuild allows it (see ensureImage).
func (a *App) createContainer(mayBuild bool) error {
	c := a.Cfg
	// Already done by ensureRunning, but every mount source below must exist
	// before `docker run`, so this does not rely on the caller.
	if err := a.prepareDataDir(); err != nil {
		return err
	}
	// Before a build that may take minutes; again below, with the mounts.
	if err := checkRunArgs(c.DockerRunArgs, nil, c.Roots); err != nil {
		return a.runArgsError(err)
	}
	labels, err := a.ensureImage(mayBuild)
	if err != nil {
		return err
	}
	platform, err := a.preparePlatformDir(labels)
	if err != nil {
		return err
	}
	a.noteInstall(filepath.Join(c.DataDir, datadir.PlatformDir(platform)), platform)

	args := []string{
		"run",
		"-d",
		"--name", c.Container,
		"--hostname", "caboose",
		"--restart", "unless-stopped",
		"-e", "CABOOSE_KEEP_VERSIONS=" + c.KeepVersions,
		// Reap zombies: a long-lived container accumulates them from agents,
		// MCP servers and tool subprocesses in a way a --rm one never did.
		"--init",
		// And on docker stop, signal the entrypoint's whole process group,
		// not the entrypoint alone: what a start.d script left running is
		// in it, and would otherwise die by SIGKILL, never hearing a TERM.
		"-e", "TINI_KILL_PROCESS_GROUP=1",
	}

	// Claude Code's musl build cannot run the ripgrep it bundles, and is
	// told to use the image's (which the image check requires on musl) by
	// this. It is set here, from the platform label, rather than in the
	// layer: ENV cannot depend on the libc, and a glibc image must not get
	// it at all. Configuration at creation, not an install (the design's
	// "Principle"); like the platform itself it changes with caboose restart.
	if strings.HasSuffix(platform, "-musl") {
		args = append(args, "-e", "USE_BUILTIN_RIPGREP=0")
	}

	// Container-wide, so the supervisor and any tmux server born inside
	// agree with the host. Per-session TZ is handled again at attach time,
	// because this one is frozen at creation.
	if tz := a.hostTimezone(); tz != "" {
		args = append(args, "-e", "TZ="+tz)
	}

	if sock := DockerSockPath(c.DockerSock, a.getenv("DOCKER_HOST")); sock != "" {
		if isSocket(sock) {
			args = append(args, "-v", sock+":/var/run/docker.sock")
			// On a Linux host the socket is root:docker 0660, so the
			// container user needs its group to reach it at all. macOS hosts
			// expose it through the VM already reachable, where this is a
			// no-op. Lstat: the socket itself, not what a link names.
			var st syscall.Stat_t
			if syscall.Lstat(sock, &st) == nil {
				args = append(args, "--group-add", strconv.FormatUint(uint64(st.Gid), 10))
			}
			a.Note("WARNING: mounting %s into the container.", sock)
			a.Note("         that is root-equivalent access to this host, and the")
			a.Note("         sandbox boundary no longer holds. unset CABOOSE_DOCKER_SOCK")
			a.Note("         and caboose restart to undo it.")
		} else {
			a.Note("CABOOSE_DOCKER_SOCK is set but '%s' is not a socket; not mounting it", sock)
		}
	}

	// SSH agent forwarding lets git/jj push over SSH, and git sign commits,
	// without copying private keys into the image or the data dir.
	args = append(args, a.sshAgentArgs()...)

	d := c.DataDir
	// What the sandbox keeps of its home: the sandbox config's [[keep]]
	// entries, each kept in the data dir's home/, never at a host path of
	// the sandbox's choosing, so it reaches no more of the host than it
	// did. Directories, but for a file entry or two: git, jj and gh save by
	// rename, which a single-file mount refuses with EBUSY.
	sb, err := a.sandboxConfig()
	if err != nil {
		return Die("%v", err)
	}
	args = append(args, keepMounts(d, sb)...)
	// The Claude Code build for this image's platform (datadir's platform.go
	// has why each piece is split, the update staging included).
	pm := datadir.PlatformMounts(platform)
	args = append(args,
		"-v", d+"/"+pm[0]+":/home/agent/.local/bin",
		"-v", d+"/"+pm[1]+":/home/agent/.local/share/claude",
		"-v", d+"/"+pm[2]+":/home/agent/.cache/claude",
		// caboose sync's repo: the container's git runs it (launcher/sync.go).
		"-v", d+"/"+datadir.SyncDir+":"+statesync.ContainerDir,
		// Where sessions propose what only the host can change (apply.go).
		"-v", d+"/"+datadir.ProposalsDir+":"+proposal.ContainerDir,
	)
	// Each root at a path of its own that is the same on every machine:
	// /work, or /work/<name> for several (config.WorkDir).
	for _, r := range c.Roots {
		args = append(args, "-v", r.Host+":"+r.Container)
	}
	// The user's own arguments, last, so they are checked against all of
	// caboose's, and labelled, so a change to them shows (runargs.go).
	if err := checkRunArgs(c.DockerRunArgs, args, c.Roots); err != nil {
		return a.runArgsError(err)
	}
	args = append(args, "--label", assets.LabelRunArgs+"="+runArgsLabel(c.DockerRunArgs))
	if len(c.DockerRunArgs) > 0 {
		a.Note("creating the container with docker run arguments %s (%s)", describeRunArgs(c.DockerRunArgs), runArgsOrigin(c))
		args = append(args, c.DockerRunArgs...)
	}
	args = append(args, c.Image, "--cc-supervise")
	if err := a.Docker.Run(args...); err != nil {
		return dockerFailed(err)
	}
	return nil
}

// ensureImage makes sure the image exists before a container is created
// from it, building it when it does not. One inspect answers both whether
// the image is there and whether it is stale.
//
// A launcher installed from a release has no image to go with it, and
// "run caboose build first" is one more step between installing and working,
// so the first launch builds -- the same build caboose build runs, ^C handling
// included, with its log on stderr since stdout is claude's. It builds with
// no tty too: a first `caboose claude -p` in a script has no one to ask, and
// failing it would only defer the same build to the next run.
// CABOOSE_NO_AUTO_BUILD restores the error, for anyone who would rather
// build (or pull) deliberately.
//
// mayBuild is false for the commands that only look after a container --
// caboose logs, caboose prune -- where a multi-minute build nobody asked for would
// be the surprise; they fail instead, saying what to run.
//
// An unreachable docker is not a missing image: that dies here, before a
// build that could only fail the same way. So does an image docker cannot
// even look up (an invalid name), which a build would only reject later.
//
// An image that exists but was built on another base than the configured
// one -- CABOOSE_BASE_IMAGE set, unset or changed since, compared as
// classifyImage does -- is rebuilt the same way: the user changed the base
// and ran a command that creates the container, so creating it on the old
// base, with a warning, would be doing the opposite of what they asked.
// Every other staleness (a new ID under the same base name, a launcher that
// embeds a different Dockerfile or layer, another host user) stays a
// warning: nobody asked for that rebuild, and it takes minutes. This is
// only ever reached with no container (createContainer), so no rebuild
// moves an image out from under a running one. CABOOSE_NO_AUTO_BUILD and
// !mayBuild refuse instead, naming both bases: going ahead on the old one
// is the silent mistake this exists to prevent.
//
// It returns the image's labels, read after the build when it built one.
//
// Once it has succeeded, it answers from then on with what it returned,
// asking and warning nothing: Restart runs it before removing the container,
// and createContainer runs it again after.
func (a *App) ensureImage(mayBuild bool) (map[string]string, error) {
	if a.image.ready {
		return a.image.labels, nil
	}
	labels, err := a.ensureImageOnce(mayBuild)
	if err == nil {
		a.image.ready, a.image.labels = true, labels
	}
	return labels, err
}

func (a *App) ensureImageOnce(mayBuild bool) (map[string]string, error) {
	c := a.Cfg
	labels, exists, err := a.Docker.ImageLabels(c.Image)
	if err != nil && !exists {
		return nil, imageInspectFailed(c.Image, err)
	}
	if exists {
		// A stale image is rebuilt before a container is created from it:
		// a launcher that updated itself leaves the one it built before
		// stale, and this is the one moment rebuilding costs no session.
		// Where a launch builds nothing, it is used as it is, and said --
		// unless its base was switched, the user's own change, which a
		// launch does not ignore.
		st := a.imageStatus(labels, exists)
		drifted := st.state == imageStale && !st.keep && !st.switched()
		if drifted && (c.NoAutoBuild != "" || !mayBuild) || !drifted && !st.switched() {
			a.warnIfStale(labels, st)
			return labels, nil
		}
		why := fmt.Sprintf("image '%s' was built on %s; %s", c.Image, st.builtOn, a.baseNow())
		if drifted {
			why = fmt.Sprintf("image '%s' is out of date (built by %s, %s)", c.Image, or(labels[assets.LabelVersion], "unknown"), st.reason)
		}
		switch {
		case c.NoAutoBuild != "":
			return nil, Die("%s —\n"+
				"       run 'caboose build' to rebuild it on that base first\n"+
				"       (CABOOSE_NO_AUTO_BUILD is set, so a launch does not rebuild it)", why)
		case !mayBuild:
			return nil, Die("%s,\n"+
				"       and there is no container yet — run caboose in a project (it rebuilds the image first)\n"+
				"       or 'caboose build'", why)
		}
		a.Note("%s — rebuilding", why)
		a.Note("%s; a few minutes, and CABOOSE_NO_AUTO_BUILD=1 turns this off.", a.buildNote())
	} else {
		switch {
		case c.NoAutoBuild != "":
			return nil, Die("image '%s' not found — run 'caboose build' first\n"+
				"       (CABOOSE_NO_AUTO_BUILD is set, so a launch does not build it)", c.Image)
		case !mayBuild:
			return nil, Die("no container yet, and no image '%s' to create it from — run caboose in a project\n"+
				"       (it builds the image first) or 'caboose build'", c.Image)
		}
		a.Note("no image '%s' yet — %s", c.Image, a.buildNote())
		a.Note("(a few minutes, once; CABOOSE_NO_AUTO_BUILD=1 turns this off).")
		// A name with a registry in it was probably meant to be pulled.
		// Building under it is what caboose build would do too, but say so,
		// since the image that results is this launcher's, not the
		// registry's.
		if strings.Contains(c.Image, "/") {
			a.Note("'%s' is not in the local store; to use the registry's image instead, ^C and 'docker pull' it.", c.Image)
		}
	}
	if err := a.build(nil, a.Stderr); err != nil {
		return nil, err
	}
	a.Note("built image '%s'", c.Image)
	// Best-effort: a failure to read them only costs the platform label,
	// and the architecture fallback still answers.
	labels, _, _ = a.Docker.ImageLabels(c.Image)
	return labels, nil
}

// imagePlatform is the Claude Code platform of the image a container is
// about to be created from: its platform label, which caboose build sets
// from the image check. An image without one was not built by caboose build,
// and has no layer to run the sandbox with either.
func (a *App) imagePlatform(labels map[string]string) (string, error) {
	image := a.Cfg.Image
	p := labels[assets.LabelPlatform]
	switch {
	case p == "":
		return "", Die("image '%s' was not built by caboose build (it has no platform label): run 'caboose build'", image)
	case !datadir.IsPlatform(p):
		return "", Die("image '%s': its platform label says %q, which is not one of %s", image, p, strings.Join(datadir.Platforms, ", "))
	}
	return p, nil
}

// preparePlatformDir picks the image's platform dir and creates its mount
// sources. Only createContainer calls it.
func (a *App) preparePlatformDir(labels map[string]string) (string, error) {
	platform, err := a.imagePlatform(labels)
	if err != nil {
		return "", err
	}
	d := a.Cfg.DataDir
	if err := datadir.EnsurePlatformLayout(d, platform); err != nil {
		return "", Die("preparing %s/%s: %v", d, datadir.PlatformDir(platform), err)
	}
	return platform, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// imageInspectFailed is the error for an ImageLabels that could not answer,
// asking about the engine only when that is what failed.
func imageInspectFailed(image string, err error) error {
	if docker.IsUnreachable(err) {
		return Die("cannot inspect image '%s': is the docker engine running? (%v)", image, err)
	}
	return Die("cannot inspect image '%s': %v", image, err)
}

// warnIfLocalImageStale is warnIfImageStale for a launch that did not have
// to create the container, and so has not inspected the image yet. Any
// docker error keeps it quiet: this is advice, and a launch that is about
// to fail over docker will say so itself.
func (a *App) warnIfLocalImageStale() {
	labels, exists, err := a.Docker.ImageLabels(a.Cfg.Image)
	if err == nil {
		a.warnIfImageStale(labels, exists)
	}
}

func (a *App) lastLogLines() {
	_ = a.Docker.Stream(a.Stderr, a.Stderr, "logs", "--tail", "30", a.Cfg.Container)
}

// pendingInstall is where the container being brought up will install
// Claude Code before it is ready, and for which platform, as noteInstall
// found it; a zero one means it has one already, or it cannot be told.
type pendingInstall struct{ dir, platform string }

// noteInstall records whether the container about to start on local, the
// dir it mounts as ~/.local, has a Claude Code to run there or installs one
// first -- which waitUntilReady tells the user about. It is asked of the
// data dir before the container starts: the install fills it in as it goes.
// A data dir used with another image can have nothing for this platform
// yet, so "first run" means on the platform, not in the data dir.
func (a *App) noteInstall(local, platform string) {
	a.install = pendingInstall{}
	if !datadir.HasInstall(local) {
		a.install = pendingInstall{local, platform}
	}
}

func (a *App) waitUntilReady() error {
	timeout, err := a.Cfg.ReadyTimeoutSeconds()
	if err != nil {
		return Die("%v", err)
	}
	waited, announced := 0, false
	for a.Docker.Quiet("exec", a.Cfg.Container, "test", "-f", ReadyMarker) != nil {
		if a.state() != "running" {
			a.Note("container exited before becoming ready; last log lines:")
			a.lastLogLines()
			return Die("startup failed")
		}
		if waited >= timeout {
			a.Note("not ready after %ds; last log lines:", timeout)
			a.lastLogLines()
			return Die("giving up (raise CABOOSE_READY_TIMEOUT if the install is just slow)")
		}
		// An install is said at once; anything else only once it is slow.
		switch in := a.install; {
		case announced:
		case in.platform != "":
			a.Note("first run on %s — installing Claude Code into %s, this takes a minute", in.platform, in.dir)
			announced = true
		case waited >= 3:
			a.Note("waiting for the container to be ready")
			announced = true
		}
		time.Sleep(time.Second)
		waited++
	}
	return nil
}

// ensureRunning brings the container up, creating it when absent; mayBuild
// is ensureImage's, for the image that takes.
func (a *App) ensureRunning(mayBuild bool) error {
	if err := a.Cfg.CheckImages(); err != nil {
		return Die("%v", err)
	}
	if err := a.prepareDataDir(); err != nil {
		return err
	}
	a.noteSetup()
	if err := a.syncSandboxInstructions(); err != nil {
		return err
	}
	a.exportProposals()
	if sb, err := a.sandboxConfig(); err == nil {
		a.noteSandboxConfig(sb)
	}
	a.notePendingProposals()
	created := false
	switch a.state() {
	case "running":
		if err := a.checkExisting(); err != nil {
			return err
		}
		a.warnIfRootsDrifted()
		a.warnIfKeepDrifted()
		a.warnIfRunArgsDrifted()
	case "absent":
		if err := a.createContainer(mayBuild); err != nil {
			return err
		}
		created = true
	default:
		// The dir it has mounted may have been emptied since (deleting a
		// platform dir is how to force a reinstall).
		a.install = pendingInstall{}
		if dir, platform, ok := a.mountedLocal(); ok {
			a.noteInstall(dir, platform)
		}
		if err := a.checkExisting(); err != nil {
			return err
		}
		if err := a.Docker.Run("start", a.Cfg.Container); err != nil {
			return dockerFailed(err)
		}
	}
	if err := a.waitUntilReady(); err != nil {
		return err
	}
	a.openAgentSocket()
	// The mount is fixed at creation, so that is when to say it is useless.
	if created {
		a.warnIfAgentUnusable()
	}
	return nil
}

// confirmSessionLoss guards the two commands that destroy sessions.
//
// The entire point of the long-lived container is that sessions -- and the
// background agents inside them -- outlive any one terminal. So the two
// commands that destroy them say what they are about to kill and ask first.
// FORCE=1 skips the prompt, which is the same escape hatch tests/run.sh uses
// and what lets the suite drive caboose restart unattended. With no tty they
// refuse outright rather than assume.
func (a *App) confirmSessionLoss(action string) error {
	if a.state() != "running" || a.lossConfirmed {
		return nil
	}
	sessions, _ := a.Docker.Output("exec", a.Cfg.Container, "tmux", "list-sessions", "-F", "#{session_name}")
	if sessions == "" {
		return nil
	}
	a.Note("%s will kill these live session(s):", action)
	fmt.Fprint(a.Stderr, indent(sessions+"\n", "  "))
	if a.getenv("FORCE") != "" {
		a.Note("FORCE=1 set, continuing.")
		return nil
	}
	refuse := Die("refusing to %s non-interactively with live sessions (set FORCE=1 to override)", action)
	if !tty.IsTerminal(os.Stdin.Fd()) {
		return refuse
	}
	t, err := os.Open("/dev/tty")
	if err != nil {
		return refuse
	}
	defer t.Close()
	fmt.Fprint(a.Stderr, "caboose: continue? [y/N] ")
	if confirmed(t) {
		return nil
	}
	return Die("aborted; nothing was changed")
}

// confirmed reads one reply line from r and reports whether it is a yes. A
// failed read -- EOF included, so "y" then Ctrl-D with no newline -- counts
// as an empty reply.
func confirmed(r io.Reader) bool {
	reply, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		reply = ""
	}
	switch strings.ToLower(strings.Trim(reply, " \t\n")) {
	case "y", "yes":
		return true
	}
	return false
}

func (a *App) removeContainer() error {
	if a.state() != "absent" {
		if err := a.Docker.Run("rm", "-f", a.Cfg.Container); err != nil {
			return dockerFailed(err)
		}
		a.Note("container removed")
	}
	return nil
}

// importHostTerminfo compiles the host's description of term into the
// container.
//
// Ghostty, Kitty and WezTerm all ship their own terminfo and none of them
// are in any distro package, so the fallback to xterm-256color would be
// permanent. If the host can describe the terminal, compile that description
// into the container instead: ncurses searches ~/.terminfo automatically, so
// one import makes the real entry available to both tmux and Claude Code.
//
// ~/.terminfo is deliberately NOT bind-mounted. It is cheap to regenerate
// and belongs to the container, so it simply re-imports after a
// caboose restart.
//
// Best-effort throughout: an old host infocmp (macOS ships ncurses 5.7) may
// not support -x, so plain infocmp is tried too, and any failure just leaves
// the caller on the generic fallback, xterm-256color.
// The final infocmp is the verification, so a tic that "succeeds" without
// producing a usable entry still reports failure.
func (a *App) importHostTerminfo(term string) bool {
	infocmp, err := exec.LookPath("infocmp")
	if err != nil {
		return false
	}
	desc, err := exec.Command(infocmp, "-x", term).Output()
	if err != nil {
		if desc, err = exec.Command(infocmp, term).Output(); err != nil {
			return false
		}
	}
	if a.Docker.Pipe(desc, "exec", "-i", a.Cfg.Container, "sh", "-c", `tic -x -o "$HOME/.terminfo" -`) != nil {
		return false
	}
	return a.Docker.Quiet("exec", a.Cfg.Container, "infocmp", term) == nil
}

// execEnvArgs builds the `-e VAR=value` pairs for a docker exec.
//
// `docker exec` forwards none of the host's session identity: -t hardcodes
// TERM=xterm (8 colors), drops COLORTERM, and never passes a timezone.
//
// Only pass a TERM the container actually has a terminfo entry for, since an
// unknown TERM degrades worse than a generic one. ncurses-term in the image
// covers most; the rest are imported from the host (importHostTerminfo).
func (a *App) execEnvArgs() []string {
	term := a.getenv("TERM")
	if term == "" {
		term = "xterm-256color"
	}
	if a.Docker.Quiet("exec", a.Cfg.Container, "infocmp", term) != nil {
		if a.importHostTerminfo(term) {
			a.Note("imported the host's '%s' terminfo into the container", term)
		} else {
			term = "xterm-256color"
		}
	}
	args := []string{"-e", "TERM=" + term}
	// Claude Code uses COLORTERM to decide truecolor; the terminfo entry
	// alone is not enough.
	if ct := a.getenv("COLORTERM"); ct != "" {
		args = append(args, "-e", "COLORTERM="+ct)
	}
	// A zone name the container's tzdata does not have degrades silently
	// back to UTC -- precisely the bug this exists to fix -- so check, and
	// say so rather than pretending it worked.
	if tz := a.hostTimezone(); tz != "" {
		if a.Docker.Quiet("exec", a.Cfg.Container, "test", "-f", "/usr/share/zoneinfo/"+tz) == nil {
			args = append(args, "-e", "TZ="+tz)
		} else {
			a.Note("host timezone '%s' is not in the container's tzdata; staying on UTC", tz)
		}
	}
	return args
}
