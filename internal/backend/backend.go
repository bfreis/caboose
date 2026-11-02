// Package backend is what the launcher runs the sandbox on. Every touch of
// the sandbox itself -- creating it, starting and stopping it, running a
// command in it, reading what it was created with -- goes through Backend,
// so the docker and gvisor isolations (a container, under runc or runsc,
// Docker) and vm (a VM of caboose's own, VM) differ only in its
// implementation. What is not the sandbox's stays outside: images, builds
// and the engine's own questions are docker's (internal/docker), asked by
// the launcher directly.
package backend

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/docker"
)

// Backend is one sandbox, by the name it was made for.
type Backend interface {
	// State is "absent", "running", or another state a stopped sandbox
	// is in ("created", "exited", ...): anything but "running" is not.
	State() string
	// Create makes the sandbox, running, from spec.
	Create(Spec) error
	// Start starts a stopped sandbox. Its error is the backend's own
	// words, for the caller to say what to do next.
	Start() error
	// Stop stops it, showing the backend's own error on stderr.
	Stop() error
	// Remove removes it, stopped or not, showing the backend's own error
	// on stderr.
	Remove() error
	// Command is a host process, not yet started, that runs the spec's
	// command in the sandbox; its stdio are the caller's to connect. A
	// spec without Stdin gets no stdin, whatever the Cmd's is.
	Command(ExecSpec) *exec.Cmd
	// Labels are the labels the sandbox was created with (assets'
	// Label*), or an error when it cannot be asked.
	Labels() (map[string]string, error)
	// Image is the ID of the image the sandbox was created from, or ""
	// when it cannot be told.
	Image() string
	// Mounts are what the sandbox has mounted, by the host path each was
	// given as; an error means it could not be asked, typically because
	// it does not exist.
	Mounts() ([]Mount, error)
	// Logs writes the last lines of what the sandbox's entrypoint said.
	Logs(stdout, stderr io.Writer, lines int) error
}

// Starter is a Backend that can tell when the running sandbox started.
type Starter interface {
	// Started is when the sandbox last started, and false while it does
	// not run or when that cannot be told.
	Started() (time.Time, bool)
}

// Linker is a Backend whose agent serves the link on a port of its own,
// where a container's is a Command running caboose-agent link.
type Linker interface {
	DialLink() (io.ReadWriteCloser, error)
}

// Spec is everything the launcher decides about a new sandbox, built once,
// for any backend to realise its own way.
type Spec struct {
	Image    string
	Cmd      []string // the entrypoint's arguments
	Hostname string
	Env      []string // NAME=VALUE, for every process in the sandbox
	Mounts   []Mount
	// Volumes are filesystems of the sandbox's own that outlive it, kept
	// across a Remove: a named docker volume (DockerVolumeName), or under
	// vm a disk.
	Volumes []Volume
	// VolumeOwner is "UID:GID", whom the root of each of Volumes is given
	// before the sandbox starts, or "" to leave it as the backend makes
	// it. docker makes a new volume at a path its image lacks root's,
	// 0755, which the agent under runc could not write. A VM ignores it:
	// its guest runs as root.
	VolumeOwner string
	// User is who the sandbox runs as: "" for the image's own user.
	User string
	// Runtime is docker's --runtime: "" for the engine's default.
	Runtime string
	// Groups are extra groups, by host GID, for the sandbox's user.
	Groups []string
	// Labels are KEY=VALUE, recorded with the sandbox for Labels.
	Labels []string
	// RunArgs are the user's own docker run arguments (a profile's run_args),
	// after all of caboose's.
	RunArgs []string
	// Egress is where a VM's agent is to serve the outbound proxy
	// (agentproto.EgressListen) that Env points the sandbox at, so that it
	// points ssh there too at boot; "" for none, and always "" for a
	// container, whose engine dials from the host already.
	Egress string
}

