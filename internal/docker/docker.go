// Package docker is a thin wrapper over the docker CLI.
//
// The CLI, not the Docker SDK: it respects docker contexts (OrbStack, colima,
// Docker Desktop, podman's docker shim), and interactive `docker exec -it`
// with TTY handling is painful through the SDK. Every call returns an error
// for its caller to act on: nothing dies silently halfway through.
package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// CLI runs the docker binary found at Path ("docker" resolves via $PATH).
type CLI struct {
	Path string
	// Stderr receives docker's own stderr on the calls that show it; nil
	// means os.Stderr.
	Stderr io.Writer
}

// New returns a CLI for the docker on $PATH.
func New() *CLI { return &CLI{Path: "docker"} }

func (c *CLI) stderr() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

// Output runs docker and returns its stdout with trailing newlines removed,
// as a shell command substitution would. docker's stderr is not shown; on a
// failure it is the error's text instead (see Failure).
func (c *CLI) Output(args ...string) (string, error) {
	out, err := c.RawOutput(args...)
	return strings.TrimRight(out, "\n"), err
}

// RawOutput is Output without the newline trimming.
func (c *CLI) RawOutput(args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command(c.Path, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &Failure{Err: err, Stderr: strings.TrimSpace(errb.String())}
	}
	return out.String(), nil
}

// Failure is a docker run that failed, with what it said on stderr: a bare
// "exit status 1" explains nothing to someone reading it in a caboose
// message. It unwraps to the underlying error, so ExitCode and errors.As on
// *exec.ExitError see through it.
type Failure struct {
	Err    error
	Stderr string
}

func (f *Failure) Error() string {
	if f.Stderr != "" {
		return f.Stderr
	}
	return f.Err.Error()
}

func (f *Failure) Unwrap() error { return f.Err }

// Quiet runs docker with both stdout and stderr discarded, for calls whose
// only answer is the exit status.
func (c *CLI) Quiet(args ...string) error {
	return exec.Command(c.Path, args...).Run()
}

// Run runs docker with stdout discarded (`>/dev/null`) and stderr shown, so a
// failure still explains itself.
func (c *CLI) Run(args ...string) error {
	cmd := exec.Command(c.Path, args...)
	cmd.Stderr = c.stderr()
	return cmd.Run()
}

// Stream runs docker with stdout and stderr connected to the given writers
// and no stdin.
func (c *CLI) Stream(stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command(c.Path, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// Pipe runs docker with input on its stdin and all output discarded.
func (c *CLI) Pipe(input []byte, args ...string) error {
	cmd := exec.Command(c.Path, args...)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Run()
}

// Exec replaces the current process with docker, so signals, the TTY and
// the exit code belong to docker and not to a Go parent process. It returns
// only on failure.
func (c *CLI) Exec(args ...string) error {
	path, err := exec.LookPath(c.Path)
	if err != nil {
		return err
	}
	return syscall.Exec(path, append([]string{"docker"}, args...), os.Environ())
}

// ExitCode is the exit status of a failed docker run, or 1 when it did not
// get as far as exiting (docker missing, say).
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ee.ExitCode()
	}
	return 1
}

// ContainerState is the container's .State.Status, or "absent".
//
// Note the explicit --type: the container and the image share the name
// 'caboose', and a bare `docker inspect` resolves the image, then prints a
// template error to stdout that pollutes the result.
func (c *CLI) ContainerState(name string) string {
	status, err := c.Output("inspect", "--type=container", "-f", "{{.State.Status}}", name)
	if err != nil || status == "" {
		return "absent"
	}
	return status
}

// ContainerExists reports whether a container by that name exists.
func (c *CLI) ContainerExists(name string) bool {
	return c.Quiet("inspect", "--type=container", name) == nil
}

// ContainerImage is the ID of the image the container was created from, or
// "" if it cannot be read.
func (c *CLI) ContainerImage(name string) string {
	id, _ := c.Output("inspect", "--type=container", "-f", "{{.Image}}", name)
	return id
}

