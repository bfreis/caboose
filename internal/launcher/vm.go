package launcher

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
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
// sun_path's 104 bytes, the last a NUL.
const maxSocketPath = 103

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
func parseMemory(s string) (int, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	mul := 1
	switch {
	case strings.HasSuffix(t, "GIB"), strings.HasSuffix(t, "GB"), strings.HasSuffix(t, "G"):
		mul, t = 1024, strings.TrimRight(t, "GIB")
	case strings.HasSuffix(t, "MIB"), strings.HasSuffix(t, "MB"), strings.HasSuffix(t, "M"):
		t = strings.TrimRight(t, "MIB")
	}
	n, err := strconv.Atoi(t)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not an amount like \"8G\" or \"4096M\"", s)
	}
	if n*mul < 512 {
		return 0, fmt.Errorf("%q is under 512M, too little for the sandbox", s)
	}
	return n * mul, nil
}

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
// (make vm-kernel and make vm-builder). A release carries neither yet, which the error says.
func (a *App) findVMFiles() (vmFiles, error) {
	if a.vmFiles != nil {
		return a.vmFiles()
	}
	f := vmFiles{Arch: runtime.GOARCH}
	if runtime.GOOS != "darwin" {
		return f, fmt.Errorf("isolation vm runs on macOS only, for now (%s)", isolationOrigin(a.Cfg))
	}
	if f.Arch != "arm64" {
		return f, fmt.Errorf("isolation vm runs on Apple silicon only, for now: there is no builder for %s yet", f.Arch)
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
		// The sandbox runs as root in the guest (decided 2026-09-27: the
		// shares show every file as root's), so the image's USER, a
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
	return vm.Machine{Kernel: f.Kernel, Initramfs: initramfs, CPUs: cpus, MemoryMiB: mem, Network: "nat"}, nil
}

func (h *vmHost) NewScratch(path string) error {
	tmpl := h.a.vmImages().template()
	if !isFile(tmpl) {
		return errors.New("no empty disk to clone a scratch disk from: 'caboose build' makes it")
	}
	_ = os.Remove(path)
	return vm.CloneFile(tmpl, path)
}

// Volume is the volume's disk in the data dir's vm/volumes/, outside
// every VM's directory, so caboose restart keeps it: cloned from the empty
// template when it is new.
func (h *vmHost) Volume(name string) (string, error) {
	dir := filepath.Join(h.a.vmRoot(), "volumes")
	p := filepath.Join(dir, name+".img")
	if isFile(p) {
		return p, nil
	}
	tmpl := h.a.vmImages().template()
	if !isFile(tmpl) {
		return "", errors.New("no empty disk to make it from: 'caboose build' makes it")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := vm.CloneFile(tmpl, p+".new"); err != nil {
		os.Remove(p + ".new")
		return "", err
	}
	return p, os.Rename(p+".new", p)
}

func (h *vmHost) StartVMM(dir vm.Dir) error {
	f, err := h.a.findVMFiles()
	if err != nil {
		return err
	}
	return h.a.startVMM(f.VMM, dir)
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
}
