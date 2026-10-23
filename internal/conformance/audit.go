package conformance

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
)

// Inspection is what the sandbox was really created with, as the launcher
// side read it: under container and gvisor from the engine's inspect
// output, under vm from the machine.json the launcher wrote for the vmm,
// and in both from the link's hello. It is never recomputed from the
// configuration, which would only check the configuration against itself.
type Inspection struct {
	Isolation string
	// Name and ID are the container's (container, gvisor): its name
	// without docker's leading "/", and its full 64-hex ID.
	Name, ID string
	Labels   map[string]string
	Mounts   []Mount
	// Privileged, Devices, CapAdd, SecurityOpt and the namespace modes
	// are docker inspect's HostConfig (container, gvisor).
	Privileged                    bool
	Devices, CapAdd, SecurityOpt  []string
	NetworkMode, PidMode, IpcMode string
	UTSMode, UsernsMode           string
	CgroupnsMode                  string
	// Shares are machine.json's (vm).
	Shares []Share
	// Machine is the rest of what the vm isolation's record says (vm).
	Machine *Machine
	// Hello is what the host's link offered the sandbox. Required.
	Hello *Hello
	// Config is what the environment's config.toml, as the launcher
	// loaded it, says of the keys the audit pins.
	Config ConfigFacts
}

// Machine is what the audit reads of a VM's machine.json, and where it
// read it.
type Machine struct {
	// Dir is the VM's directory, which holds machine.json.
	Dir string
	// Network is machine.json's network: "nat" or "none".
	Network string
	// Paths are the host paths machine.json names that the VM writes or
	// that hold the guest's filesystems: its writable disks and the
	// console. Each must be inside B.
	Paths []string
	// BootPaths are the ones it only reads to boot: the kernel, the
	// initramfs and the read-only disks. Each must be inside B or exactly
	// one of the boot inputs the launcher side named (Inputs.BootInputs).
	BootPaths []string
}

// Mount is one mount: a bind (Source a host path) or an engine volume
// (Volume its name, Source empty).
type Mount struct {
	Source, Target string
	Volume         string
	ReadOnly       bool
}

// MountSet is ms by target and mode, sorted, each once: how a plan of
// mounts and a sandbox's are compared whatever the isolation, since a
// VM binds paths of its share and has its volumes as disks, where docker
// lists sources and volumes by name.
func MountSet(ms []Mount) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = fmt.Sprintf("%s ro=%v", m.Target, m.ReadOnly)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Share is one virtio-fs share of a VM.
type Share struct {
	Name, Path string
	ReadOnly   bool
}

// Hello is what the link's hello offered.
type Hello struct {
	SSHAgent bool
	HostExec bool
}

// ConfigFacts are the configuration keys the audit requires.
type ConfigFacts struct {
	RunArgs      []string
	EngineSocket bool
	HostExec     bool
	SSHAgent     string // [link] ssh_agent; must be "none"
	ForwardPorts string // [link] forward_ports; must be "none"
}

// AuditedMount is one mount the audit let through, as the sandbox-side
// gate compares it with /proc/self/mountinfo.
type AuditedMount struct {
	Target   string
	ReadOnly bool
}

// AuditResult is what passed the audit.
type AuditResult struct {
	Mounts []AuditedMount // sorted by target
}

// Equal reports whether r and o let through the same mounts.
func (r AuditResult) Equal(o AuditResult) bool { return slices.Equal(r.Mounts, o.Mounts) }

// expectation is what the audit compares an Inspection against.
type expectation struct {
	base, env, isolation  string
	trust                 string
	dataDir               string // the environment's data dir, inside B/home
	cabooseHome, userHome string
	roots                 []string // host paths of the suite's roots
	protected             []string // paths under B no source may overlap
	forbidden             []string
	planned               []Share
	volumes               []string
	bootInputs            []string
}

