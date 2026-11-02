package launcher

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/version"
	"github.com/bfreis/caboose/internal/vm"
)

// The vm isolation: the sandbox is a VM of caboose's own, run by
// caboose-vmm on Virtualization.framework, with no Docker engine. Its
// image is a root disk the builder guest wrote (vmimages.go), its kernel
// and the builder's disk are files beside the launcher, and everything a
// container's docker exec did goes through the agent in the guest
// (backend.VM). What of the data dir it uses sits in vm/: a directory per
// VM (the sandbox's is named after the container, the builder's
// "builder"), and images/.

// vmDirName is the data dir's directory of everything vm.
const vmDirName = "vm"

// maxSocketPath is the longest path a Unix socket may have on a Mac:
// sun_path's 104 bytes, the last a NUL. The daemon's socket has the same
// limit, so it is one constant.
const maxSocketPath = daemonproto.MaxSocketPath

// isVM reports whether the configuration's isolation is vm.
func (a *App) isVM() bool { return isolationOf(a.Cfg) == isolationVM }

// vmRoot is the data dir's vm/.
func (a *App) vmRoot() string { return filepath.Join(a.Cfg.DataDir, vmDirName) }

// vmDir is the directory of the VM called name.
func (a *App) vmDir(name string) vm.Dir { return vm.Dir(filepath.Join(a.vmRoot(), name)) }

// vmBox is the sandbox as a VM, named after the container.
func (a *App) vmBox() *backend.VM {
	v := backend.NewVM(a.Cfg.Container, a.vmDir(a.Cfg.Container), &vmHost{a: a})
	v.Ready = ReadyMarker
	// The launcher says what went wrong, as Die; the backend only returns
	// it.
	v.Stderr = nil
	v.Note = a.Note
	if exe, err := a.executable(); err == nil {
		v.Self = exe
	}
	return v
}

// vmSize is the VM's CPUs and memory in MiB: config.toml's, else half this
// machine's CPUs and half its memory, at most 8 GiB.
func vmSize(c *config.Config) (cpus, memMiB int, err error) {
	cpus = max(1, runtime.NumCPU()/2)
	if c.VMCPUs > 0 {
		cpus = c.VMCPUs
	}
	memMiB = 4096
	if m := hostMemory(); m > 0 {
		memMiB = int(min(m/2, 8<<30) >> 20)
	}
	if c.VMMemory != "" {
		if memMiB, err = parseMemory(c.VMMemory); err != nil {
			return 0, 0, fmt.Errorf("%s: %v", c.ProfileKey("memory"), err)
		}
	}
	return cpus, memMiB, nil
}

// parseMemory is an amount of memory in MiB: "8G", "8GiB", "4096M",
// "4096MiB", or a number of MiB.
func parseMemory(s string) (int, error) { return config.ParseMemory(s) }

// vmFiles are what the vm isolation runs, beside the launcher.
type vmFiles struct {
	VMM     string // caboose-vmm, signed with the virtualization entitlement
	Kernel  string // the guest's kernel
	Builder string // the builder guest's disk
	Arch    string // the guests' architecture: the Mac's
}

