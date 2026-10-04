package backendtest

import (
	"bytes"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
)

// The fake keeps a backend's contract: absent until created, labelled
// with its image's labels beneath the Spec's, and gone, labels and all,
// once removed.
func TestFakeLifecycle(t *testing.T) {
	f := &Fake{ImageFor: func(name string) (string, map[string]string) {
		return "sha256:" + name, map[string]string{"a": "image", "b": "image"}
	}}
	if f.State() != "absent" || f.Image() != "" {
		t.Errorf("zero value: %s %q", f.State(), f.Image())
	}
	if _, err := f.Labels(); !errors.Is(err, ErrAbsent) {
		t.Errorf("labels of none: %v", err)
	}
	if err := f.Start(); !errors.Is(err, ErrAbsent) {
		t.Errorf("start of none: %v", err)
	}
	spec := backend.Spec{Image: "img", Labels: []string{"b=spec", "c=x=y"}, Mounts: []backend.Mount{{Source: "/h", Target: "/work"}}}
	if err := f.Create(spec); err != nil {
		t.Fatal(err)
	}
	if err := f.Create(spec); err == nil {
		t.Error("created twice")
	}
	labels, _ := f.Labels()
	mounts, _ := f.Mounts()
	if f.State() != "running" || f.Image() != "sha256:img" || labels["a"] != "image" || labels["b"] != "spec" || labels["c"] != "x=y" ||
		!slices.Equal(mounts, spec.Mounts) {
		t.Errorf("created: %s %q %v %v", f.State(), f.Image(), labels, mounts)
	}
	f.StopErr = errors.New("no")
	if err := f.Stop(); err == nil || f.State() != "running" {
		t.Errorf("failed stop: %v, %s", err, f.State())
	}
	f.StopErr = nil
	if err := f.Stop(); err != nil || f.State() != "exited" {
		t.Errorf("stop: %v, %s", err, f.State())
	}
	if err := f.Remove(); err != nil || f.State() != "absent" || f.Image() != "" {
		t.Errorf("remove: %v, %s", err, f.State())
	}
	if want := []string{"start", "create", "create", "stop", "stop", "remove"}; !slices.Equal(f.Calls, want) {
		t.Errorf("calls %q, want %q", f.Calls, want)
	}
}

// Commands answer as scripted, and are recorded; the log is tailed.
func TestFakeCommandsAndLogs(t *testing.T) {
	f := &Fake{Status: "running", Log: "1\n2\n3\n", Exec: func(s backend.ExecSpec) *exec.Cmd {
		if s.Argv[0] == "fail" {
			return Reply("out", "why", 3)
		}
		return nil
	}}
	out, err := backend.Output(f, "fail")
	if out != "out" || err == nil || !strings.Contains(err.Error(), "why") {
		t.Errorf("fail: %q, %v", out, err)
	}
	if err := backend.Quiet(f, "true"); err != nil {
		t.Errorf("default: %v", err)
	}
	if !f.Ran("fail") || f.Ran("other") {
		t.Errorf("execs %+v", f.Execs)
	}
	var b bytes.Buffer
	if err := f.Logs(&b, nil, 2); err != nil || b.String() != "2\n3\n" {
		t.Errorf("logs: %q, %v", b.String(), err)
	}
}