func (g *Guard) expect() expectation {
	in := g.inputs()
	e := expectation{
		base:        g.base,
		env:         g.env,
		isolation:   g.isolation,
		trust:       g.trust,
		cabooseHome: physical(g.facts.CabooseHome),
		userHome:    physical(g.facts.UserHome),
		forbidden:   forbiddenSources(),
		planned:     in.PlannedShares,
		volumes:     in.Volumes,
		bootInputs:  in.BootInputs,
	}
	for _, r := range g.RootNames() {
		e.roots = append(e.roots, g.RootPath(r))
	}
	// The data dir is inside the throwaway CABOOSE_HOME, so the home as a
	// whole cannot be protected: a source must be strictly inside the data
	// dir (or a root), and what of the home and the data dir no source may
	// reach is named one by one.
	home := g.CabooseHome()
	envDir := filepath.Join(g.base, filepath.FromSlash(g.EnvDirRel()))
	e.dataDir = g.DataDir()
	e.protected = append(e.protected, filepath.Join(g.base, filepath.FromSlash(MarkerFile)),
		filepath.Join(g.base, "canary"), filepath.Join(home, "vm"),
		filepath.Join(envDir, config.FileName), filepath.Join(g.base, filepath.FromSlash(g.StoreRel())),
		filepath.Join(g.DataDir(), "vm"), filepath.Join(g.base, filepath.FromSlash(g.DataMarker())))
	e.protected = append(e.protected, daemon.Dirs(home)...)
	for _, f := range []string{"grants.json", "requests.json", "decisions.log", "broker.lock"} {
		e.protected = append(e.protected, filepath.Join(envDir, f))
	}
	for _, f := range []string{"link.lock", "link.log", "link.json"} {
		e.protected = append(e.protected, filepath.Join(g.DataDir(), f))
	}
	return e
}

var (
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// The namespace modes allowed besides the engine's default (""):
	// never the host's, never another container's.
	networkModes = map[string]bool{"default": true, "bridge": true, "none": true}
	ipcModes     = map[string]bool{"private": true, "shareable": true, "none": true}
	privateModes = map[string]bool{"private": true}
	// unconfined are the security options that turn a confinement off,
	// as key and value ("seccomp=unconfined", or the old "seccomp:...").
	unconfined = map[string]string{
		"seccomp":           "unconfined",
		"apparmor":          "unconfined",
		"label":             "disable",
		"no-new-privileges": "false",
		"systempaths":       "unconfined",
	}
)

// checkMode refuses a namespace mode that is not the engine's default or
// one of allowed.
func checkMode(what, mode string, allowed map[string]bool) error {
	if mode == "" || allowed[mode] {
		return nil
	}
	return fmt.Errorf("the container's %s mode is %q", what, mode)
}

func checkSecurityOpt(opts []string) error {
	for _, o := range opts {
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			k, v, _ = strings.Cut(o, ":")
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.ToLower(strings.TrimSpace(v))
		if bad, ok := unconfined[k]; ok && v == bad {
			return fmt.Errorf("the container has security option %q", o)
		}
	}
	return nil
}

