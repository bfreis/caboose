package launcher

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// What caboose's own arguments look like, as createContainer builds them.
var testOwnArgs = []string{
	"--name", "box", "-e", "TINI_KILL_PROCESS_GROUP=1", "-e", "SSH_AUTH_SOCK=/run/agent.sock",
	"-v", "/d/home/.claude:/home/agent/.claude", "-v", "/sock:/run/agent.sock",
	"-v", "/d/local/bin:/home/agent/.local/bin",
}

func TestCheckRunArgs(t *testing.T) {
	roots := []config.Root{{Name: "dev", Host: "/h/dev", Container: "/work/dev"}}
	for _, arg := range []string{
		"--cap-add=NET_ADMIN", "--device=/dev/net/tun", "--dns=100.100.100.100", "--privileged",
		"--read-only", "--add-host=box.lan=10.0.0.2", "--env=FOO=bar", "--env=FOO", "--label=com.example.a=b",
		"--volume=/h/cache:/cache", "--volume=/data", "--volume=vol:/home/agent/.cache/pip:ro",
		"--mount=type=bind,src=/h/x,dst=/opt/x", "--tmpfs=/scratch:size=64m", "--network=host",
		"--volume=relative", "--shm-size=1g",
	} {
		if err := checkRunArgs([]string{arg}, testOwnArgs, roots); err != nil {
			t.Errorf("%s: refused: %v", arg, err)
		}
	}
	for arg, want := range map[string]string{
		"-e":                                  "short flag; write the long one, --env=",
		"-X":                                  "short flag; write the long one, --flag=value",
		"NET_ADMIN":                           "not a flag",
		"--":                                  "not a flag",
		"--cap-add":                           "no value; write it as --cap-add=VALUE",
		"--name=other":                        "caboose's own: caboose names the container",
		"--user=root":                         "caboose's own",
		"--entrypoint=/bin/sh":                "caboose's own",
		"--init=false":                        "caboose's own",
		"--rm":                                "caboose's own",
		"--env-file=/x":                       "caboose's own",
		"--env=HOME=/root":                    "caboose sets HOME itself",
		"--env=CABOOSE_ANY=1":                 "caboose sets CABOOSE_ANY itself",
		"--env=TINI_VERBOSITY=3":              "caboose sets TINI_VERBOSITY itself",
		"--env=SSH_AUTH_SOCK=/x":              "caboose sets SSH_AUTH_SOCK itself",
		"--label=" + labelPrefix + "compat=9": "caboose's labels are its own",
		"--volume=/x:/work":                   "overlaps /work",
		"--volume=/x:/work/other":             "overlaps /work",
		"--volume=/x:/":                       "would hide the agent's home",
		"--volume=/x:/home/agent/.claude/y":   "overlaps /home/agent/.claude",
		"--volume=/x:/home/agent/.local":      "overlaps /home/agent/.local/bin",
		"--mount=type=bind,src=/x,target=/run/agent.sock":         "overlaps /run/agent.sock",
		"--tmpfs=/home/agent/.claude":                             "overlaps /home/agent/.claude",
		"--volume=/x:/home":                                       "would hide the agent's home",
		"--volume=/x:/home/agent/":                                "would hide the agent's home",
		"--volume=/x:/etc/claude-code:ro":                         "overlaps /etc/claude-code",
		"--mount=type=bind,src=/x,dst=/etc/claude-code/CLAUDE.md": "overlaps /etc/claude-code",
		"--tmpfs=/etc":                                            "overlaps /etc/claude-code",
	} {
		err := checkRunArgs([]string{"--dns=1.1.1.1", arg}, testOwnArgs, roots)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "'"+arg+"'") {
			t.Errorf("%s: err = %v, want %q", arg, err, want)
		}
	}
	// With no own arguments, the roots and what never depends on them.
	if err := checkRunArgs([]string{"--volume=/x:/home/agent/.claude"}, nil, roots); err != nil {
		t.Errorf("without own arguments: %v", err)
	}
	if err := checkRunArgs([]string{"--volume=/x:/work/dev/y"}, nil, roots); err == nil {
		t.Error("a mount inside a root: not refused")
	}
	if err := checkRunArgs([]string{"--volume=/x:/etc/claude-code"}, nil, roots); err == nil {
		t.Error("a mount over caboose's instructions, without own arguments: not refused")
	}
}

