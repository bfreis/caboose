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
	// across a Remove: a named docker volume, or under vm a disk.
	Volumes []Volume
	// User is who the sandbox runs as: "" for the image's own user.
	User string
	// Runtime is docker's --runtime: "" for the engine's default.
	Runtime string
	// Groups are extra groups, by host GID, for the sandbox's user.
	Groups []string
	// Labels are KEY=VALUE, recorded with the sandbox for Labels.
	Labels []string
	// RunArgs are the user's own docker run arguments (docker_run_args),
	// after all of caboose's.
	RunArgs []string
	// Egress is where a VM's agent is to serve the outbound proxy
	// (agentproto.EgressListen) that Env points the sandbox at, so that it
	// points ssh there too at boot; "" for none, and always "" for a
	// container, whose engine dials from the host already.
	Egress string
}

// Mount is one host path mounted into the sandbox. A Mount from Mounts
// names the host path as the backend recorded it.
type Mount struct {
	Source string // on the host
	Target string // in the sandbox
}

// Volume is one of the sandbox's own filesystems, by name, at Target.
type Volume struct {
	Name   string
	Target string
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