// Audit refuses an Inspection of a sandbox that can reach anything the
// suite did not make, and returns the mounts it let through.
func Audit(in Inspection, e expectation) (AuditResult, error) {
	var res AuditResult
	if in.Isolation != e.isolation {
		return res, fmt.Errorf("the sandbox's isolation is %q, not %q", in.Isolation, e.isolation)
	}
	if err := auditConfig(in); err != nil {
		return res, err
	}
	if in.Hello == nil {
		return res, errors.New("the link's hello was not read")
	}
	if in.Hello.SSHAgent {
		return res, errors.New("the link offers the SSH agent")
	}
	if in.Hello.HostExec {
		return res, errors.New("the link offers host exec")
	}
	if got := in.Labels[assets.LabelIsolation]; got != e.isolation {
		return res, fmt.Errorf("the sandbox's isolation label is %q, not %q", got, e.isolation)
	}
	if e.isolation == config.KindVM {
		if err := auditMachine(in.Machine, e); err != nil {
			return res, err
		}
		if err := auditShares(in.Shares, e); err != nil {
			return res, err
		}
	} else {
		if err := auditContainer(in, e); err != nil {
			return res, err
		}
	}
	seen := map[string]bool{}
	for _, m := range in.Mounts {
		if !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target {
			return res, fmt.Errorf("mount target %q is not a clean absolute path", m.Target)
		}
		if seen[m.Target] {
			return res, fmt.Errorf("two mounts at %s", m.Target)
		}
		seen[m.Target] = true
		if m.Volume != "" {
			if m.Source != "" || !slices.Contains(e.volumes, m.Volume) {
				return res, fmt.Errorf("volume %q at %s is not one of the environment's", m.Volume, m.Target)
			}
		} else if err := auditSource(m.Source, e); err != nil {
			return res, fmt.Errorf("mount at %s: %w", m.Target, err)
		}
		res.Mounts = append(res.Mounts, AuditedMount{Target: m.Target, ReadOnly: m.ReadOnly})
	}
	slices.SortFunc(res.Mounts, func(a, b AuditedMount) int { return strings.Compare(a.Target, b.Target) })
	return res, nil
}

func auditConfig(in Inspection) error {
	c := in.Config
	switch {
	case len(c.RunArgs) != 0:
		return errors.New("run_args is set")
	case c.EngineSocket:
		return errors.New("engine_socket is on")
	case c.HostExec:
		return errors.New("host_exec is on")
	case c.SSHAgent != "none":
		return fmt.Errorf("ssh_agent is %q, not \"none\"", c.SSHAgent)
	case c.ForwardPorts != "none":
		return fmt.Errorf("forward_ports is %q, not \"none\"", c.ForwardPorts)
	}
	return nil
}

func auditContainer(in Inspection, e expectation) error {
	if want := config.ContainerFor(e.env); in.Name != want {
		return fmt.Errorf("the container is %q, not %q", in.Name, want)
	}
	if !hex64.MatchString(in.ID) {
		return fmt.Errorf("the container's ID %q is not a full ID", in.ID)
	}
	switch {
	case in.Privileged:
		return errors.New("the container is privileged")
	case len(in.Devices) != 0:
		return errors.New("the container has devices")
	case len(in.CapAdd) != 0:
		return errors.New("the container has added capabilities")
	}
	for _, c := range []struct {
		what, mode string
		allowed    map[string]bool
	}{
		{"network", in.NetworkMode, networkModes},
		{"PID", in.PidMode, privateModes},
		{"IPC", in.IpcMode, ipcModes},
		{"UTS", in.UTSMode, privateModes},
		{"user namespace", in.UsernsMode, privateModes},
		{"cgroup namespace", in.CgroupnsMode, privateModes},
	} {
		if err := checkMode(c.what, c.mode, c.allowed); err != nil {
			return err
		}
	}
	if err := checkSecurityOpt(in.SecurityOpt); err != nil {
		return err
	}
	if in.Machine != nil {
		return errors.New("a container has a VM's machine")
	}
	if len(in.Shares) != 0 {
		return errors.New("a container has VM shares")
	}
	return nil
}

// auditMachine refuses a VM whose record is not this environment's, names
// a path outside B, or has a network the run's trust does not allow.
func auditMachine(m *Machine, e expectation) error {
	if m == nil {
		return errors.New("the VM's machine.json was not read")
	}
	want := filepath.Join(e.dataDir, "vm", config.ContainerFor(e.env))
	if physical(m.Dir) != want {
		return fmt.Errorf("the VM's directory is %s, not %s", m.Dir, want)
	}
	switch {
	case m.Network == "none":
	case m.Network == "nat" && e.trust != config.TrustHostile:
	default:
		return fmt.Errorf("the VM's network is %q under trust %s", m.Network, e.trust)
	}
	for _, p := range m.Paths {
		if pp := physical(p); pp == "" || !strictlyInside(pp, e.base) {
			return fmt.Errorf("the VM's machine names %s, outside the base", p)
		}
	}
	for _, p := range m.BootPaths {
		pp := physical(p)
		if pp != "" && strictlyInside(pp, e.base) {
			continue
		}
		if err := bootInput(p, e.bootInputs); err != nil {
			return err
		}
	}
	return nil
}