// Mount is one host path mounted into the sandbox. A Mount from Mounts
// names the host path as the backend recorded it, and leaves ReadOnly
// false whatever it was created with.
type Mount struct {
	Source string // on the host
	Target string // in the sandbox
	// ReadOnly mounts it so that nothing in the sandbox can write it, its
	// root included: docker's :ro, and under vm a read-only directory of
	// the share, bound read-only in the guest. Under vm the guest's root
	// can still unmount or cover it, for the running VM only: the host's
	// files stay as they are.
	ReadOnly bool
	// Type and Volume are set only on a Mount from Mounts, as the engine
	// says them: Type "" or "bind" for a host path, "volume" for one of
	// the engine's volumes, whose name Volume is, or another of the
	// engine's types ("tmpfs", ...).
	Type, Volume string
}

// Volume is one of the sandbox's own filesystems, by name, at Target.
type Volume struct {
	Name   string
	Target string
	// Live puts a VM's volume on caboose-vmm's own disk device, whose
	// size can change while the VM runs, rather than one of the
	// framework's, which every macOS vm runs on has: caboose-vmm serves
	// one only from macOS 27 (vm.LiveDiskMacOS), and refuses to boot a
	// VM with one before. It means nothing to a container.
	Live bool
}

// DockerVolumeName is the name of the docker volume that holds volume
// vol of the container named container: container-vol
// (caboose-default-cache).
func DockerVolumeName(container, vol string) string { return container + "-" + vol }

// The labels of a docker volume caboose made for a container's volume:
// the container's name, and the volume's own name.
const (
	VolumeLabelSandbox = "dev.bfreis.caboose.sandbox"
	VolumeLabelName    = "dev.bfreis.caboose.volume"
)

// EngineVolumes is a Backend whose volumes are the engine's, which
// outlive the sandbox: listed, and deleted, by the volume's own name.
type EngineVolumes interface {
	// ListVolumes is the engine's volumes made for this sandbox, by their
	// labels, whether or not the sandbox exists or mounts them; with
	// sizes, each with the space it takes, which can take the engine a
	// while: only where it is shown.
	ListVolumes(sizes bool) ([]EngineVolume, error)
	// RemoveVolume deletes the engine's volume for volume name, and what
	// it holds; the engine refuses one a container mounts.
	RemoveVolume(name string) error
}

// EngineVolume is one of the engine's volumes made for the sandbox: the
// volume's own name, the engine's name for it, and the space it takes
// as the engine says it ("24.08kB"; "" when it does not say).
type EngineVolume struct {
	Name   string `json:"name"`
	Engine string `json:"engine"`
	Size   string `json:"size,omitempty"`
}

// ExecSpec is a command to run in the sandbox.
type ExecSpec struct {
	Argv []string
	Env  []string // NAME=VALUE, on top of the sandbox's
	Dir  string   // "" for the sandbox's default
	User string   // "" for the sandbox's own user
	// Stdin connects the command's stdin to the Cmd's; TTY gives it a
	// terminal too, which only makes sense on one.
	Stdin, TTY bool
}

// Output runs argv in the sandbox and returns its stdout with trailing
// newlines removed, as a shell command substitution would. On a failure
// the error is what the command said on stderr (a *docker.Failure).
func Output(b Backend, argv ...string) (string, error) {
	out, err := RawOutput(b, argv...)
	return strings.TrimRight(out, "\n"), err
}

// RawOutput is Output without the newline trimming.
func RawOutput(b Backend, argv ...string) (string, error) {
	return Capture(b, ExecSpec{Argv: argv}, nil)
}

// Capture runs s, with stdin (when s.Stdin) from in, and returns its
// stdout; on a failure the error carries its stderr, as Output's does.
func Capture(b Backend, s ExecSpec, in io.Reader) (string, error) {
	var out, errb bytes.Buffer
	cmd := b.Command(s)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &docker.Failure{Err: err, Stderr: strings.TrimSpace(errb.String())}
	}
	return out.String(), nil
}

// Quiet runs argv in the sandbox with its output discarded, for a command
// whose only answer is its exit status.
func Quiet(b Backend, argv ...string) error {
	return b.Command(ExecSpec{Argv: argv}).Run()
}
