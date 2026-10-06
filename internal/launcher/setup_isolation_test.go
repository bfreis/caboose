package launcher

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
)

// isolationEngine is a setup environment on an engine that says it is
// engine (as docker info's OperatingSystem), whose runtimes gain runsc once
// the fake orb restarts it with runsc registered, or from the start with
// runsc set: caboose's download, with this caboose's flags, in docker.json
// too. What the engine
// has loaded is the file loaded (docker info's Runtimes), which a restart
// rewrites from docker.json; setLoaded changes it. image says whether the
// image exists; a container run under runsc answers probe. The machine is
// goos.
type isolationEngine struct {
	*setupEnv
	dir, daemon string
}

func newIsolationEngine(t *testing.T, goosIs, engine string, runsc, image bool, probe string) *isolationEngine {
	t.Helper()
	e := newSetupEnv(t, "default", "", false)
	dir := t.TempDir()
	touch := func(name string, on bool) {
		if on {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	touch("image", image)
	docker := `#!/bin/sh
d='` + dir + `'
case "$1 $2 $3" in
  "info --format {{json .Runtimes}}")
    if [ -e "$d/loaded" ]; then cat "$d/loaded"; else echo '{"runc":{}}'; fi; exit 0 ;;
  "info --format {{.OperatingSystem}}") echo '` + engine + `'; exit 0 ;;
  "info --format {{.Architecture}}") echo aarch64; exit 0 ;;
esac
case "$1" in
  image) [ -e "$d/image" ] && { echo '{}'; exit 0; }; exit 1 ;;
  version) exit 0 ;;
  run) printf '%s\n' "$@" > "$d/probe"; echo ` + probe + `; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(e.a.Docker.Path, []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(e.a.Cfg.Home, ".orbstack", "config", "docker.json")
	// orb restarts the engine: it loads the runtimes in docker.json, as
	// setup writes them (caboose's runsc with this caboose's flags), and
	// the restart is recorded.
	withRunsc := runtimesJSON(t, runscEntry(filepath.Join(e.a.Cfg.CabooseHome, "runsc", "aarch64", "runsc")))
	orb := filepath.Join(dir, "orb")
	script := "#!/bin/sh\n[ \"$*\" = 'restart docker' ] || exit 2\ntouch '" + dir + "/restarted'\n" +
		"if grep -q '\"runsc\"' '" + daemon + "'; then echo '" + withRunsc + "'; else echo '{\"runc\":{}}'; fi > '" + dir + "/loaded'\n"
	if err := os.WriteFile(orb, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if runsc {
		if err := os.WriteFile(filepath.Join(dir, "loaded"), []byte(withRunsc), 0o644); err != nil {
			t.Fatal(err)
		}
		data, _, err := withRuntime(nil, runscName, runscEntry(filepath.Join(e.a.Cfg.CabooseHome, "runsc", "aarch64", "runsc")))
		if err == nil {
			err = os.MkdirAll(filepath.Dir(daemon), 0o755)
		}
		if err == nil {
			err = os.WriteFile(daemon, data, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		// The release downloaded is the fake server's latest.
		writeRunscRecord(t, filepath.Join(e.a.Cfg.CabooseHome, "runsc", "aarch64"), releaseSum(t, releaseGood), time.Now())
	}
	srv := runscServer(t, "aarch64", releaseGood, "")
	saved := []any{goos, orbCommand, runscReleases, engineWait, enginePoll}
	goos, orbCommand, runscReleases, engineWait, enginePoll = goosIs, orb, srv.URL, time.Second, 10*time.Millisecond
	t.Cleanup(func() {
		goos, orbCommand, runscReleases = saved[0].(string), saved[1].(string), saved[2].(string)
		engineWait, enginePoll = saved[3].(time.Duration), saved[4].(time.Duration)
	})
	return &isolationEngine{setupEnv: e, dir: dir, daemon: daemon}
}

// releaseSum is the sha512 of a test release (base64).
func releaseSum(t *testing.T, release string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(release)
	if err != nil {
		t.Fatal(err)
	}
	h := sha512.Sum512(b)
	return hex.EncodeToString(h[:])
}

// writeRunscRecord puts a runsc and its record, of the release with sum
// downloaded at when, in dir.
func writeRunscRecord(t *testing.T, dir, sum string, when time.Time) {
	t.Helper()
	b, _ := json.Marshal(runscRelease{SHA512: sum, URL: "https://example.com/gvisor.tar.bz2", Downloaded: when.UTC()})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runscName), []byte("old runsc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runscRecord), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// runtimesJSON is docker info's Runtimes with runc and, as runsc, rt.
func runtimesJSON(t *testing.T, rt daemonRuntime) string {
	t.Helper()
	b, err := json.Marshal(map[string]daemonRuntime{"runc": {}, runscName: rt})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// setLoaded makes the engine's runsc rt, as if loaded at its last restart.
func (e *isolationEngine) setLoaded(rt daemonRuntime) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, "loaded"), []byte(runtimesJSON(e.t, rt)), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *isolationEngine) restarted() bool {
	_, err := os.Stat(filepath.Join(e.dir, "restarted"))
	return err == nil
}

func (e *isolationEngine) isolation() string {
	e.t.Helper()
	if _, err := os.Stat(e.configPath()); err != nil {
		return ""
	}
	v, _ := e.file().Vals["isolation"].(string)
	return v
}

func (e *isolationEngine) runsc() string {
	return filepath.Join(e.a.Cfg.CabooseHome, "runsc", "aarch64", "runsc")
}

// On OrbStack with no runsc: downloaded, registered in docker.json beside
// what was there (kept as it was), the engine restarted, and gvisor chosen
// by default and written.
func TestSetupIsolationRegistersOnOrbStack(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", false, false, "")
	if err := os.MkdirAll(filepath.Dir(e.daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := `{"features": {"buildkit": true}}`
	if err := os.WriteFile(e.daemon, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.run("\ny\n\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Downloaded gVisor into", "its checksum verified", "+ \"runtimes\": {",
		"Registered runsc in", "OrbStack's Docker engine restarted, with runsc",
		"it is tried when the container is created", `Wrote isolation = "gvisor.default"`)
	var cfg struct {
		Features map[string]bool
		Runtimes map[string]struct {
			Path        string
			RuntimeArgs []string
		}
	}
	data, _ := os.ReadFile(e.daemon)
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("docker.json: %v\n%s", err, data)
	}
	if !cfg.Features["buildkit"] || cfg.Runtimes["runsc"].Path != e.runsc() || !slices.Equal(cfg.Runtimes["runsc"].RuntimeArgs, runscArgs) {
		t.Errorf("docker.json:\n%s", data)
	}
	if fi, _ := os.Stat(e.daemon); fi.Mode().Perm() != 0o600 {
		t.Errorf("docker.json mode = %v", fi.Mode())
	}
	if b, _ := os.ReadFile(e.daemon + ".before-caboose"); string(b) != orig {
		t.Errorf("backup = %q", b)
	}
	if _, err := os.Stat(e.runsc()); err != nil {
		t.Error(err)
	}
	if got := e.isolation(); got != "gvisor.default" {
		t.Errorf("isolation = %q", got)
	}

	// Run again: runsc is there, nothing is downloaded or edited, and
	// gvisor, written, is kept.
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if e.said("Downloaded") || !e.said("Nothing changed") {
		t.Errorf("second run:\n%s", e.errb)
	}
}

// docker.json emptied by hand while the engine still runs caboose's runsc:
// said, since the next restart would drop it, and registered again.
func TestSetupIsolationRunscLostFromDaemon(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", true, false, "")
	if err := os.MkdirAll(filepath.Dir(e.daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.daemon, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.run("\n\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("no longer registers it: the engine drops it at its next restart", `+ "runtimes": {`,
		"Registered runsc in", "OrbStack's Docker engine restarted, with runsc", `Wrote isolation = "gvisor.default"`)
	var cfg struct{ Runtimes map[string]daemonRuntime }
	data, _ := os.ReadFile(e.daemon)
	if err := json.Unmarshal(data, &cfg); err != nil || !slices.Equal(cfg.Runtimes["runsc"].RuntimeArgs, runscArgs) {
		t.Errorf("docker.json (%v):\n%s", err, data)
	}
}

// A runsc that is not caboose's download is not caboose's to change, even
// when docker.json does not list it.
func TestSetupIsolationForeignRunscKept(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", true, false, "")
	e.setLoaded(daemonRuntime{Path: "/usr/local/bin/runsc"})
	if err := os.MkdirAll(filepath.Dir(e.daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.daemon, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut(`Wrote isolation = "gvisor.default"`)
	if e.said("without flags") || e.said("no longer registers") || e.restarted() {
		t.Errorf("offered:\n%s", e.errb)
	}
	if b, _ := os.ReadFile(e.daemon); string(b) != "{}" {
		t.Errorf("docker.json = %s", b)
	}
}

// caboose's runsc of an older release than gVisor's latest: said, and
// downloaded over it when agreed to, with no engine restart (the path and
// the entry are the same); declined, it stays as it was. One with no
// record cannot be told, and is offered too. The latest is only said.
func TestSetupIsolationUpdatesRunsc(t *testing.T) {
	for _, tc := range []struct {
		name, sum, answer string
		updated           bool
		want              string
	}{
		{"older", "ab", "\n\n", true, "gVisor has a newer release than the one caboose downloaded into"},
		{"declined", "ab", "n\n\n", false, "Not updated; caboose setup isolation offers it again"},
		{"no record", "", "\n\n", true, "caboose cannot read which gVisor release is in"},
		{"latest", "latest", "\n", false, "is its latest release (downloaded "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newIsolationEngine(t, "darwin", "OrbStack", true, false, "")
			dir := filepath.Dir(e.runsc())
			switch tc.sum {
			case "":
				os.Remove(filepath.Join(dir, runscRecord))
			case "latest":
			default:
				writeRunscRecord(t, dir, strings.Repeat(tc.sum, 64), time.Now().Add(-100*24*time.Hour))
			}
			if err := e.run(tc.answer, "isolation"); err != nil {
				t.Fatalf("%v\n%s", err, e.errb)
			}
			e.wantOut(tc.want, `Wrote isolation = "gvisor.default"`)
			b, _ := os.ReadFile(e.runsc())
			if updated := string(b) == "\x7fELF runsc"; updated != tc.updated {
				t.Errorf("updated = %v, runsc = %q\n%s", updated, b, e.errb)
			}
			if tc.updated {
				e.wantOut("Updated gVisor in", "its checksum verified")
				if rec, ok := readRunscRelease(dir); !ok || rec.SHA512 != releaseSum(t, releaseGood) {
					t.Errorf("record = %+v", rec)
				}
			}
			if e.restarted() {
				t.Error("engine restarted")
			}
		})
	}
}

// With no answer from gVisor's server, nothing is offered, and setup goes on.
func TestSetupIsolationRunscUpdateUnknown(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", true, false, "")
	runscReleases = "http://127.0.0.1:1"
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Could not tell whether gVisor has a newer release", `Wrote isolation = "gvisor.default"`)
}

// Declined, nothing is downloaded or edited, and docker is written: the
// strongest that works.
func TestSetupIsolationDeclined(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", false, false, "")
	if err := e.run("n\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Not downloaded", `Wrote isolation = "container.default"`)
	if _, err := os.Stat(e.daemon); err == nil {
		t.Error("docker.json written")
	}
	if _, err := os.Stat(filepath.Join(e.a.Cfg.CabooseHome, "runsc")); err == nil {
		t.Error("runsc downloaded")
	}
	if got := e.isolation(); got != "container.default" {
		t.Errorf("isolation = %q", got)
	}
}

// Downloaded, but the edit declined: docker.json is untouched, no engine
// restart, docker written.
func TestSetupIsolationRegisterDeclined(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", false, false, "")
	if err := e.run("\nn\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Not registered; caboose setup isolation offers it again", `Wrote isolation = "container.default"`)
	if _, err := os.Stat(e.daemon); err == nil {
		t.Error("docker.json written")
	}
	if e.restarted() {
		t.Error("engine restarted")
	}
}

// An engine that does not come back with runsc is said, and docker written.
func TestSetupIsolationRestartFails(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", false, false, "")
	if err := os.WriteFile(orbCommand, []byte("#!/bin/sh\necho 'no such engine' >&2; exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.run("\ny\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("'"+orbCommand+" restart docker' failed: no such engine", "Restart Docker from OrbStack's menu", `Wrote isolation = "container.default"`)
}

// runsc already there, with an image to try it: a probe runs under it,
// and says the sandbox runs as root on this engine.
func TestSetupIsolationTriesRunsc(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "OrbStack", true, true, "no")
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("A container runs under runsc. On this engine the agent user cannot write its mounts under it", `Wrote isolation = "gvisor.default"`)
	if b, _ := os.ReadFile(filepath.Join(e.dir, "probe")); !strings.Contains(string(b), "--runtime\nrunsc\n") {
		t.Errorf("probe: %q", b)
	}
}

// docker written, it stays the default, even with gvisor available.
func TestSetupIsolationKeepsDocker(t *testing.T) {
	e := newIsolationEngine(t, "linux", "Ubuntu 24.04", true, false, "")
	e.writeConfig("isolation = \"container.default\"\n[container.default]\n")
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Now: container (the profile container.default).", "Nothing changed")
}

// On Linux caboose does not register runsc: it says how, and writes
// docker; gvisor written there is kept only if asked.
func TestSetupIsolationLinux(t *testing.T) {
	e := newIsolationEngine(t, "linux", "Ubuntu 24.04", false, false, "")
	if err := e.run("", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("install it as "+gvisorInstall+" says", "sudo runsc install -- --host-uds=open --net-raw --allow-packet-socket-write",
		`Wrote isolation = "container.default"`)

	e.writeConfig("isolation = \"gvisor.default\"\n[gvisor.default]\n")
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("isolation is gvisor, which cannot work here", `Wrote isolation = "container.default"`)
}

// On Linux, a runsc gVisor's install registered without --host-uds=open
// works, but cannot reach the forwarded SSH agent: said, with the fix,
// and nothing changed. One with it says nothing.
func TestSetupIsolationLinuxRunscFlags(t *testing.T) {
	e := newIsolationEngine(t, "linux", "Ubuntu 24.04", true, false, "")
	e.setLoaded(daemonRuntime{Path: "/usr/local/bin/runsc"})
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Docker's runsc runs without --host-uds=open", "/etc/docker/daemon.json",
		"sudo systemctl reload docker", `Wrote isolation = "gvisor.default"`)

	e.setLoaded(daemonRuntime{Path: "/usr/local/bin/runsc", RuntimeArgs: runscInstallArgs})
	e.errb.Reset()
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if e.said("without --host-uds") {
		t.Errorf("said:\n%s", e.errb)
	}
}

// Docker Desktop: not yet.
func TestSetupIsolationDockerDesktop(t *testing.T) {
	e := newIsolationEngine(t, "darwin", "Docker Desktop", false, false, "")
	if err := e.run("", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("cannot register one with Docker Desktop yet", `Wrote isolation = "container.default"`)
}

// A container created under runsc, which docker no longer has: setup says
// what the next launch does with it, or, keeping gvisor without runsc,
// that nothing can start until runsc is back, and how.
func TestSetupIsolationContainerRuntimeGone(t *testing.T) {
	e := newIsolationEngine(t, "linux", "Ubuntu 24.04", false, false, "")
	b, _ := os.ReadFile(e.a.Docker.Path)
	s := strings.Replace(string(b), "case \"$1\" in\n  image)", `case "$1" in
  inspect) case "$*" in *State.Status*) echo exited ;; *Labels*) echo '{"`+assets.LabelIsolation+`":"gvisor"}' ;; esac; exit 0 ;;
  image)`, 1)
	if err := os.WriteFile(e.a.Docker.Path, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	e.writeConfig("isolation = \"gvisor.default\"\n[gvisor.default]\n")
	if err := e.run("\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut(`Wrote isolation = "container.default"`, "the next launch recreates it with isolation container (it is stopped, so no sessions are lost)")

	e.writeConfig("isolation = \"gvisor.default\"\n[gvisor.default]\n")
	if err := e.run("2\n", "isolation"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("no new one can be created until docker has runsc")
}