// ContainerLabels are the container's labels -- its image's, as it was
// created from it, with any given at docker run -- or an error when it
// cannot be inspected.
func (c *CLI) ContainerLabels(name string) (map[string]string, error) {
	out, err := c.Output("inspect", "--type=container", "-f", "{{json .Config.Labels}}", name)
	if err != nil {
		return nil, err
	}
	return ParseLabels(out)
}

// ImageID is the ID the image tag points at now, or "".
func (c *CLI) ImageID(name string) string {
	id, _ := c.Output("inspect", "--type=image", "-f", "{{.Id}}", name)
	return id
}

// ImageLabels reads the image's labels. exists is false, with a nil error,
// when the image is simply not in the local store; an error means docker
// could not answer about it -- IsUnreachable tells when that is because the
// engine is down, so a caller deciding whether to build does not mistake an
// unreachable daemon for a missing image. An image with no labels gives an
// empty, non-nil map.
//
// When inspect fails, a second call decides why: `docker version` exits
// non-zero exactly when the client cannot reach the engine, in docker and
// in podman's shim alike (a --format naming .Server fields would not: local
// podman has none). An engine that answers that but not the inspect has no
// such image. That is not decided by matching "No such image" on stderr,
// whose wording differs between docker and its look-alikes, and not by
// `docker images -q REF` either: that lists every tag of the repository, so
// with only caboose:backup present it called caboose (:latest) there, and a
// launch died over "docker engine not running" instead of building.
//
// The one stderr that is matched is "invalid reference format", which
// docker and podman share: a CABOOSE_IMAGE with an uppercase letter, say.
// That fails every inspect against a perfectly healthy engine, and calling
// it absent would start a multi-minute setup only for `docker build -t` to
// reject the name; it is an error, with docker's own words, instead.
func (c *CLI) ImageLabels(ref string) (labels map[string]string, exists bool, err error) {
	out, err := c.Output("image", "inspect", "--format", "{{json .Config.Labels}}", ref)
	if err != nil {
		if strings.Contains(err.Error(), "invalid reference format") {
			return nil, false, fmt.Errorf("docker image inspect %s: %w", ref, err)
		}
		if c.Quiet("version") != nil {
			return nil, false, unreachable{fmt.Errorf("docker image inspect %s: %w", ref, err)}
		}
		return nil, false, nil
	}
	labels, err = ParseLabels(out)
	if err != nil {
		return nil, true, fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
	return labels, true, nil
}

// unreachable marks an error as the engine not answering at all.
type unreachable struct{ error }

func (u unreachable) Unwrap() error { return u.error }

// Unreachable marks err as the engine not answering, for IsUnreachable: for
// callers outside this package that decided so the way ImageLabels does,
// with a failing `docker version` after a failed call.
func Unreachable(err error) error { return unreachable{err} }

// IsUnreachable reports whether err, from ImageLabels (or marked by
// Unreachable), means the docker engine could not be reached, as opposed to
// it rejecting the request.
func IsUnreachable(err error) bool {
	var u unreachable
	return errors.As(err, &u)
}

// ParseLabels parses the JSON of an image's .Config.Labels, which docker
// prints as "null" for an image built without any.
func ParseLabels(out string) (map[string]string, error) {
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(out), &labels); err != nil {
		return nil, fmt.Errorf("parsing labels %q: %w", out, err)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	return labels, nil
}

// Mount is one bind mount of a container.
type Mount struct{ Destination, Source string }

// Mounts lists the container's mounts; an error means it could not be
// inspected, typically because it does not exist yet.
func (c *CLI) Mounts(name string) ([]Mount, error) {
	out, err := c.Output("inspect", "--type=container", name,
		"--format", `{{range .Mounts}}{{.Destination}}{{"\t"}}{{.Source}}{{"\n"}}{{end}}`)
	if err != nil {
		return nil, err
	}
	return ParseMounts(out), nil
}

// ParseMounts parses "destination<TAB>source" lines.
func ParseMounts(out string) []Mount {
	var ms []Mount
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		dest, src, _ := strings.Cut(line, "\t")
		ms = append(ms, Mount{Destination: dest, Source: src})
	}
	return ms
}
