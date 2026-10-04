package backend

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/hvsock"
	"github.com/bfreis/caboose/internal/vm"
)

// VM is a sandbox that is a VM of caboose's own: the vm isolation. A
// detached vmm process owns the VM and serves its vm.sock; everything
// else goes to the agent in the guest through that socket, and what
// docker inspect would say is the launcher's own record, vm.State.
type VM struct {
	Name string
	Dir  vm.Dir
	Host VMHost
	// Self is this launcher's executable, which Command runs as the exec
	// helper; "" is os.Executable.
	Self string
	// Ready is the file the entrypoint makes once the sandbox is ready.
	Ready string
	// Stderr gets what Stop and Remove have to say, as docker's own
	// stderr does.
	Stderr io.Writer
}

// VMHost is what the VM needs of the rest of caboose: its images, its
// kernel and size, its disks, and vmm.
type VMHost interface {
	// Image resolves the Spec's image.
	Image(ref string) (VMImage, error)
	// Machine is the VM's kernel, initramfs and size; the backend adds
	// the disks, shares and console.
	Machine() (vm.Machine, error)
	// NewScratch makes an empty scratch disk at path, over any there.
	NewScratch(path string) error
	// Volume is the disk of the volume called name, made empty when it
	// is new: kept apart from the VM's directory, so it outlives it.
	Volume(name string) (string, error)
	// StartVMM starts vmm, detached, to boot the VM of dir's machine.json
	// and serve its vm.sock.
	StartVMM(dir vm.Dir) error
}

// VMImage is an image as the vm isolation boots it: a disk, and what the
// image's config says, which the guest cannot read.
type VMImage struct {
	ID   string
	Disk string // the root disk, attached read-only
	// Labels are the image's, which the VM has too, under its own, as a
	// container has its image's.
	Labels map[string]string
	// User is the image's USER, as UID or UID:GID.
	User       string
	Env        []string
	Entrypoint []string
	Cmd        []string
}

// NewVM is the VM name, in dir.
func NewVM(name string, dir vm.Dir, host VMHost) *VM {
	return &VM{Name: name, Dir: dir, Host: host, Stderr: os.Stderr}
}

func (v *VM) say(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	if v.Stderr != nil {
		fmt.Fprintf(v.Stderr, "caboose: %v\n", err)
	}
	return err
}

// State is "absent" with no record of the VM, "running" while vmm serves
// its socket, and "exited" otherwise.
func (v *VM) State() string {
	if _, err := os.Stat(v.Dir.State()); err != nil {
		return "absent"
	}
	if v.Dir.Serving() {
		return "running"
	}
	return "exited"
}

