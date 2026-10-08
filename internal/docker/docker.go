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
	"runtime"
	"strings"
)

// goos is runtime.GOOS, a variable so that tests can play the Mac.
var goos = runtime.GOOS

// hostMnt is where Docker Desktop's VM has the Mac's filesystem.
const hostMnt = "/host_mnt"

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

// Build runs docker build with args, as Stream does, with BuildKit's
// default provenance attestation off. On an engine with the containerd
// image store an image with one has an index for an ID, and the
// attestation, new on every build, gives a fully cached rebuild a new ID:
// the image reads as stale for nothing. A variable rather than
// --provenance=false, which the legacy builder refuses; a user's own
// --provenance still wins.
func (c *CLI) Build(stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command(c.Path, append([]string{"build"}, args...)...)
	cmd.Env = append(os.Environ(), "BUILDX_NO_DEFAULT_ATTESTATIONS=1")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// Load runs docker load, reading the image tarball from r, with its
// stdout and stderr connected to the given writers. It returns when docker
// exits, even one that exits without reading r to its end: a reader that
// blocks (a pipe the image is still being written into) does not hold it
// up. The copy from r then ends at r's next read or write that fails, so
// such a reader is the caller's to close.
func (c *CLI) Load(r io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command(c.Path, "load")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if f, ok := r.(*os.File); ok {
		cmd.Stdin = f
		return cmd.Run()
	}
	// exec's own copy of a reader that is not a file would keep Wait from
	// returning until that reader gave something, or ended.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdin = pr
	err = cmd.Start()
	pr.Close()
	if err != nil {
		pw.Close()
		return err
	}
	go func() {
		_, _ = io.Copy(pw, r)
		pw.Close()
	}()
	return cmd.Wait()
}

// Architecture is the engine's architecture as docker info reports it:
// the kernel's name for it, "x86_64" or "aarch64" on the machines caboose
// runs on.
func (c *CLI) Architecture() (string, error) {
	arch, err := c.Output("info", "--format", "{{.Architecture}}")
	if err != nil {
		return "", err
	}
	if arch == "" {
		return "", errors.New("docker info reports no architecture")
	}
	return arch, nil
}

// Command is docker with args, not yet started, for a caller that needs
// its pipes or its process: every exec in the sandbox (backend.Docker),
// and caboose logs, which becomes it.
func (c *CLI) Command(args ...string) *exec.Cmd { return exec.Command(c.Path, args...) }

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
// docker and podman share: an image name with an uppercase letter, say.
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
//
// On a Mac, Docker Desktop records most bind-mount sources as its VM sees
// them, /Users/u/dev as /host_mnt/Users/u/dev, and some as they were given:
// the prefix is taken off, so a source is always the Mac's path, as
// OrbStack reports it. No Mac path starts with /host_mnt (/ is read-only).
func ParseMounts(out string) []Mount {
	var ms []Mount
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		dest, src, _ := strings.Cut(line, "\t")
		if goos == "darwin" && strings.HasPrefix(src, hostMnt+"/") {
			src = strings.TrimPrefix(src, hostMnt)
		}
		ms = append(ms, Mount{Destination: dest, Source: src})
	}
	return ms
}
