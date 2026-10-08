package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// apkoTools is what an apko build takes from internal/apkobuild: resolving
// a package set into a lock, and building a lock into an image tarball. A
// field of App's (apko) so that tests can stand in for the network.
type apkoTools struct {
	resolve func(ctx context.Context, spec apkobuild.Spec, o apkobuild.Options) (*apkobuild.Lock, error)
	build   func(ctx context.Context, l *apkobuild.Lock, o apkobuild.Options, tag string, w io.Writer) (string, error)
	// check resolves a spec against the indexes alone, for the link's
	// check of a proposal (proposalChecker).
	check func(ctx context.Context, spec apkobuild.Spec, o apkobuild.Options) (apkobuild.CheckResult, error)
}

// apkoReal is apkobuild itself.
var apkoReal = apkoTools{resolve: apkobuild.Resolve, build: apkobuild.Build, check: apkobuild.Check}

func (a *App) apkoTools() apkoTools {
	if a.apko != nil {
		return *a.apko
	}
	return apkoReal
}

// buildApko builds an apko profile's base as tag, into the docker engine,
// and returns the hash of the lock it was built from, which labels the
// layer: apkoBase, for the engine's architecture, with docker load as
// where the image goes.
func (a *App) buildApko(tag string, pull bool) (string, error) {
	arch, err := a.engineArch()
	if err != nil {
		return "", err
	}
	return a.apkoBase(tag, pull, arch, "the engine", func(r io.Reader, _ string) error {
		if err := a.Docker.Load(r, a.Stderr, a.Stderr); err != nil {
			return dockerFailed(err)
		}
		return nil
	})
}

// apkoBase builds an apko profile's base as tag, for arch (apk's name for
// it, which machine has: "the engine"), as an image tarball that load
// reads, and returns the hash of the lock it was built from, which labels
// the layer. load is also given that hash; an error it returns is returned
// as it is, unless the build failed first.
//
// The lock, next to config.toml (config.LockPath), is resolved again first
// when there is none, when it cannot be read, when it was resolved from
// other packages or for another architecture, and when pull says to fetch
// the newest packages; otherwise it is built exactly as it is, so a
// rebuild gives the same base. The lock is written only once load
// succeeded, and under caboose apply (App.deferLock) not at all: apply
// writes it once config.toml is written. apko's own logging is quiet but for its warnings: the
// launcher says what happens in a line or two.
func (a *App) apkoBase(tag string, pull bool, arch, machine string, load func(r io.Reader, lockHash string) error) (string, error) {
	c, p := a.Cfg, a.Cfg.ImageProfile
	lockPath := c.LockPath()
	if lockPath == "" {
		return "", Die("an apko image profile keeps its lock in the environment's directory, and there is none (CABOOSE_HOME is unset)")
	}
	want, err := p.Spec().List()
	if err != nil {
		return "", Die("packages of %s: %v", p, err)
	}
	opts := apkobuild.Options{Arch: arch, CacheDir: a.apkCacheDir()}
	// Held from the resolve to the build's end, so that a prune between
	// them cannot take what the resolve found cached.
	unlockCache, err := apkobuild.LockCache(opts.CacheDir)
	if err != nil {
		return "", Die("%v", err)
	}
	defer unlockCache()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx = clog.WithLogger(ctx, clog.New(&noteHandler{a: a, prefix: "apko: "}))
	tools := a.apkoTools()

	lock, err := apkobuild.ReadLock(lockPath)
	why, write := "", false
	switch {
	case pull:
		why = "as --pull asks"
	case errors.Is(err, fs.ErrNotExist):
		why = "it has no lock yet"
	case err != nil:
		why = fmt.Sprintf("its lock cannot be used: %v", err)
	case !slices.Equal(lock.Input(), want) && !lock.Spec().Equal(p.Spec()):
		why = "its packages changed"
	case !slices.Equal(lock.Input(), want):
		why = "caboose's packages changed with this launcher"
	case lock.Arch() != arch:
		why = fmt.Sprintf("its lock is for %s, and %s is %s", lock.Arch(), machine, arch)
	case !lock.Spec().Equal(p.Spec()):
		// Another spec for the same packages: the lock stands, recording
		// the spec it now serves.
		if lock, err = lock.WithSpec(p.Spec()); err != nil {
			return "", Die("packages of %s: %v", p, err)
		}
		write = true
	}
	if why != "" {
		a.Note("resolving the packages of %s (%s)", p, why)
		if lock, err = tools.resolve(ctx, p.Spec(), opts); err != nil {
			return "", Die("resolving the packages of %s: %v", p, err)
		}
		a.Note("resolved %d packages for %s", len(lock.Packages()), p)
		write = true
	}

	a.Note("building the base image '%s' from %d packages (%s)", tag, len(lock.Packages()), p)
	pr, pw := io.Pipe()
	built := make(chan error, 1)
	buildCtx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()
	go func() {
		_, err := tools.build(buildCtx, lock, opts, tag, pw)
		pw.CloseWithError(err)
		built <- err
	}()
	loadErr := load(&buildSource{PipeReader: pr, cancel: cancelBuild}, lock.Hash())
	// A load that ended early leaves the build blocked on the pipe, or still
	// fetching packages it will never write: this stops it, with an error
	// that says the load came first. After a load that read it all, the
	// build has nothing left to do.
	pr.CloseWithError(errLoadEnded)
	cancelBuild()
	buildErr := <-built
	switch {
	case loadErr != nil && (buildErr == nil || errors.Is(buildErr, errLoadEnded) || errors.Is(buildErr, context.Canceled) && ctx.Err() == nil):
		return "", loadErr
	case buildErr != nil:
		// A build that failed cut the load short too: its error is the cause.
		return "", Die("building the base image '%s': %v", tag, buildErr)
	case loadErr != nil:
		return "", loadErr
	}
	// The lock is the image's record, so it is written only once the image
	// it built is in: a failed build leaves the last good one, and the image
	// it describes.
	// Under caboose apply (deferLock), config.toml is not yet what this
	// lock is of: apply writes the lock after it.
	writeLock := func() error {
		if err := lock.Write(lockPath); err != nil {
			return Die("%v", err)
		}
		a.Note("lock written to %s", a.short(lockPath))
		return nil
	}
	switch {
	case !write:
	case a.deferLock:
		a.pendingLock = writeLock
	default:
		if err := writeLock(); err != nil {
			return "", err
		}
	}
	return lock.Hash(), nil
}