// bootInput refuses a boot path outside B that is not, exactly, one of
// the boot inputs named, or is not a regular file: a kernel, an
// initramfs or a read-only disk the VM reads to boot, which the guest
// cannot reach as a filesystem. Never a prefix: the directory one is in
// is not allowed by it.
func bootInput(p string, inputs []string) error {
	pp := physical(p)
	if pp == "" {
		return fmt.Errorf("the VM's machine names %q, not an absolute path", p)
	}
	for _, in := range inputs {
		if ip := physical(in); ip != "" && ip == pp {
			fi, err := os.Lstat(pp)
			if err != nil {
				return fmt.Errorf("the VM's boot input %s: %v", p, err)
			}
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("the VM's boot input %s is not a regular file", p)
			}
			return nil
		}
	}
	return fmt.Errorf("the VM's machine names %s, outside the base and not a boot input", p)
}

func auditShares(got []Share, e expectation) error {
	if e.planned == nil {
		return errors.New("the planned shares were not given")
	}
	for _, s := range got {
		if err := auditSource(s.Path, e); err != nil {
			return fmt.Errorf("share %s: %w", s.Name, err)
		}
	}
	key := func(s Share) string { return fmt.Sprintf("%s\x00%s\x00%t", s.Name, physical(s.Path), s.ReadOnly) }
	a, b := mapSlice(got, key), mapSlice(e.planned, key)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return errors.New("the VM's shares are not the ones planned for the base")
	}
	return nil
}

func mapSlice[T any](s []T, f func(T) string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = f(v)
	}
	return out
}

// auditSource refuses a host source that is not strictly inside the
// environment's data dir (B/home/envs/<env>/data) or inside one of the
// suite's roots, or that is or overlaps anything the
// sandbox must not reach.
func auditSource(src string, e expectation) error {
	if !filepath.IsAbs(src) {
		return fmt.Errorf("source %q is not an absolute path", src)
	}
	clean := filepath.Clean(src)
	for _, f := range e.forbidden {
		if clean == filepath.Clean(f) {
			return fmt.Errorf("source %s is a forbidden socket", clean)
		}
	}
	fi, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("source %s: %v", clean, err)
	}
	if fi.Mode()&fs.ModeSocket != 0 {
		return fmt.Errorf("source %s is a socket", clean)
	}
	p, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("source %s: %v", clean, err)
	}
	if p == "/" {
		return errors.New("source is /")
	}
	if p == e.userHome || strictlyInside(e.userHome, p) {
		return fmt.Errorf("source %s is or contains the home directory", p)
	}
	if overlaps(p, e.cabooseHome) {
		return fmt.Errorf("source %s overlaps the real CABOOSE_HOME", p)
	}
	if p == e.base || strictlyInside(e.base, p) {
		return fmt.Errorf("source %s is or is above the base", p)
	}
	for _, x := range e.protected {
		if overlaps(p, x) {
			return fmt.Errorf("source %s reaches %s", p, x)
		}
	}
	if e.dataDir != "" && strictlyInside(e.dataDir, e.base) && strictlyInside(p, e.dataDir) {
		return nil
	}
	for _, r := range e.roots {
		if p == r || strictlyInside(p, r) {
			return nil
		}
	}
	return fmt.Errorf("source %s is not inside the base's data dir or one of its roots", p)
}
