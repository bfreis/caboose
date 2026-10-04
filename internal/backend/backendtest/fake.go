// Package backendtest is an in-memory backend.Backend for the launcher's
// tests: a sandbox with a state, the Spec it was created from, labels,
// mounts and a log, whose commands answer as a test scripts them. It
// stands for a container or a VM alike, so one scenario can run under
// every isolation.
package backendtest

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/bfreis/caboose/internal/backend"
)

// ErrAbsent is what a call about the sandbox returns while there is none,
// as an inspect of a missing container fails.
var ErrAbsent = errors.New("no such sandbox")

// Fake is a sandbox in memory. Its zero value is absent; set Status (and
// SandboxLabels, SandboxMounts, ImageID) for one that exists already. The
// exported fields may be changed between calls, and from Exec's function.
type Fake struct {
	mu sync.Mutex

	// Status is State's answer while the sandbox exists: "running",
	// "exited", ...; "" is "absent".
	Status string
	// SandboxLabels and SandboxMounts are what the sandbox has, set by
	// Create; Labels and Mounts fail while it is absent.
	SandboxLabels map[string]string
	SandboxMounts []backend.Mount
	// ImageID is Image's answer while the sandbox exists.
	ImageID string
	// ImageFor, when set, is the image a Spec's Image names: its ID, which
	// Create sets ImageID to, and its labels, which the sandbox has
	// beneath the Spec's, as a container or a VM inherits them. Unset,
	// ImageID is the name, and the image has no labels.
	ImageFor func(name string) (id string, labels map[string]string)
	// Log is what the sandbox's entrypoint said; Logs writes its last
	// lines.
	Log string

	// CreateErr, StartErr, StopErr and RemoveErr fail those calls, which
	// then change nothing.
	CreateErr, StartErr, StopErr, RemoveErr error
	// OnCreate, when set, runs at Create before anything changes, and its
	// error fails it: the moment a backend would realise the Spec.
	OnCreate func(backend.Spec) error

	// Exec answers each Command; nil, or a nil *exec.Cmd from it, is
	// success with no output (OK). It runs without the Fake's lock held.
	Exec func(backend.ExecSpec) *exec.Cmd

	// Specs are the Specs Create was given, in order.
	Specs []backend.Spec
	// Execs are the ExecSpecs Command was given, in order.
	Execs []backend.ExecSpec
	// Calls are the lifecycle calls made, in order: "create", "start",
	// "stop", "remove", and "logs N".
	Calls []string
}

var _ backend.Backend = (*Fake)(nil)

func (f *Fake) exists() bool { return f.Status != "" && f.Status != "absent" }

func (f *Fake) State() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists() {
		return "absent"
	}
	return f.Status
}

// Create records s; unless it fails, the sandbox then runs, with s's
// mounts and its image's labels and s's.
func (f *Fake) Create(s backend.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "create")
	f.Specs = append(f.Specs, s)
	switch {
	case f.exists():
		return fmt.Errorf("a sandbox exists already (%s)", f.Status)
	case f.CreateErr != nil:
		return f.CreateErr
	}
	if f.OnCreate != nil {
		if err := f.OnCreate(s); err != nil {
			return err
		}
	}
	id, labels := s.Image, map[string]string{}
	if f.ImageFor != nil {
		var il map[string]string
		id, il = f.ImageFor(s.Image)
		maps.Copy(labels, il)
	}
	for _, l := range s.Labels {
		k, v, _ := strings.Cut(l, "=")
		labels[k] = v
	}
	f.Status, f.SandboxLabels, f.ImageID = "running", labels, id
	f.SandboxMounts = slices.Clone(s.Mounts)
	return nil
}

// Spec is the last Spec Create was given, and whether there was one.
func (f *Fake) Spec() (backend.Spec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Specs) == 0 {
		return backend.Spec{}, false
	}
	return f.Specs[len(f.Specs)-1], true
}

// lifecycle records call, and fails it with ErrAbsent or fail; else it
// sets the state to to.
func (f *Fake) lifecycle(call string, fail error, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, call)
	switch {
	case !f.exists():
		return ErrAbsent
	case fail != nil:
		return fail
	}
	f.Status = to
	if to == "absent" {
		f.SandboxLabels, f.SandboxMounts, f.ImageID = nil, nil, ""
	}
	return nil
}

func (f *Fake) Start() error  { return f.lifecycle("start", f.StartErr, "running") }
func (f *Fake) Stop() error   { return f.lifecycle("stop", f.StopErr, "exited") }
func (f *Fake) Remove() error { return f.lifecycle("remove", f.RemoveErr, "absent") }

// Command is Exec's answer to s, recorded.
func (f *Fake) Command(s backend.ExecSpec) *exec.Cmd {
	f.mu.Lock()
	f.Execs = append(f.Execs, s)
	h := f.Exec
	f.mu.Unlock()
	var cmd *exec.Cmd
	if h != nil {
		cmd = h(s)
	}
	if cmd == nil {
		cmd = OK()
	}
	return cmd
}

// Ran reports whether a command whose argv starts with prefix was run.
func (f *Fake) Ran(prefix ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.Execs {
		if len(s.Argv) >= len(prefix) && slices.Equal(s.Argv[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func (f *Fake) Labels() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists() {
		return nil, ErrAbsent
	}
	return maps.Clone(f.SandboxLabels), nil
}

func (f *Fake) Image() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists() {
		return ""
	}
	return f.ImageID
}

func (f *Fake) Mounts() ([]backend.Mount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists() {
		return nil, ErrAbsent
	}
	return slices.Clone(f.SandboxMounts), nil
}

// Logs writes Log's last lines lines to stdout.
func (f *Fake) Logs(stdout, _ io.Writer, lines int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "logs "+strconv.Itoa(lines))
	if !f.exists() {
		return ErrAbsent
	}
	ls := strings.SplitAfter(f.Log, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	_, err := io.WriteString(stdout, strings.Join(ls[max(0, len(ls)-lines):], ""))
	return err
}

// Reply is a command that prints stdout and stderr, and exits with code.
func Reply(stdout, stderr string, code int) *exec.Cmd {
	return exec.Command("sh", "-c", `printf '%s' "$1"; printf '%s' "$2" >&2; exit "$3"`,
		"reply", stdout, stderr, strconv.Itoa(code))
}

// OK is a command that succeeds with no output.
func OK() *exec.Cmd { return Reply("", "", 0) }

// Fail is a command that fails, with status 1 and no output.
func Fail() *exec.Cmd { return Reply("", "", 1) }
