package launcher

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

func TestCheckIsolation(t *testing.T) {
	for _, iso := range []string{"", "docker", "gvisor"} {
		if err := checkIsolation(&config.Config{Isolation: iso}); err != nil {
			t.Errorf("%q: %v", iso, err)
		}
	}
	env := func(k string) string { return map[string]string{"CABOOSE_ISOLATION": "vm"}[k] }
	err := checkIsolation(&config.Config{Isolation: "vm", Getenv: env})
	if err == nil || !strings.Contains(err.Error(), `isolation "vm" is not "docker" or "gvisor" (CABOOSE_ISOLATION)`) {
		t.Errorf("err = %v", err)
	}
}

// The agent where it can write its mounts; else root, told it is a sandbox.
func TestRunAs(t *testing.T) {
	if args, user := runAs(true); args != nil || user != "" {
		t.Errorf("writable: %q %q", args, user)
	}
	args, user := runAs(false)
	if !slices.Equal(args, []string{"--user", "0:0", "-e", "IS_SANDBOX=1"}) || user != "0:0" {
		t.Errorf("not writable: %q %q", args, user)
	}
}

// The runtime decides the user, so the user's own arguments cannot set it,
// nor IS_SANDBOX, which only root under gVisor gets.
func TestRunArgsRefuseIsolation(t *testing.T) {
	for arg, want := range map[string]string{
		"--runtime=runsc":    "set by isolation",
		"--env=IS_SANDBOX=1": "caboose sets IS_SANDBOX",
	} {
		if err := checkRunArgs([]string{arg}, nil, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", arg, err)
		}
	}
}

// isolationFake is a docker whose engine has runtimes, where a probe (a
// run --rm) answers probe, and every other docker run logs its arguments,
// one a line.
func isolationFake(t *testing.T, iso, runtimes, probe string) (a *App, log string, errb *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	log = filepath.Join(tmp, "log")
	script := `#!/bin/sh
case "$1" in
  info) echo '` + runtimes + `'; exit 0 ;;
  inspect) exit 1 ;;
  image) echo '{"` + assets.LabelPlatform + `":"linux-arm64"}'; exit 0 ;;
  run) shift
    if [ "$1" = --rm ]; then printf '%s\n' "$@" > "` + log + `.probe"; echo ` + probe + `; exit 0; fi
    printf '%s\n' "$@" >> "` + log + `" ;;
esac
`
	fake := filepath.Join(tmp, "docker")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	errb = &bytes.Buffer{}
	a = &App{
		Cfg: &config.Config{Container: "box", Image: "img", Roots: []config.Root{{Host: tmp, Container: "/work"}},
			DataDir: filepath.Join(tmp, "data"), KeepVersions: "2", Getenv: func(string) string { return "" },
			Isolation: iso},
		Docker: &docker.CLI{Path: fake},
		Stdout: &bytes.Buffer{}, Stderr: errb,
	}
	return a, log, errb
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// hasSeq reports whether seq appears in args, in a row.
func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

const withRunsc = `{"io.containerd.runc.v2":{},"runc":{},"runsc":{"path":"/usr/local/bin/runsc"}}`

func TestCreateContainerIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, iso, probe string
		runtime, user    bool
		userLabel        string
	}{
		{"docker", "docker", "", false, false, ""},
		{"gvisor, agent writes", "gvisor", "yes", true, false, ""},
		{"gvisor, agent cannot write", "gvisor", "no", true, true, "0:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, log, errb := isolationFake(t, tc.iso, withRunsc, tc.probe)
			if err := a.createContainer(false); err != nil {
				t.Fatalf("%v\n%s", err, errb)
			}
			args := readArgs(t, log)
			if got := hasSeq(args, "--runtime", "runsc"); got != tc.runtime {
				t.Errorf("--runtime runsc: %v, want %v\n%q", got, tc.runtime, args)
			}
			if got := hasSeq(args, "--user", "0:0", "-e", "IS_SANDBOX=1"); got != tc.user {
				t.Errorf("root: %v, want %v\n%q", got, tc.user, args)
			}
			if !hasSeq(args, "--label", assets.LabelIsolation+"="+tc.iso, "--label", assets.LabelUser+"="+tc.userLabel) {
				t.Errorf("labels missing: %q", args)
			}
			probe, err := os.ReadFile(log + ".probe")
			if (err == nil) != tc.runtime {
				t.Fatalf("probe ran: %v, want %v", err == nil, tc.runtime)
			}
			if tc.runtime {
				p := strings.Split(string(probe), "\n")
				if !hasSeq(p, "--runtime", "runsc") || !hasSeq(p, "--user", "agent") ||
					!hasSeq(p, "-v", filepath.Join(a.Cfg.DataDir, "home/.claude")+":/p") {
					t.Errorf("probe: %q", p)
				}
			}
			if said := strings.Contains(errb.String(), "runs as root inside gVisor"); said != tc.user {
				t.Errorf("said root: %v\n%s", said, errb)
			}
		})
	}
}

// With no runsc, nothing is created, and the error says what to do.
func TestCreateContainerNoRuntime(t *testing.T) {
	a, log, _ := isolationFake(t, "gvisor", `{"runc":{}}`, "yes")
	a.Cfg.Env = "work"
	err := a.createContainer(false)
	if err == nil || !strings.Contains(err.Error(), "no runsc runtime (it has runc): 'caboose -e work setup isolation' registers it") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("docker run happened")
	}
}

