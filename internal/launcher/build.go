package launcher

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/imagecheck"
	"github.com/bfreis/caboose/internal/version"
)

// Build builds the image: a base, checked, with the caboose layer on top --
// the replacement for build.sh. Claude Code is not baked in (it lives in the
// bind-mounted data dir), so a rebuild does not reset its version; it only
// refreshes the OS packages, the language toolchains, the CLIs and the
// entrypoint.
//
// The base is built here and tagged config.DefaultBaseTag(CABOOSE_IMAGE),
// from the environment's image/ dir when it has one, else from the
// embedded Dockerfile; or it is CABOOSE_BASE_IMAGE, which is the user's and
// never built: only pulled, when it is not local or --pull asks.
// Either way the image check runs on it next, and a base that fails it
// stops the build with the list of what is missing, before the layer. So the
// default image goes through the path a user's does, and there is one path
// to test.
//
// Each context is a temp dir holding only embedded files, never a checkout,
// so nothing unlisted can reach the daemon, by construction rather than by
// a .dockerignore someone has to keep up to date. The layer is built for the host UID/GID, so
// files the container writes into bind mounts belong to the host user.
//
// Extra args (--no-cache, --progress=plain, --platform ...) go to every
// `docker build` this runs, except --pull: the layer is FROM a base that is
// local by then, and may exist nowhere else, so pulling it would fail. It
// goes to the base's build on the default base, and on a user's means
// `docker pull` it first.
//
// A running container keeps using the image it was created from; pick up a
// rebuild with `caboose restart`.
//
// The last build's stdout stays stdout here, as it would running `docker
// build` directly, so `caboose build -q` still prints just the image
// ID -- the final image's. Everything before it (the base's build, a pull,
// the notes) is on stderr.
func (a *App) Build(extra []string) error {
	return a.build(extra, a.Stdout)
}

// build is Build with the layer build's stdout sent to stdout. The build a
// launch starts by itself passes stderr: its stdout belongs to claude, whose
// output (`caboose claude -p ...`) may be piped somewhere that a build log would
// corrupt.
func (a *App) build(extra []string, stdout io.Writer) error {
	if err := a.Cfg.CheckImages(); err != nil {
		return Die("%v", err)
	}
	if tag := tagArg(extra); tag != "" {
		return Die("caboose build names its images itself, so it takes no %s: set CABOOSE_IMAGE to build under another name", tag)
	}
	// docker gets ^C along with us, from the terminal's process group. Hold
	// on to it here instead of dying, so the context dirs are still removed.
	// (Notify, not Ignore: an ignored signal would stay ignored in docker.)
	// Holding it means noticing it, too: a step that ^C cut short can end
	// looking like a success, or like a different failure (a check with no
	// answer), so after each one a signal that arrived meanwhile ends the
	// build as interrupted, whatever the step returned.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	interrupted := func(err error) error {
		select {
		case s := <-sigs:
			return interruptedBy(s)
		default:
			return err
		}
	}

	base, byo := a.Cfg.Base()
	dirHash := ""
	if a.Cfg.ImageDir != "" {
		// Before the build, so that the label never claims an edit made
		// while it ran: such an edit reads as stale afterwards.
		var err error
		if dirHash, err = assets.DirHash(a.Cfg.ImageDir); err != nil {
			return Die("reading %s: %v", a.Cfg.ImageDir, err)
		}
	}
	pull, layerExtra := splitPull(extra)
	platform := platformArg(extra)
	if byo {
		if err := interrupted(a.fetchBase(base, pull, platform)); err != nil {
			return err
		}
	} else if err := interrupted(a.buildBase(base, extra)); err != nil {
		return err
	}

	// The check and the labels are about this ID. The layer's FROM names the
	// base by its tag all the same: BuildKit resolves a FROM by name only
	// (an image ID reads as a repository called sha256), so a retag between
	// here and the layer's build could slip another image in. Nothing here
	// can stop that for a user's tag; it is caught afterwards, since the
	// base-id label then names an image the tag no longer points at, and
	// the image reads as stale until rebuilt.
	baseID := a.Docker.ImageID(base)
	if baseID == "" {
		return Die("cannot read the ID of base image '%s'", base)
	}
	a.Note("checking base image '%s' (%s) against caboose's requirements", base, shortID(baseID))
	// With --platform, the check runs on that platform's variant, the one
	// the layer is built on, not the host's.
	rep, err := imagecheck.Run(a.Docker, baseID, platform, os.Getuid(), os.Getgid())
	if err := interrupted(nil); err != nil {
		return err
	}
	if err != nil {
		if docker.IsUnreachable(err) {
			return Die("cannot check base image '%s': is the docker engine running? (%v)", base, err)
		}
		return Die("cannot check base image '%s': %v", base, err)
	}
	for _, n := range rep.Notes() {
		a.Note("%s", n)
	}
	if problems := rep.Problems(); len(problems) > 0 {
		a.Note("base image '%s' does not meet caboose's requirements:", base)
		for _, p := range problems {
			a.Note("  %s", p)
		}
		a.Note("'caboose check-image %s' prints the full checklist.", base)
		return Die("not building the caboose layer on '%s'", base)
	}

	return interrupted(a.buildLayer(base, baseID, byo, dirHash, rep.Platform, layerExtra, stdout))
}