func TestWideningRunArgs(t *testing.T) {
	got := wideningRunArgs([]string{"--cap-add=NET_ADMIN", "--dns=1.1.1.1", "--network=host", "--network=bridge", "--privileged", "--volume=/a:/b"})
	want := []string{"--cap-add=NET_ADMIN", "--network=host", "--privileged", "--volume=/a:/b"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// runFake is a docker for creating a container from image img, which logs
// every argument of `docker run`, one a line.
func runFake(t *testing.T, runArgs []string) (a *App, log string, errb *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	log = filepath.Join(tmp, "log")
	script := `#!/bin/sh
case "$1" in
  inspect) exit 1 ;;
  image) echo '{"` + assets.LabelPlatform + `":"linux-arm64"}'; exit 0 ;;
  run) shift; printf '%s\n' "$@" >> "` + log + `" ;;
esac
`
	fake := filepath.Join(tmp, "docker")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	errb = &bytes.Buffer{}
	a = &App{
		Cfg: &config.Config{Container: "box", Image: "img", Roots: []config.Root{{Host: tmp, Container: "/work"}},
			DataDir: filepath.Join(tmp, "data"), KeepVersions: 2, Getenv: func(string) string { return "" },
			RunArgs: runArgs, Profile: "container.default",
			File: &config.File{Path: "/e/config.toml", Vals: map[string]any{"container.default.run_args": runArgs}}},
		Docker: &docker.CLI{Path: fake},
		Stdout: &bytes.Buffer{}, Stderr: errb,
	}
	return a, log, errb
}

// The user's arguments go in after caboose's, right before the image, and
// the container is labelled with them -- with nothing, when there are none.
func TestCreateContainerRunArgs(t *testing.T) {
	for _, user := range [][]string{nil, {"--cap-add=NET_ADMIN", "--device=/dev/net/tun"}} {
		a, log, errb := runFake(t, user)
		if err := a.createContainer(false); err != nil {
			t.Fatalf("%v\n%s", err, errb)
		}
		b, _ := os.ReadFile(log)
		args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		img := slices.Index(args, "img")
		if img < 0 || len(args) != img+2 {
			t.Fatalf("args %q", args)
		}
		label := "--label\n" + assets.LabelRunArgs + "=" + runArgsLabel(user)
		want := strings.Join(append(strings.Split(label, "\n"), user...), "\n")
		if got := strings.Join(args[img-2-len(user):img], "\n"); got != want {
			t.Errorf("before the image:\n%s\nwant\n%s", got, want)
		}
		if len(user) > 0 && !strings.Contains(errb.String(), "--cap-add=NET_ADMIN --device=/dev/net/tun (run_args in [container.default] of /e/config.toml)") {
			t.Errorf("not said: %s", errb)
		}
	}
}

// A refused argument creates nothing, and says where it came from.
func TestCreateContainerRefusesRunArgs(t *testing.T) {
	for _, arg := range []string{"--name=x", "--volume=/x:/home/agent/.claude", "--volume=/x:/etc/claude-code"} {
		a, log, _ := runFake(t, []string{arg})
		err := a.createContainer(false)
		if err == nil || !strings.Contains(err.Error(), "'"+arg+"'") || !strings.Contains(err.Error(), "(run_args in [container.default] of /e/config.toml)") {
			t.Errorf("%s: err = %v", arg, err)
		}
		if _, err := os.Stat(log); err == nil {
			t.Errorf("%s: docker run happened", arg)
		}
	}
}

// The container's label against the configuration: an empty label is no
// arguments, and no label at all is not this caboose's container.
func TestRunArgsDrift(t *testing.T) {
	label := func(v string) map[string]string { return map[string]string{assets.LabelRunArgs: v} }
	for _, tc := range []struct {
		name   string
		labels map[string]string
		config []string
		drift  string
	}{
		{"no label", map[string]string{}, nil, "records no docker run arguments"},
		{"no labels at all", nil, nil, "records no docker run arguments"},
		{"empty label, none", label(""), nil, ""},
		{"same", label(`["--a=1","--b"]`), []string{"--a=1", "--b"}, ""},
		{"added", label(""), []string{"--a=1"}, "created with docker run arguments none; the configuration says --a=1"},
		{"removed", label(`["--a=1"]`), nil, "created with docker run arguments --a=1; the configuration says none"},
		{"unreadable", label("nope"), []string{"--a=1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := &backendtest.Fake{Status: "running", SandboxLabels: tc.labels}
			a := &App{Cfg: &config.Config{Container: "box", RunArgs: tc.config}, Backend: box}
			if got := a.runArgsDrift(); !strings.Contains(got, tc.drift) || (tc.drift == "") != (got == "") {
				t.Errorf("drift %q, want %q", got, tc.drift)
			}
		})
	}
}