// findVMFiles finds caboose-vmm next to this launcher (as a release
// installs it, and make vmm builds it), and the kernel and builder disk in
// CABOOSE_HOME/vm/<tag>/<arch>/ (a release's), else the checkout's vm-dist/
// (make vm-kernel and make vm-builder).
func (a *App) findVMFiles() (vmFiles, error) {
	if a.vmFiles != nil {
		return a.vmFiles()
	}
	f := vmFiles{Arch: runtime.GOARCH}
	if runtime.GOOS != "darwin" {
		return f, fmt.Errorf("isolation vm runs only on macOS on Apple silicon (%s)", isolationOrigin(a.Cfg))
	}
	if f.Arch != "arm64" {
		return f, fmt.Errorf("isolation vm runs only on macOS on Apple silicon, not on %s (%s)", f.Arch, isolationOrigin(a.Cfg))
	}
	var missing []string
	exe, err := a.executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err == nil {
		f.VMM = filepath.Join(filepath.Dir(exe), "caboose-vmm")
	}
	if f.VMM == "" || !isFile(f.VMM) {
		missing = append(missing, "caboose-vmm, next to caboose ('make vmm' in the checkout)")
	}
	find := func(name, make string) string {
		var dirs []string
		if tag := a.releaseTag(); tag != "" {
			dirs = append(dirs, a.vmReleaseDir(tag, f.Arch))
		}
		if a.Checkout != "" {
			dirs = append(dirs, filepath.Join(a.Checkout, "vm-dist"))
		}
		for _, d := range dirs {
			if p := filepath.Join(d, name); isFile(p) {
				return p
			}
		}
		if a.releaseTag() != "" {
			missing = append(missing, name+" (fetched from this caboose's release when vm first needs it)")
		} else {
			missing = append(missing, fmt.Sprintf("%s ('%s' in the checkout)", name, make))
		}
		return ""
	}
	f.Kernel = find("kernel-"+f.Arch, "make vm-kernel")
	f.Builder = find("builder-"+f.Arch+".img", "make vm-builder")
	if len(missing) > 0 {
		return f, fmt.Errorf("isolation vm needs files this caboose does not have: %s", strings.Join(missing, ", "))
	}
	return f, nil
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// checkVM is checkRuntime for the vm isolation: the files it runs, and a
// data dir whose VM sockets fit a Unix socket's path.
func (a *App) checkVM() error {
	if _, err := a.ensureVMFiles(); err != nil {
		return Die("%v", err)
	}
	for _, name := range []string{a.Cfg.Container, builderName} {
		if s := a.vmDir(name).Socket(); len(s) > maxSocketPath {
			return Die("the data dir's path is too long for the VM's socket: %s is %d bytes, and macOS allows %d. Set CABOOSE_DATA_DIR to a shorter path",
				s, len(s), maxSocketPath)
		}
	}
	return nil
}

// builderName is the builder guest's VM directory.
const builderName = "builder"

// dockerVolume is the volume dockerd in the sandbox keeps its images on.
const dockerVolume = "docker"

// initramfs writes the guest's init, the agent this launcher embeds, as
// an initramfs in the data dir's vm/, so the init always matches the
// launcher that boots it.
func (a *App) initramfs(arch string) (string, error) {
	agent, err := assets.Agent(arch)
	if err != nil {
		return "", fmt.Errorf("this caboose embeds no caboose-agent for %s: %v", arch, err)
	}
	var b bytes.Buffer
	if err := vm.WriteInitramfs(&b, agent); err != nil {
		return "", err
	}
	p := filepath.Join(a.vmRoot(), "initramfs-"+arch+".cpio")
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b.Bytes()) {
		return p, nil
	}
	if err := os.MkdirAll(a.vmRoot(), 0o700); err != nil {
		return "", err
	}
	tmp := p + ".new"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// startVMM starts caboose-vmm for dir, detached, its output appended to
// dir's vmm.log: the VM lives as long as it does.
func (a *App) startVMM(vmm string, dir vm.Dir) error {
	if err := os.MkdirAll(string(dir), 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(dir.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(vmm, string(dir))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// vmHost is backend.VMHost for the launcher: its images, files and size.
type vmHost struct{ a *App }

func (h *vmHost) Image(ref string) (backend.VMImage, error) {
	rec, ok, err := h.a.vmImages().get(ref)
	if err != nil {
		return backend.VMImage{}, err
	}
	if !ok {
		return backend.VMImage{}, fmt.Errorf("no image '%s' yet: 'caboose build' builds it", ref)
	}
	return backend.VMImage{
		ID: rec.ID, Disk: h.a.vmImages().disk(rec), Labels: rec.Labels,
		// The sandbox runs as root in the guest (the shares show every
		// file as root's), so the image's USER, a
		// name, is not asked for.
		User: rootUser,
		Env:  rec.Config.Env, Entrypoint: rec.Config.Entrypoint, Cmd: rec.Config.Cmd,
	}, nil
}

func (h *vmHost) Machine() (vm.Machine, error) {
	f, err := h.a.findVMFiles()
	if err != nil {
		return vm.Machine{}, err
	}
	initramfs, err := h.a.initramfs(f.Arch)
	if err != nil {
		return vm.Machine{}, err
	}
	cpus, mem, err := vmSize(h.a.Cfg)
	if err != nil {
		return vm.Machine{}, err
	}
	return vm.Machine{Kernel: f.Kernel, Initramfs: initramfs, CPUs: cpus, MemoryMiB: mem, Network: h.a.vmNetwork()}, nil
}

// NewScratch makes the scratch disk, of the profile's scratch_size: the
// size is read from config.toml at every start, so a new one takes effect
// at the next.
func (h *vmHost) NewScratch(path string) error {
	size, err := scratchSize(h.a.Cfg)
	if err != nil {
		return err
	}
	tmpl, err := h.a.vmTemplate()
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	return vm.NewDisk(tmpl, path, size)
}

// vmTemplate is the empty ext4 every scratch disk and volume is made
// from, made first when it is missing (caboose build makes it with the
// first image, but a start must not fail for want of it): the builder
// guest, booted to write that disk alone. One launcher makes it at a
// time, and one that waited for another finds it made.
func (a *App) vmTemplate() (string, error) {
	store := a.vmImages()
	tmpl := store.template()
	if isFile(tmpl) {
		return tmpl, nil
	}
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return "", err
	}
	unlock, err := vm.HoldLock(filepath.Join(store.dir, ".template.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	if isFile(tmpl) {
		return tmpl, nil
	}
	a.Note("making %s, the empty disk scratch disks and volumes are made from, in the builder guest", a.short(tmpl))
	tmp := filepath.Join(store.dir, ".empty-new.img")
	boot := a.buildGuest
	if boot == nil {
		boot = func(out, template string) (vmBuildGuest, error) { return a.startBuilder(out, template) }
	}
	g, err := boot("", tmp)
	if err != nil {
		return "", err
	}
	defer g.Stop()
	if err := g.Template(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("the empty disk for scratch disks and volumes: %w", err)
	}
	if err := g.Stop(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("stopping the builder: %w", err)
	}
	if err := os.Rename(tmp, tmpl); err != nil {
		return "", err
	}
	return tmpl, nil
}

// scratchSize is the scratch disk's size in bytes: the profile's
// scratch_size, else config.DefaultScratchSize.
func scratchSize(c *config.Config) (int64, error) {
	if c.VMScratchSize == "" {
		return config.DefaultScratchSize, nil
	}
	n, err := config.ParseScratchSize(c.VMScratchSize)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", c.ProfileKey("scratch_size"), err)
	}
	return n, nil
}

// volumesDir is the data dir's directory of volumes' disks, beside every
// VM's directory, so caboose restart keeps them.
func (a *App) volumesDir() string { return filepath.Join(a.vmRoot(), "volumes") }

// volumeDisk is the disk of the volume called name.
func (a *App) volumeDisk(name string) string { return filepath.Join(a.volumesDir(), name+".img") }

// volumeSize is the size config.toml gives the volume called name: the
// profile's docker_size for caboose's own, else the size of the profile's
// volume of that name; ok is false for a volume the profile does not
// declare (the VM was created with it, and the profile changed since).
func volumeSize(c *config.Config, name string) (size int64, ok bool, err error) {
	if name == dockerVolume {
		n, err := dockerSize(c)
		return n, err == nil, err
	}
	for _, v := range c.Volumes {
		if v.Name == name {
			return v.Size, true, nil
		}
	}
	return 0, false, nil
}

// dockerSize is the size of the disk of the sandbox's dockerd: the
// profile's docker_size, else config.DefaultDockerSize.
func dockerSize(c *config.Config) (int64, error) {
	if c.VMDockerSize == "" {
		return config.DefaultDockerSize, nil
	}
	n, err := config.ParseScratchSize(c.VMDockerSize)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", c.ProfileKey("docker_size"), err)
	}
	return n, nil
}

// Volume is the volume's disk, at the size config.toml gives it, read
// at every start: made from the empty template at that size when it is
// missing, and grown to it when it is smaller, the guest growing its
// filesystem to fill it. A disk larger than its size is left as it is: a
// start never shrinks a volume, which would cut off what the guest wrote
// there. A volume the profile no longer declares keeps its disk's size,
// and is made at config.DefaultVolumeSize when missing. A disk another
// process holds (a VM of its own, caboose-vmm's lock) is refused here,
// naming the volume, rather than by caboose-vmm as the VM boots.
func (h *vmHost) Volume(name string) (backend.VMDisk, error) {
	size, ok, err := volumeSize(h.a.Cfg, name)
	if err != nil {
		return backend.VMDisk{}, err
	}
	p := h.a.volumeDisk(name)
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !ok {
			size = config.DefaultVolumeSize
		}
		if err := h.newVolume(p, size); err != nil {
			return backend.VMDisk{}, err
		}
	case err != nil:
		return backend.VMDisk{}, err
	case !fi.Mode().IsRegular():
		return backend.VMDisk{}, fmt.Errorf("%s is not a disk image", p)
	default:
		// Before it is grown: a VM running on it serves it at the size
		// it booted with.
		if err := diskFree(p); err != nil {
			return backend.VMDisk{}, err
		}
		if ok && fi.Size() < size {
			if err := os.Truncate(p, size); err != nil {
				return backend.VMDisk{}, fmt.Errorf("growing %s to %s: %v", p, gibString(size), err)
			}
		}
	}
	if fi, err = os.Stat(p); err != nil {
		return backend.VMDisk{}, err
	}
	return backend.VMDisk{Path: p, Size: fi.Size()}, nil
}

// newVolume makes a volume's disk at p, of size, from the empty template.
func (h *vmHost) newVolume(p string, size int64) error {
	tmpl, err := h.a.vmTemplate()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	_ = os.Remove(p + ".new")
	if err := vm.NewDisk(tmpl, p+".new", size); err != nil {
		os.Remove(p + ".new")
		return err
	}
	return os.Rename(p+".new", p)
}

// diskFree says, as an error, when another process holds the lock
// caboose-vmm takes on a disk it serves.
func diskFree(p string) error {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("its disk %s is in use by another process (a VM still running on it?)", p)
	}
	if err != nil {
		return fmt.Errorf("locking its disk %s: %v", p, err)
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func (h *vmHost) StartVMM(dir vm.Dir) error {
	f, err := h.a.findVMFiles()
	if err != nil {
		return err
	}
	h.a.noteVMMVersion(f.VMM)
	return h.a.startVMM(f.VMM, dir)
}

// noteVMMVersion says, as a VM boots, when caboose-vmm is not this
// caboose's own version: an older one ignores a share's ReadOnly, so the
// Mac does not refuse the guest's writes to caboose's instructions. It
// never stops the boot (a matching caboose-vmm may need signing the user
// cannot do yet), and a check that fails says only what it could not
// tell; the check bounds itself (vmmCheckTimeout).
func (a *App) noteVMMVersion(path string) {
	v := a.checkVMM(path)
	mine := version.Get().Version
	if v.Check.Version == mine {
		return
	}
	a.Note("caboose-vmm is %s and this caboose %s, so read-only mounts (caboose's instructions at %s) are not enforced by the Mac until they match: %s",
		or(v.Check.Version, "of an unknown version"), mine, datadir.ManagedTarget, a.vmmFix())
}

// doctorVM is doctorIsolation under vm: the files it runs, caboose-vmm's
// own check of this Mac and of its signature, the socket's path, the
// size.
func (a *App) doctorVM(c *checkup) {
	f, err := a.findVMFiles()
	if f.VMM != "" && isFile(f.VMM) {
		if v := a.checkVMM(f.VMM); v.Problem != "" {
			c.problem("isolation", v.Fix, "vm: %s", v.Problem)
			return
		}
	}
	if err != nil && a.vmFetchable(f) {
		c.note("isolation", "vm: its kernel and builder for %s are fetched from the release the first time vm needs them", a.releaseTag())
		return
	}
	if err != nil {
		c.problem("isolation", "in the checkout, make vmm vm-kernel vm-builder; or "+otherProfile(a.Cfg),
			"%v", err)
		return
	}
	if s := a.vmDir(a.Cfg.Container).Socket(); len(s) > maxSocketPath {
		c.problem("isolation", "set CABOOSE_DATA_DIR to a shorter path",
			"the VM's socket path, %s, is %d bytes: macOS allows %d", s, len(s), maxSocketPath)
		return
	}
	cpus, mem, err := vmSize(a.Cfg)
	if err != nil {
		c.problem("isolation", "fix it in config.toml", "%v", err)
		return
	}
	c.ok("isolation", "vm: %d CPUs, %d MiB; %s, kernel %s, builder %s", cpus, mem, f.VMM, f.Kernel, f.Builder)
	a.doctorVMDisks(c)
}

// doctorVMDisks checks what the VM's disks are made from, and the scratch
// disk's size against the space the data dir's volume has.
func (a *App) doctorVMDisks(c *checkup) {
	size, err := scratchSize(a.Cfg)
	if err != nil {
		c.problem("disks", "fix it in config.toml", "%v", err)
		return
	}
	if tmpl := a.vmImages().template(); !isFile(tmpl) {
		c.note("disks", "%s, the empty disk scratch disks and volumes are made from, is missing: the next start makes it in the builder guest, or 'caboose build' now", a.short(tmpl))
		return
	}
	docker, err := dockerSize(a.Cfg)
	if err != nil {
		c.problem("disks", "fix it in config.toml", "%v", err)
		return
	}
	warned := false
	for _, w := range []string{scratchSpaceWarning(a.Cfg.DataDir, size), dockerSpaceWarning(a.Cfg.DataDir, docker)} {
		if w != "" {
			c.note("disks", "%s", w)
			warned = true
		}
	}
	if !warned {
		c.ok("disks", "a scratch disk of up to %s at each start, and dockerd's of up to %s", gibString(size), gibString(docker))
	}
}

// vmFreeSpace is the space free to this user on the volume that holds
// path. A variable for the tests.
var vmFreeSpace = func(path string) (int64, error) {
	avail, _, err := realStatfs(path)
	return int64(avail), err
}

// scratchSpaceWarning says when a scratch disk of size could outgrow the
// space free on the volume of the data dir, which holds it: allowed, since
// the disk takes only what the VM writes, but a VM that writes that much
// fills the volume first. "" when it fits, or when the space is unknown.
func scratchSpaceWarning(dataDir string, size int64) string {
	return diskSpaceWarning(dataDir, "scratch_size", "the scratch disk", size)
}

// dockerSpaceWarning is scratchSpaceWarning for dockerd's disk, of
// docker_size, which the data dir holds too.
func dockerSpaceWarning(dataDir string, size int64) string {
	return diskSpaceWarning(dataDir, "docker_size", "dockerd's disk", size)
}

// diskSpaceWarning says when disk, of size as key gives it, could outgrow
// the space free on the volume of the data dir; "" when it fits, or when
// the space is unknown.
func diskSpaceWarning(dataDir, key, disk string, size int64) string {
	free, err := vmFreeSpace(dataDir)
	if err != nil || size <= free {
		return ""
	}
	return fmt.Sprintf("%s is %s, and the volume of the data dir (%s) has %s free: %s takes only what the VM writes, but a VM that writes past %s fills that volume first",
		key, gibString(size), dataDir, gibString(free), disk, gibString(free))
}

// gibString is n bytes in the units config.toml writes a size in: "64G",
// or "10.5G" for what is not a whole number of them.
func gibString(n int64) string {
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%dG", n>>30)
	}
	return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
}