// interruptedBy is the exit for a build a signal stopped: 128 plus its
// number, as a shell reports a command killed by it.
func interruptedBy(s os.Signal) error {
	code := 130
	if n, ok := s.(syscall.Signal); ok {
		code = 128 + int(n)
	}
	return &ExitError{Code: code, Msg: "interrupted"}
}

// tagArg is the -t or --tag among a build's extra args, "" when there is
// none: the images' names are the launcher's to give.
func tagArg(extra []string) string {
	for _, arg := range extra {
		switch {
		case arg == "--tag" || strings.HasPrefix(arg, "--tag="):
			return "--tag"
		case strings.HasPrefix(arg, "-t"):
			return "-t"
		}
	}
	return ""
}

// platformArg is the value of a build's --platform (--platform X or
// --platform=X; the last one wins, as for docker), "" when there is none.
func platformArg(extra []string) string {
	p := ""
	for i, arg := range extra {
		switch {
		case arg == "--platform" && i+1 < len(extra):
			p = extra[i+1]
		case strings.HasPrefix(arg, "--platform="):
			p = strings.TrimPrefix(arg, "--platform=")
		}
	}
	return p
}

// splitPull takes --pull out of a build's extra args: whether one asked to
// pull (a bare --pull, or --pull=true), and the args without any --pull.
func splitPull(extra []string) (pull bool, rest []string) {
	for _, arg := range extra {
		switch {
		case arg == "--pull":
			pull = true
		case strings.HasPrefix(arg, "--pull="):
			v := strings.TrimPrefix(arg, "--pull=")
			pull = v != "false" && v != "0"
		default:
			rest = append(rest, arg)
		}
	}
	return pull, rest
}

// buildBase builds the base as tag, with all the extra args: from the
// environment's image/ dir, which is its context as it stands, or else
// from the embedded Dockerfile, written to a temp dir. Its stdout goes to
// stderr, so that only the final image's build writes to stdout. It takes
// no build args: nothing in the base depends on the host.
func (a *App) buildBase(tag string, extra []string) error {
	ctx, hash := a.Cfg.ImageDir, ""
	if ctx != "" {
		a.Note("building the base image '%s' from %s", tag, filepath.Join(ctx, "Dockerfile"))
	} else {
		dir, err := os.MkdirTemp("", "caboose-base-")
		if err != nil {
			return Die("%v", err)
		}
		defer os.RemoveAll(dir)
		if err := assets.WriteBaseContext(dir); err != nil {
			return Die("%v", err)
		}
		ctx, hash = dir, assets.BaseHash()
		a.Note("building the base image '%s' from the Dockerfile embedded in this launcher", tag)
	}
	argv := []string{"build", "-t", tag,
		"--label", assets.LabelVersion + "=" + version.Get().Version,
		"--label", assets.LabelBaseHash + "=" + hash}
	argv = append(append(argv, extra...), ctx)
	if err := a.Docker.Stream(a.Stderr, a.Stderr, argv...); err != nil {
		return dockerFailed(err)
	}
	return nil
}

