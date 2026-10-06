package launcher

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend/backendtest"
)

// The agent where it can write its mounts; else root, told it is a sandbox.
func TestRunAs(t *testing.T) {
	if user, env := runAs(true); env != nil || user != "" {
		t.Errorf("writable: %q %q", user, env)
	}
	user, env := runAs(false)
	if !slices.Equal(env, []string{"IS_SANDBOX=1"}) || user != "0:0" {
		t.Errorf("not writable: %q %q", user, env)
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

// hasSeq reports whether seq appears in args, in a row.
func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

const withRunsc = `{"io.containerd.runc.v2":{},"runc":{},"runsc":{"path":"/usr/local/bin/runsc","runtimeArgs":["--host-uds=open"]}}`

// Doctor calls docker the weakest, says when gvisor would work, and makes
// a gvisor without runsc a problem.
func TestDoctorIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, iso, runtimes, want string
	}{
		{"docker", "docker", `{"runc":{}}`, "docker, the weakest"},
		{"docker, runsc there", "docker", withRunsc, `a gvisor profile ('caboose setup isolation') would give the sandbox a kernel of its own`},
		{"gvisor", "gvisor", withRunsc, "gvisor (runsc)"},
		{"gvisor, no runsc", "gvisor", `{"runc":{}}`, "gvisor needs docker's runsc runtime, which it does not have (it has runc)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newBoxApp(t, tc.iso, &backendtest.Fake{})
			a.write(t, "runtimes", tc.runtimes)
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

// Doctor says when caboose's runsc is old, or of a release it did not
// record, and leaves anyone else's alone; it fetches nothing.
func TestDoctorRunscRelease(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		age   time.Duration // -1: no record
		other bool
		want  string
		level level
	}{
		{"recent", 10 * 24 * time.Hour, false, "caboose's gVisor, downloaded 2026-09-", levelOK},
		{"old", 100 * 24 * time.Hour, false, "downloaded 100 days ago, and gVisor releases about weekly: caboose setup isolation checks it", levelNote},
		{"no record", -1, false, "has no readable record of its release: caboose setup isolation checks it", levelNote},
		{"not caboose's", 100 * 24 * time.Hour, true, "", levelOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "runsc", "aarch64")
			path := filepath.Join(dir, "runsc")
			if tc.other {
				path = "/usr/local/bin/runsc"
			}
			b, _ := json.Marshal(map[string]daemonRuntime{"runc": {}, "runsc": {Path: path, RuntimeArgs: runscArgs}})
			a := newBoxApp(t, isolationGVisor, &backendtest.Fake{})
			a.write(t, "runtimes", string(b))
			a.Cfg.CabooseHome, a.Cfg.Env = home, "default"
			a.Now = func() time.Time { return now }
			if tc.age >= 0 {
				writeRunscRecord(t, dir, strings.Repeat("ab", 64), now.Add(-tc.age))
			}
			c := &checkup{}
			a.doctorIsolation(c)
			got := c.String()
			if !strings.Contains(got, tc.want) || strings.Contains(got, "runsc ") != (tc.want != "") {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if tc.want != "" && c.rows[len(c.rows)-1].level != tc.level {
				t.Errorf("level = %v\n%s", c.rows[len(c.rows)-1].level, got)
			}
		})
	}
}

// A runsc without --host-uds=open cannot reach the forwarded SSH agent:
// doctor says so, with the fix for who registered it -- the daemon config
// or gVisor's install on Linux, setup for caboose's own.
func TestDoctorRunscHostUDS(t *testing.T) {
	saved := goos
	t.Cleanup(func() { goos = saved })
	goos = "linux"
	for _, tc := range []struct {
		name, path string
		args       []string
		want       string
	}{
		{"guide's install", "/usr/local/bin/runsc", nil,
			`add "--host-uds=open" to runsc's runtimeArgs in /etc/docker/daemon.json ('sudo runsc install -- --host-uds=open --net-raw --allow-packet-socket-write' writes them all)`},
		{"none", "/usr/local/bin/runsc", []string{"--host-uds=none"}, "/etc/docker/daemon.json"},
		{"caboose's", "", []string{"--dcache=0"}, "caboose setup isolation"},
		{"open", "/usr/local/bin/runsc", []string{"--host-uds", "open"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := or(tc.path, filepath.Join(home, "runsc", "x86_64", "runsc"))
			b, _ := json.Marshal(map[string]daemonRuntime{"runc": {}, "runsc": {Path: path, RuntimeArgs: tc.args}})
			a := newBoxApp(t, isolationGVisor, &backendtest.Fake{})
			a.write(t, "runtimes", string(b))
			a.Cfg.CabooseHome, a.Cfg.Env = home, "default"
			c := &checkup{}
			a.doctorIsolation(c)
			got := c.String()
			if strings.Contains(got, "runs without --host-uds=open, so the sandbox cannot reach the SSH agent") != (tc.want != "") {
				t.Errorf("got %q", got)
			}
			if tc.want != "" {
				// The row's fix, or its text for a note (no forwarded
				// agent on this machine).
				fixes := ""
				for _, r := range c.rows {
					if strings.Contains(r.text, "--host-uds=open") {
						fixes += r.text + " " + r.fix + "\n"
					}
				}
				if !strings.Contains(fixes, tc.want) {
					t.Errorf("got %q, want %q", fixes, tc.want)
				}
			}
		})
	}
}

func TestRunscHostUDS(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{runscArgs, true},
		{runscInstallArgs, true},
		{[]string{"-host-uds=all"}, true},
		{[]string{"--host-uds", "open"}, true},
		{[]string{"--host-uds=create"}, false},
		{[]string{"--host-uds=open", "--host-uds=none"}, false},
		{[]string{"--host-uds"}, false},
		{[]string{"host-uds=open"}, false},
	} {
		if got := runscHostUDS(tc.args); got != tc.want {
			t.Errorf("%q: %v", tc.args, got)
		}
	}
	// What caboose registers is what it tells others to.
	if !slices.Equal(runscArgs[:len(runscInstallArgs)], runscInstallArgs) {
		t.Errorf("runscArgs %q do not start with runscInstallArgs %q", runscArgs, runscInstallArgs)
	}
}