// Create records the VM from s, then starts it. What a container would
// take from docker run's flags and the image, the agent gets as a boot
// spec; the mounts become directories of the VM's one share.
func (v *VM) Create(s Spec) error {
	if _, err := os.Stat(v.Dir.State()); err == nil {
		return fmt.Errorf("the VM %s already exists", v.Name)
	}
	switch {
	case s.Runtime != "":
		return fmt.Errorf("a VM has no runtime to choose (%s)", s.Runtime)
	case len(s.RunArgs) > 0:
		return errors.New("docker_run_args means nothing to a VM: vm_cpus and vm_memory size it")
	case len(s.Groups) > 0:
		return errors.New("a VM's user takes no host groups")
	}
	img, err := v.Host.Image(s.Image)
	if err != nil {
		return err
	}
	user := s.User
	if user == "" {
		user = img.User
	}
	if !numericUser.MatchString(user) {
		return fmt.Errorf("the VM's user must be UID or UID:GID, not %q: the guest has no names to look it up by before it boots", user)
	}
	cmd := s.Cmd
	if len(cmd) == 0 {
		cmd = img.Cmd
	}
	cmd = append(append([]string(nil), img.Entrypoint...), cmd...)
	if len(cmd) == 0 {
		return fmt.Errorf("the image %s has nothing to run", s.Image)
	}
	shares, mounts, err := planShares(s.Mounts)
	if err != nil {
		return err
	}
	m, err := v.Host.Machine()
	if err != nil {
		return err
	}
	m.Disks = []vm.Disk{{Path: img.Disk, ReadOnly: true}, {Path: v.Dir.Scratch()}}
	var disks []agentproto.GuestDisk
	var volumes []string
	for i, vol := range s.Volumes {
		if i >= 24 {
			return errors.New("a VM takes at most 24 volumes")
		}
		p, err := v.Host.Volume(vol.Name)
		if err != nil {
			return fmt.Errorf("volume %s: %w", vol.Name, err)
		}
		m.Disks = append(m.Disks, vm.Disk{Path: p})
		volumes = append(volumes, vol.Name)
		disks = append(disks, agentproto.GuestDisk{Device: "/dev/vd" + string(rune('c'+i)), Target: vol.Target})
	}
	m.Shares = shares
	m.Console = v.Dir.Console()
	if m.Network == "" {
		m.Network = "nat"
	}
	st := vm.State{
		Image:  img.ID,
		Labels: map[string]string{},
		Boot: agentproto.BootSpec{
			Hostname: s.Hostname,
			Env:      mergeEnv(img.Env, s.Env),
			User:     user,
			Mounts:   mounts,
			Disks:    disks,
			Cmd:      cmd,
			Ready:    v.Ready,
			Egress:   s.Egress,
		},
		Machine: m,
		Volumes: volumes,
	}
	for k, val := range img.Labels {
		st.Labels[k] = val
	}
	for _, l := range s.Labels {
		k, val, _ := strings.Cut(l, "=")
		st.Labels[k] = val
	}
	for _, mt := range s.Mounts {
		st.Mounts = append(st.Mounts, vm.Mount{Source: mt.Source, Target: mt.Target})
	}
	if err := v.Dir.WriteState(st); err != nil {
		return err
	}
	return v.Start()
}

var numericUser = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)

// mergeEnv is base with over on top: a name over sets replaces base's in
// place, as docker run -e does the image's ENV.
func mergeEnv(base, over []string) []string {
	out := append([]string(nil), base...)
	at := map[string]int{}
	for i, e := range out {
		k, _, _ := strings.Cut(e, "=")
		at[k] = i
	}
	for _, e := range over {
		k, _, _ := strings.Cut(e, "=")
		if i, ok := at[k]; ok {
			out[i] = e
			continue
		}
		at[k] = len(out)
		out = append(out, e)
	}
	return out
}

// planShares makes the mounts directories of the VM's one share. A
// virtio-fs share is a directory, so a file is mounted from its own
// directory, shared whole (a kept file entry's is the data dir's home/,
// which holds only what is kept). A source that does not exist is made a
// directory, as docker run -v does.
func planShares(ms []Mount) ([]vm.Share, []agentproto.GuestMount, error) {
	var shares []vm.Share
	names := map[string]string{}
	var mounts []agentproto.GuestMount
	for _, m := range ms {
		if !filepath.IsAbs(m.Source) {
			return nil, nil, fmt.Errorf("mount source %q is not an absolute path", m.Source)
		}
		src := filepath.Clean(m.Source)
		fi, err := os.Stat(src)
		if errors.Is(err, os.ErrNotExist) {
			err = os.MkdirAll(src, 0o755)
			fi, _ = os.Stat(src)
		}
		if err != nil || fi == nil {
			return nil, nil, fmt.Errorf("mount source %s: %v", src, err)
		}
		dir, file := src, ""
		if !fi.IsDir() {
			dir, file = filepath.Dir(src), filepath.Base(src)
		}
		name, ok := names[dir]
		if !ok {
			name = "m" + strconv.Itoa(len(shares))
			names[dir] = name
			shares = append(shares, vm.Share{Name: name, Path: dir})
		}
		mounts = append(mounts, agentproto.GuestMount{Tag: vm.ShareTag, Path: path.Join(name, file), Target: m.Target})
	}
	return shares, mounts, nil
}

