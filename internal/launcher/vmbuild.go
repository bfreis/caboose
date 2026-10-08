package launcher

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/hvsock"
	"github.com/bfreis/caboose/internal/linkdebug"
	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/builder"
)

// buildVM is caboose build under vm: the same base, check and layer as
// with docker, run in the builder guest (internal/vm/builder), and the
// layer written as a root disk into the image store, recorded under the
// image's name with the labels and config docker in the builder gave it.
// The empty disk sandbox scratch disks are cloned from is made with the
// first build. An apko base is built on this Mac, as with docker
// (apkoBase), for the guests' architecture, and its tarball streamed into
// docker load in the builder; its lock is written once the image is
// recorded.
func (a *App) buildVM(extra []string) error {
	c := a.Cfg
	if p := platformArg(extra); p != "" {
		return Die("under isolation vm the image is built for this Mac's architecture only, so caboose build takes no --platform")
	}
	store := a.vmImages()
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return Die("%v", err)
	}

	pull, layerExtra := splitPull(extra)
	p, base := c.ImageProfile, c.BaseRef()
	req := builder.Request{
		UID: os.Getuid(), GID: os.Getgid(),
		LayerFile: assets.LayerDockerfile, Image: c.Image,
		LayerArgs: layerExtra, Pull: pull,
	}
	var err error
	if req.Probe, err = assets.ProbeScript(); err != nil {
		return Die("%v", err)
	}
	baseHash := ""
	tmp, err := os.MkdirTemp("", "caboose-vm-build-")
	if err != nil {
		return Die("%v", err)
	}
	defer os.RemoveAll(tmp)
	switch p.Kind {
	case config.ImageKindRef:
		req.BaseRef = base
		a.Note("pulling base image '%s' (%s) in the builder when it lacks it", base, p)
	case config.ImageKindDockerfile:
		if err := a.checkDockerfileDir(); err != nil {
			return err
		}
		// Before the build, so that the label never claims an edit made
		// while it ran.
		if baseHash, err = assets.DirHash(p.Dir); err != nil {
			return Die("reading %s: %v", p.Dir, err)
		}
		req.BaseContext, req.BaseTag, req.BaseArgs = p.Dir, base, extra
		a.Note("building the base image '%s' from %s (%s), in the builder", base, filepath.Join(p.Dir, "Dockerfile"), p)
	case config.ImageKindApko:
		// Built on this Mac, its packages in CABOOSE_HOME's cache, and
		// streamed into the builder's docker load (below).
		req.BaseTag = base
	default:
		return Die("image profile %s is of no kind this caboose builds", p)
	}
	req.LayerContext = filepath.Join(tmp, "layer")
	if err := writeContext(req.LayerContext, assets.WriteLayerContext); err != nil {
		return Die("%v", err)
	}

	out := filepath.Join(store.dir, ".root-new.img")
	template := ""
	if !isFile(store.template()) {
		template = filepath.Join(store.dir, ".empty-new.img")
	}
	boot := a.buildGuest
	if boot == nil {
		boot = func(out, template string) (vmBuildGuest, error) { return a.startBuilder(out, template) }
	}
	g, err := boot(out, template)
	if err != nil {
		return err
	}
	defer g.Stop()
	if p.Kind != config.ImageKindApko {
		return a.finishVMBuild(g, req, out, template, baseHash)
	}
	arch, err := a.vmArch()
	if err != nil {
		return err
	}
	// The whole build is apkoBase's load, so the lock is written only once
	// the image it built is in the image store.
	_, err = a.apkoBase(base, pull, arch, "the builder guest", func(r io.Reader, lockHash string) error {
		req.BaseTarball = r
		return a.finishVMBuild(g, req, out, template, lockHash)
	})
	return err
}

// vmBuildGuest is what caboose build asks of the builder guest
// (builder.Guest).
type vmBuildGuest interface {
	Build(req builder.Request) (builder.Result, error)
	Template() error
	Stop() error
}

// vmArch is the guests' architecture, the Mac's, as apk names it: what an
// apko base is built for under vm.
func (a *App) vmArch() (string, error) {
	f, err := a.findVMFiles()
	if err != nil {
		return "", Die("%v", err)
	}
	arch, err := apkobuild.ArchFor(f.Arch)
	if err != nil {
		return "", Die("%v", err)
	}
	return arch, nil
}