// fetchBase makes sure a user's base image is local: pulled when it is not,
// or when pull asks for the registry's latest -- for platform, when a build
// names one. docker's progress goes to stderr, as caboose check-image's pull
// does.
func (a *App) fetchBase(ref string, pull bool, platform string) error {
	_, exists, err := a.Docker.ImageLabels(ref)
	if err != nil && !exists {
		return imageInspectFailed(ref, err)
	}
	switch {
	case !exists:
		a.Note("base image '%s' (CABOOSE_BASE_IMAGE) is not in the local store; pulling it (docker's output follows)", ref)
	case pull:
		a.Note("pulling base image '%s' (CABOOSE_BASE_IMAGE), as --pull asks", ref)
	default:
		return nil
	}
	argv := []string{"pull"}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	if err := a.Docker.Stream(a.Stderr, a.Stderr, append(argv, ref)...); err != nil {
		return Die("cannot pull base image '%s' (%v)", ref, err)
	}
	return nil
}

// buildLayer builds layer.Dockerfile FROM base as CABOOSE_IMAGE, labelled
// with what it was built from: the launcher's version, the hashes of the
// embedded contexts it used, the kind of base and its name and ID, the
// platform the check found, which picks the data dir's dot_local/<platform>,
// and the host IDs the agent user has. dirHash is the environment's image/
// dir's, when the base was built from it.
func (a *App) buildLayer(base, baseID string, byo bool, dirHash, platform string, extra []string, stdout io.Writer) error {
	dir, err := os.MkdirTemp("", "caboose-layer-")
	if err != nil {
		return Die("%v", err)
	}
	defer os.RemoveAll(dir)
	if err := assets.WriteLayerContext(dir); err != nil {
		return Die("%v", err)
	}
	a.Note("building the caboose layer on '%s' as '%s'", base, a.Cfg.Image)
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	kind, baseHash := assets.BaseKindBYO, ""
	switch {
	case dirHash != "":
		kind, baseHash = assets.BaseKindEnv, dirHash
	case !byo:
		kind, baseHash = assets.BaseKindDefault, assets.BaseHash()
	}
	values := map[string]string{
		assets.LabelVersion:   version.Get().Version,
		assets.LabelLayerHash: assets.LayerHash(),
		assets.LabelBaseKind:  kind,
		assets.LabelBaseHash:  baseHash,
		assets.LabelBaseName:  base,
		assets.LabelBaseID:    baseID,
		assets.LabelPlatform:  platform,
		assets.LabelUID:       uid,
		assets.LabelGID:       gid,
		assets.LabelCompat:    strconv.Itoa(assets.Compat),
	}
	argv := []string{"build", "-t", a.Cfg.Image, "-f", dir + "/" + assets.LayerDockerfile,
		"--build-arg", "BASE=" + base,
		"--build-arg", "CABOOSE_UID=" + uid,
		"--build-arg", "CABOOSE_GID=" + gid}
	// Every label, even an empty one: see assets.LayerLabels.
	for _, l := range assets.LayerLabels {
		argv = append(argv, "--label", l+"="+values[l])
	}
	argv = append(append(argv, extra...), dir)
	if err := a.Docker.Stream(stdout, a.Stderr, argv...); err != nil {
		return dockerFailed(err)
	}
	return nil
}

// shortID is an image ID as docker's CLI shows one: 12 hex digits, no
// algorithm.
func shortID(id string) string {
	return short(strings.TrimPrefix(id, "sha256:"))
}

func (a *App) buildNote() string {
	if base, byo := a.Cfg.Base(); byo {
		return fmt.Sprintf("building the caboose layer on '%s' (CABOOSE_BASE_IMAGE)", base)
	}
	if a.Cfg.ImageDir != "" {
		return "building it from " + filepath.Join(a.Cfg.ImageDir, "Dockerfile")
	}
	return "building it from the Dockerfile embedded in this launcher"
}