func TestIsolationDrift(t *testing.T) {
	for _, tc := range []struct {
		name, labels, config, drift string
	}{
		{"no label, docker", `{}`, "docker", ""},
		{"no label, gvisor", `{}`, "gvisor", "created with isolation docker; the configuration says gvisor"},
		{"same", `{"` + assets.LabelIsolation + `":"gvisor"}`, "gvisor", ""},
		{"back to docker", `{"` + assets.LabelIsolation + `":"gvisor"}`, "", "created with isolation gvisor; the configuration says docker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '"+tc.labels+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			a := &App{Cfg: &config.Config{Container: "box", Isolation: tc.config}, Docker: &docker.CLI{Path: fake}}
			if got := a.isolationDrift(); !strings.Contains(got, tc.drift) || (tc.drift == "") != (got == "") {
				t.Errorf("drift = %q, want %q", got, tc.drift)
			}
		})
	}
}

// Doctor calls docker the weakest, says when gvisor would work, and makes
// a gvisor without runsc a problem.
func TestDoctorIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, iso, runtimes, want string
	}{
		{"docker", "docker", `{"runc":{}}`, "docker, the weakest"},
		{"docker, runsc there", "docker", withRunsc, `isolation = "gvisor" would give the sandbox a kernel of its own`},
		{"gvisor", "gvisor", withRunsc, "gvisor (runsc)"},
		{"gvisor, no runsc", "gvisor", `{"runc":{}}`, "gvisor needs docker's runsc runtime, which it does not have (it has runc)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := isolationFake(t, tc.iso, tc.runtimes, "")
			c := &checkup{}
			a.doctorIsolation(c)
			if got := c.String(); !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if problem := c.count(levelProblem) > 0; problem != strings.Contains(tc.name, "no runsc") {
				t.Errorf("problem: %v\n%s", problem, c)
			}
		})
	}
}

// stoppedUnder makes a's fake container exist, stopped, created with
// isolation iso, until a docker rm removes it.
func stoppedUnder(t *testing.T, a *App, log, iso string) {
	t.Helper()
	b, err := os.ReadFile(a.Docker.Path)
	if err != nil {
		t.Fatal(err)
	}
	gone := log + ".rm"
	s := strings.Replace(string(b), "  inspect) exit 1 ;;\n", `  inspect) [ -e "`+gone+`" ] && exit 1
    case "$*" in
      *State.Status*) echo exited ;;
      *Labels*) echo '{"`+assets.LabelIsolation+`":"`+iso+`"}' ;;
    esac
    exit 0 ;;
  rm) touch "`+gone+`"; exit 0 ;;
`, 1)
	if err := os.WriteFile(a.Docker.Path, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A stopped container created under runsc, which docker no longer has,
// can never start: it is recreated with the configured isolation, or,
// when that cannot work either, left alone with an error that says what
// to do. Doctor says so too.
func TestRuntimeGone(t *testing.T) {
	a, log, errb := isolationFake(t, "docker", `{"runc":{}}`, "")
	stoppedUnder(t, a, log, "gvisor")
	if rt := a.runtimeGone(); rt != "runsc" {
		t.Fatalf("runtimeGone = %q", rt)
	}
	c := &checkup{}
	a.doctorContainerIsolation(c)
	if got := c.String(); !strings.Contains(got, "created under runsc, which docker no longer has") || c.count(levelProblem) != 1 {
		t.Errorf("doctor: %s", got)
	}
	if err := a.recreateForRuntime("runsc", false); err != nil {
		t.Fatalf("%v\n%s", err, errb)
	}
	if _, err := os.Stat(log + ".rm"); err != nil {
		t.Error("not removed")
	}
	args := readArgs(t, log)
	if hasSeq(args, "--runtime", "runsc") || !hasSeq(args, "--label", assets.LabelIsolation+"=docker") {
		t.Errorf("created with: %q", args)
	}
	if !strings.Contains(errb.String(), "no sessions are lost: recreating it with isolation docker") {
		t.Errorf("said:\n%s", errb)
	}

	a, log, _ = isolationFake(t, "gvisor", `{"runc":{}}`, "")
	stoppedUnder(t, a, log, "gvisor")
	err := a.recreateForRuntime("runsc", false)
	if err == nil || !strings.Contains(err.Error(), "cannot start.\n") || !strings.Contains(err.Error(), "setup isolation' registers it") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(log + ".rm"); err == nil {
		t.Error("removed, with nothing to replace it")
	}

	// A container of today's runtime is not gone.
	a, log, _ = isolationFake(t, "docker", `{"runc":{}}`, "")
	stoppedUnder(t, a, log, "docker")
	if rt := a.runtimeGone(); rt != "" {
		t.Errorf("runtimeGone = %q", rt)
	}
}

func TestStartFailed(t *testing.T) {
	err := startFailed("box", errors.New("Error response from daemon: unknown or invalid runtime name: runsc\nfailed to start containers: box"))
	want := "container box did not start: Error response from daemon: unknown or invalid runtime name: runsc\n" +
		"       'caboose restart' recreates it; it is stopped, so no sessions are lost."
	if err.Error() != want {
		t.Errorf("got:\n%s", err)
	}
}