// errLoadEnded is what an apko build writing into a docker load that has
// already ended gets.
var errLoadEnded = errors.New("docker load ended before the image was written")

// buildSource is the tarball an apko build writes, as its load reads it.
// A load that is done with it, before its end or not, may close it (the
// builder guest's does, once its docker load has ended): that stops the
// build at once, where it may otherwise go on fetching packages until its
// next write finds the pipe closed.
type buildSource struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (s *buildSource) Close() error {
	s.cancel()
	return s.CloseWithError(errLoadEnded)
}

// engineArch is the docker engine's architecture as apk names it.
func (a *App) engineArch() (string, error) {
	arch, err := a.Docker.Architecture()
	if err != nil {
		if docker.IsUnreachable(err) {
			return "", Die("cannot ask the docker engine its architecture: is it running? (%v)", err)
		}
		return "", Die("cannot ask the docker engine its architecture (docker info: %v); is it running?", err)
	}
	switch arch {
	case "x86_64", "aarch64":
		return arch, nil
	}
	if apk, err := apkobuild.ArchFor(arch); err == nil {
		return apk, nil
	}
	return "", Die("the docker engine is %s, and an apko image is built for x86_64 and aarch64 only: set image to a dockerfile or ref profile ('%s')",
		arch, SetupCommand(a.Cfg.Env, "image"))
}

// apkCacheDir is where downloaded packages are kept between builds, for
// every environment: CABOOSE_HOME/cache/apk, made when missing. "" (no
// cache) when there is no CABOOSE_HOME, or it cannot be made.
func (a *App) apkCacheDir() string {
	if a.Cfg.CabooseHome == "" {
		return ""
	}
	dir := filepath.Join(a.Cfg.CabooseHome, "cache", "apk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.Note("not keeping downloaded packages: %v", err)
		return ""
	}
	return dir
}

// packagesPhrase says what an apko profile is made of, for a line about
// the image: "caboose's packages + 2 of yours".
func (a *App) packagesPhrase() string {
	p := a.Cfg.ImageProfile
	what := "caboose's packages"
	if !p.Defaults {
		what = "the packages the sandbox requires"
	}
	if n := len(p.Packages); n > 0 {
		what += fmt.Sprintf(" + %d of yours", n)
	}
	return what
}

// noteHandler is a slog handler that says warnings and errors as the
// launcher's notes, with prefix, and drops the rest: apko's logging, which
// would otherwise drown the build's own lines.
type noteHandler struct {
	a      *App
	prefix string
}

func (h *noteHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h *noteHandler) Handle(_ context.Context, r slog.Record) error {
	h.a.Note("%s%s", h.prefix, r.Message)
	return nil
}

func (h *noteHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *noteHandler) WithGroup(string) slog.Handler      { return h }

// apkCacheRecent is how long a cached package is kept whatever the locks
// say: a build fetching packages writes its lock only once its image is in.
const apkCacheRecent = time.Hour

// pruneApkCache removes from the package cache what no environment's lock
// names (apkobuild.PruneCache), for caboose prune, and says what it freed
// in a line. A lock it cannot read could name anything, so then it removes
// nothing and says why; an error is returned after that line.
func (a *App) pruneApkCache() error {
	if a.Cfg.CabooseHome == "" {
		return nil
	}
	dir := filepath.Join(a.Cfg.CabooseHome, "cache", "apk")
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	envs, err := a.envNames()
	if err != nil {
		return Die("not pruning the package cache %s: listing the environments: %v", a.short(dir), err)
	}
	var locks []*apkobuild.Lock
	for _, env := range envs {
		paths, err := filepath.Glob(filepath.Join(config.EnvDirFor(a.Cfg.CabooseHome, env), "apko-*.lock.json"))
		if err != nil {
			return Die("not pruning the package cache %s: %v", a.short(dir), err)
		}
		for _, p := range paths {
			l, err := apkobuild.ReadLock(p)
			if err != nil {
				a.Note("not pruning the package cache %s: %v", a.short(dir), err)
				a.Note("  a lock that cannot be read could name any package; fix or delete it, or run 'caboose -e %s build --pull'", env)
				return nil
			}
			locks = append(locks, l)
		}
	}
	r, err := apkobuild.PruneCache(dir, locks, time.Now().Add(-apkCacheRecent))
	switch {
	case errors.Is(err, apkobuild.ErrCacheInUse):
		a.Note("package cache %s: a build is using the package cache; not pruned", a.short(dir))
		return nil
	case r == apkobuild.CachePrune{} && err == nil:
		a.Note("package cache %s: nothing to free", a.short(dir))
	default:
		a.Note("package cache %s: freed %s (%d %s no lock names, %d %s)", a.short(dir), sizeString(r.Bytes),
			r.Packages, plural(r.Packages, "package", "packages"), r.Indexes, plural(r.Indexes, "old index", "old indexes"))
	}
	if err != nil {
		return Die("pruning the package cache %s: %v", a.short(dir), err)
	}
	return nil
}