func TestStartFailed(t *testing.T) {
	err := startFailed("container", "box", errors.New("Error response from daemon: unknown or invalid runtime name: runsc\nfailed to start containers: box"))
	want := "container box did not start: Error response from daemon: unknown or invalid runtime name: runsc\n" +
		"       'caboose restart' recreates it; it is stopped, so no sessions are lost."
	if err.Error() != want {
		t.Errorf("got:\n%s", err)
	}
}

// The isolation picks the runtime and the user, and the sandbox is
// labelled with both: the agent under docker, and under gvisor where the
// probe finds it can write its mounts; else root, told it is a sandbox,
// which under vm it always is, with no runtime to name and nothing to
// probe.
func TestCreateContainerIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, iso, probe string
		runtime, user    bool
		userLabel        string
	}{
		{"docker", "docker", "", false, false, ""},
		{"gvisor, agent writes", "gvisor", "yes", true, false, ""},
		{"gvisor, agent cannot write", "gvisor", "no", true, true, "0:0"},
		{"vm", "vm", "", false, true, "0:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBoxApp(t, tc.iso, &backendtest.Fake{})
			if tc.iso != isolationVM {
				b.write(t, "probe-answer", tc.probe)
			}
			if err := b.createContainer(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			spec, _ := b.box.Spec()
			if got := spec.Runtime == "runsc"; got != tc.runtime || (!got && spec.Runtime != "") {
				t.Errorf("runtime %q, want runsc: %v", spec.Runtime, tc.runtime)
			}
			if got := spec.User == "0:0" && slices.Contains(spec.Env, "IS_SANDBOX=1"); got != tc.user {
				t.Errorf("root: %v, want %v\n%+v", got, tc.user, spec)
			}
			if !hasSeq(spec.Labels, assets.LabelIsolation+"="+tc.iso, assets.LabelUser+"="+tc.userLabel) {
				t.Errorf("labels missing: %q", spec.Labels)
			}
			probe, err := os.ReadFile(filepath.Join(b.engine, "probe"))
			if (err == nil) != tc.runtime {
				t.Fatalf("probe ran: %v, want %v", err == nil, tc.runtime)
			}
			if tc.runtime {
				p := strings.Split(string(probe), "\n")
				if !hasSeq(p, "--runtime", "runsc") || !hasSeq(p, "--user", "agent") ||
					!hasSeq(p, "-v", filepath.Join(b.Cfg.DataDir, "home/.claude")+":/p") {
					t.Errorf("probe: %q", p)
				}
			}
			if said := strings.Contains(b.errb.String(), "runs as root inside gVisor"); said != (tc.user && tc.iso == isolationGVisor) {
				t.Errorf("said root: %v\n%s", said, b.errb)
			}
		})
	}
}

// With no runsc, nothing is created, and the error says what to do.
func TestCreateContainerNoRuntime(t *testing.T) {
	b := newBoxApp(t, isolationGVisor, &backendtest.Fake{})
	b.write(t, "runtimes", `{"runc":{}}`)
	b.Cfg.Env = "work"
	err := b.createContainer(false)
	if err == nil || !strings.Contains(err.Error(), "no runsc runtime (it has runc): 'caboose -e work setup isolation' registers it") {
		t.Errorf("err = %v", err)
	}
	if len(b.box.Specs) != 0 {
		t.Error("created")
	}
}