// finishVMBuild runs req in the builder guest g, and records what it made
// in the image store: the image's root disk, written onto out, and the
// empty disk scratch disks are cloned from, onto template when not "".
// baseHash labels the layer, as layerLabels takes it.
func (a *App) finishVMBuild(g vmBuildGuest, req builder.Request, out, template, baseHash string) error {
	c, p, store := a.Cfg, a.Cfg.ImageProfile, a.vmImages()
	r, err := g.Build(req)
	var ce *builder.CheckError
	switch {
	case errors.As(err, &ce):
		a.Note("base image '%s' does not meet caboose's requirements:", ce.Report.Image)
		for _, p := range ce.Report.Problems() {
			a.Note("  %s", p)
		}
		return Die("not building the caboose layer on '%s'", ce.Report.Image)
	case err != nil:
		return Die("%v", err)
	}
	// Not a requirement: a sandbox without docker inside is a sandbox.
	if missing := r.Report.EngineMissing(); len(missing) > 0 {
		lacks := "no dockerd"
		if r.Report.Engine["dockerd"] != "" {
			lacks = "no " + strings.Join(missing, ", ")
		}
		a.Note("base image '%s' has %s: `docker` won't work inside the sandbox ('caboose check-image' says what to add)",
			r.Base, lacks)
	}
	for _, n := range r.Report.Notes() {
		a.Note("%s", n)
	}
	if template != "" {
		if err := g.Template(); err != nil {
			return Die("the empty disk for scratch disks: %v", err)
		}
	}
	if err := g.Stop(); err != nil {
		return Die("stopping the builder: %v", err)
	}
	if template != "" {
		if err := os.Rename(template, store.template()); err != nil {
			return Die("%v", err)
		}
	}
	rec := vmImage{Name: c.Image, ID: r.ImageID, Config: r.Config, Disk: diskName(r.ImageID),
		Labels: layerLabels(r.Base, r.BaseID, p.Kind, baseHash, r.Report.Platform)}
	if err := os.Rename(out, store.disk(rec)); err != nil {
		return Die("%v", err)
	}
	if err := store.put(rec); err != nil {
		return Die("recording image '%s': %v", c.Image, err)
	}
	if err := store.put(vmImage{Name: r.Base, ID: r.BaseID}); err != nil {
		return Die("recording base image '%s': %v", r.Base, err)
	}
	store.prune(a.vmRoot())
	a.Note("wrote image '%s' as %s (%s)", c.Image, rec.Disk, shortID(r.ImageID))
	return nil
}

// startBuilder boots the builder guest, with out and template as
// builder.Builder.Start takes them ("" for none: caboose check-image
// writes no disk).
func (a *App) startBuilder(out, template string) (*builder.Guest, error) {
	if err := a.checkVM(); err != nil {
		return nil, err
	}
	f, err := a.findVMFiles()
	if err != nil {
		return nil, Die("%v", err)
	}
	initramfs, err := a.initramfs(f.Arch)
	if err != nil {
		return nil, Die("%v", err)
	}
	cpus, mem, err := vmSize(a.Cfg)
	if err != nil {
		return nil, Die("%v", err)
	}
	self, err := a.executable()
	if err != nil {
		return nil, Die("cannot find this caboose, to run commands in the builder: %v", err)
	}
	lc, err := settingsOf(a.Cfg).check()
	if err != nil {
		return nil, Die("%v", err)
	}
	b := &builder.Builder{
		Dir: a.vmDir(builderName), Kernel: f.Kernel, Initramfs: initramfs, Image: f.Builder,
		CPUs: cpus, MemoryMiB: mem, Self: self, Stderr: a.Stderr,
		StartVMM: func(d vm.Dir) error { return a.startVMM(f.VMM, d) },
	}
	if lc.egress != nil {
		// The build's downloads made from this machine, as the sandbox's
		// are: a base's curl to a host only a VPN reaches would hang.
		b.Egress, b.Link = agentproto.EgressListen, a.builderLink(lc.egress)
	}
	a.Note("starting the builder guest (%d CPUs, %d MiB)", cpus, mem)
	g, err := b.Start(out, template)
	if err != nil {
		return nil, Die("%v", err)
	}
	return g, nil
}

// writeContext writes a build context into a new dir.
func writeContext(dir string, write func(string) error) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := write(dir); err != nil {
		return fmt.Errorf("writing the build context: %w", err)
	}
	return nil
}

// builderLink opens the host link to a builder guest, for the build's
// duration: it serves the outbound proxy alone, as egress_ports and
// egress_allow say -- no port forwarded, no URL opened, no notification,
// no SSH agent, no relay. Its refusals are said on stderr, among the
// build's output. Closing it ends the session and waits for it.
func (a *App) builderLink(egress *hostlink.Egress) func(vm.Dir) (io.Closer, error) {
	return func(d vm.Dir) (io.Closer, error) {
		conn, err := hvsock.Dial(d.Socket(), agentproto.PortLink, 10*time.Second)
		if err != nil {
			return nil, err
		}
		sess := agentproto.NewSession(conn, conn, true)
		logger := log.New(a.Stderr, "caboose: the builder's link: ", 0)
		cfg := hostlink.Config{Ports: hostlink.PortSet{}, OpenURL: hostlink.OpenOff, Actions: builderActions{},
			Log: logger, Egress: egress, Diagnose: linkdebug.Get().Raw != ""}
		var closing atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := hostlink.Run(sess, cfg); err != nil && !closing.Load() {
				logger.Printf("ended: %v", err)
			}
		}()
		return closerFunc(func() error {
			closing.Store(true)
			sess.Close()
			conn.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			return nil
		}), nil
	}
}

// builderActions are the builder link's: it does nothing for the guest
// but its outbound proxy.
type builderActions struct{}

var errBuilderAction = errors.New("not for the builder guest")

func (builderActions) OpenURL(string) error         { return errBuilderAction }
func (builderActions) Notify(string, string) error  { return errBuilderAction }
func (builderActions) Confirm(string) (bool, error) { return false, errBuilderAction }

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