// Start boots the recorded VM: a new scratch disk, its volumes' disks
// (made empty when one was deleted while it was stopped), vmm, and then
// the boot spec to the agent. It returns once the agent has the spec, not once the
// sandbox is ready: the launcher waits for that as it does for a
// container, saying what takes long.
func (v *VM) Start() error {
	st, err := v.Dir.ReadState()
	if err != nil {
		return fmt.Errorf("no VM %s to start: %w", v.Name, err)
	}
	if v.Dir.Serving() {
		return nil
	}
	if err := v.Host.NewScratch(v.Dir.Scratch()); err != nil {
		return fmt.Errorf("the VM's scratch disk: %w", err)
	}
	// A state from before Volumes was recorded names none, and boots its
	// disks as they are.
	for i, name := range st.Volumes {
		if 2+i >= len(st.Machine.Disks) {
			break
		}
		p, err := v.Host.Volume(name)
		if err != nil {
			return fmt.Errorf("volume %s: %w", name, err)
		}
		st.Machine.Disks[2+i].Path = p
	}
	return v.Dir.Boot(st.Machine, st.Boot, v.Host.StartVMM)
}

// WaitReady waits up to timeout for the sandbox to be ready, and says why
// when it cannot be: the boot's own error, where a container can only
// time out.
func (v *VM) WaitReady(timeout time.Duration) error {
	st, err := v.Dir.ReadState()
	if err != nil {
		return err
	}
	return v.Dir.WaitReady(st.Boot, timeout)
}

// Stop asks the agent to shut the guest down, which ends vmm, and signals
// vmm when it does not end in time.
func (v *VM) Stop() error {
	if err := v.Dir.Stop(); err != nil {
		return v.say("%v", err)
	}
	return nil
}

// Remove stops the VM and deletes its directory; the image's disk, which
// other VMs may share, stays.
func (v *VM) Remove() error {
	if err := v.Stop(); err != nil {
		return err
	}
	if err := os.RemoveAll(string(v.Dir)); err != nil {
		return v.say("%v", err)
	}
	return nil
}

// Command is the exec helper (vm.ExecCommand).
func (v *VM) Command(s ExecSpec) *exec.Cmd {
	self := v.Self
	if self == "" {
		exe, err := os.Executable()
		if err != nil {
			return &exec.Cmd{Err: fmt.Errorf("cannot find this caboose to run a command in the VM: %w", err)}
		}
		self = exe
	}
	return vm.ExecCommand(self, v.Dir.Socket(), agentproto.ExecRequest{
		Argv: s.Argv, Env: s.Env, Dir: s.Dir, User: s.User, Stdin: s.Stdin, TTY: s.TTY,
	})
}

// DialLink connects to the guest's link port, where a container's link is
// a docker exec of caboose-agent link.
func (v *VM) DialLink() (io.ReadWriteCloser, error) {
	return hvsock.Dial(v.Dir.Socket(), agentproto.PortLink, 10*time.Second)
}

func (v *VM) Labels() (map[string]string, error) {
	st, err := v.Dir.ReadState()
	if err != nil {
		return nil, err
	}
	return st.Labels, nil
}

func (v *VM) Image() string {
	st, err := v.Dir.ReadState()
	if err != nil {
		return ""
	}
	return st.Image
}

func (v *VM) Mounts() ([]Mount, error) {
	st, err := v.Dir.ReadState()
	if err != nil {
		return nil, err
	}
	var out []Mount
	for _, m := range st.Mounts {
		out = append(out, Mount{Source: m.Source, Target: m.Target})
	}
	return out, nil
}

// Logs writes the console's last lines.
func (v *VM) Logs(stdout, _ io.Writer, lines int) error {
	b, err := os.ReadFile(v.Dir.Console())
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, lastLines(string(b), lines))
	return err
}

// lastLines is s's last n lines, each with its newline.
func lastLines(s string, n int) string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" || n <= 0 {
		return ""
	}
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		i = strings.LastIndexByte(s[:i], '\n')
		if i < 0 {
			i = -1
			break
		}
	}
	return s[i+1:] + "\n"
}
