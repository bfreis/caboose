package backend

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/docker"
)

// echoDocker is a docker that prints its arguments, one a line, and fails
// with "no" on stderr when the last one is "fail".
func echoDocker(t *testing.T) *Docker {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
for a; do printf '%s\n' "$a"; last=$a; done
[ "$last" = fail ] && { echo no >&2; exit 3; }
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewDocker(&docker.CLI{Path: p}, "box")
}

func argv(t *testing.T, d *Docker, s ExecSpec) []string {
	t.Helper()
	out, err := Capture(d, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// Each ExecSpec is the docker exec it always was: flags before the
// container, the command after it.
func TestDockerCommand(t *testing.T) {
	d := echoDocker(t)
	for _, tc := range []struct {
		s    ExecSpec
		want []string
	}{
		{ExecSpec{Argv: []string{"tmux", "ls"}}, []string{"exec", "box", "tmux", "ls"}},
		{ExecSpec{Argv: []string{"chmod", "0666", "/s"}, User: "0"}, []string{"exec", "-u", "0", "box", "chmod", "0666", "/s"}},
		{ExecSpec{Argv: []string{"bash"}, Env: []string{"TERM=xterm", "TZ=UTC"}, Dir: "/work/p", Stdin: true, TTY: true},
			[]string{"exec", "-i", "-t", "-e", "TERM=xterm", "-e", "TZ=UTC", "-w", "/work/p", "box", "bash"}},
		{ExecSpec{Argv: []string{"agent", "link"}, Stdin: true}, []string{"exec", "-i", "box", "agent", "link"}},
	} {
		if got := argv(t, d, tc.s); !slices.Equal(got, tc.want) {
			t.Errorf("%+v:\n got %q\nwant %q", tc.s, got, tc.want)
		}
	}
}

// A failure says what the command said on stderr, and keeps its status.
func TestCaptureFailure(t *testing.T) {
	_, err := Output(echoDocker(t), "fail")
	var f *docker.Failure
	if !errors.As(err, &f) || f.Error() != "no" || docker.ExitCode(err) != 3 {
		t.Errorf("err = %v", err)
	}
	if Quiet(echoDocker(t), "fail") == nil || Quiet(echoDocker(t), "ok") != nil {
		t.Error("Quiet does not tell success from failure")
	}
}

// The user's own arguments come after all of caboose's, where docker takes
// the last of a single-value flag, and the image and its command last.
func TestRunArgv(t *testing.T) {
	got := RunArgv("box", Spec{
		Image: "img", Cmd: []string{"--cc-supervise"}, Hostname: "caboose",
		Env:     []string{"A=1"},
		Mounts:  []Mount{{Source: "/h", Target: "/work"}, {Source: "/m", Target: "/etc/m", ReadOnly: true}},
		Volumes: []Volume{{Name: "docker", Target: "/var/lib/docker"}},
		Groups:  []string{"999"},
		Runtime: "runsc", User: "0:0",
		Labels:  []string{"k=v"},
		RunArgs: []string{"--cpus=2"},
	})
	want := []string{"run", "-d", "--name", "box", "--hostname", "caboose", "--restart", "unless-stopped", "--init",
		"-e", "A=1", "-v", "/h:/work", "-v", "/m:/etc/m:ro", "-v", "box-docker:/var/lib/docker", "--group-add", "999", "--runtime", "runsc", "--user", "0:0",
		"--label", "k=v", "--cpus=2", "img", "--cc-supervise"}
	if !slices.Equal(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if got := RunArgv("box", Spec{Image: "img", Hostname: "h"}); slices.Contains(got, "--runtime") || slices.Contains(got, "--user") {
		t.Errorf("defaults spelled out: %q", got)
	}
}

// A container has no outbound proxy of caboose's: a Spec with one is
// refused before docker runs.
func TestDockerRefusesEgress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch \"$0.ran\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewDocker(&docker.CLI{Path: p}, "box")
	if err := d.Create(Spec{Image: "img", Egress: "127.0.0.1:9128"}); err == nil || !strings.Contains(err.Error(), "outbound proxy") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(p + ".ran"); err == nil {
		t.Error("docker ran")
	}
}
