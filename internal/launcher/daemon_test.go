package launcher

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/saferm"
)

// A release runs the daemon in CABOOSE_HOME/daemon, a checkout's build in
// CABOOSE_HOME/daemon-dev.
func TestDaemonDir(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(e *updateEnv)
		want  string
	}{
		"managed release": {func(*updateEnv) {}, daemon.DirName},
		"other release":   {func(e *updateEnv) { e.a.Executable = func() (string, error) { return "/usr/local/bin/caboose", nil } }, daemon.DirName},
		"dev build":       {func(e *updateEnv) { e.version("v1.0.0-3-gabcdef0") }, daemon.DevDirName},
		"a checkout":      {func(e *updateEnv) { e.a.Checkout = "/src/caboose" }, daemon.DevDirName},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdateEnv(t)
			tc.setup(e)
			if got, want := e.a.daemonDir(), filepath.Join(e.a.Cfg.CabooseHome, tc.want); got != want {
				t.Errorf("daemon dir %s, want %s", got, want)
			}
		})
	}
}

// Starting the daemon runs `caboose daemon` from this executable.
func TestSpawnDaemon(t *testing.T) {
	e := newUpdateEnv(t)
	if err := e.a.daemonConfig().Spawn(); err != nil {
		t.Fatal(err)
	}
	if len(e.spawned) != 1 || !slices.Equal(e.spawned[0][1:], []string{"daemon"}) {
		t.Errorf("spawned %v", e.spawned)
	}
}

// Without the tests' Spawn, the daemon is started detached with exactly
// the minimal environment: nothing else of this process's.
func TestSpawnDaemonEnvironment(t *testing.T) {
	vars := map[string]string{"HOME": "/h", "LANG": "C", "SSH_AUTH_SOCK": "/s", "DOCKER_HOST": "unix:///d"}
	a := &App{Cfg: &config.Config{Home: "/h", CabooseHome: "/h/.caboose", Getenv: func(k string) string { return vars[k] }},
		Executable: func() (string, error) { return "/bin/caboose", nil }}
	old := startDetached
	t.Cleanup(func() { startDetached = old })
	var gotExe string
	var gotArgs, gotEnv []string
	startDetached = func(exe string, args, env []string) (*exec.Cmd, error) {
		gotExe, gotArgs, gotEnv = exe, args, env
		return nil, errors.New("not started")
	}
	if err := a.spawnDaemon(); err == nil {
		t.Fatal("no error from the stub")
	}
	want := []string{"HOME=/h", "LANG=C", "CABOOSE_HOME=/h/.caboose"}
	if gotExe != "/bin/caboose" || !slices.Equal(gotArgs, []string{"daemon"}) || !slices.Equal(gotEnv, want) {
		t.Errorf("started %s %v with %v, want env %v", gotExe, gotArgs, gotEnv, want)
	}
}

// status and stop with no daemon say so and create nothing, not even
// CABOOSE_HOME.
func TestDaemonStatusCreatesNothing(t *testing.T) {
	tmp, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := saferm.RemoveRoot(tmp); err != nil {
			t.Error(err)
		}
	})
	home := filepath.Join(tmp, "ch")
	for _, arg := range []string{"status", "stop"} {
		out := &bytes.Buffer{}
		a := &App{Cfg: &config.Config{Home: tmp, CabooseHome: home, Getenv: func(string) string { return "" }},
			Stdout: out, Stderr: &bytes.Buffer{},
			Spawn: func(string, ...string) error { t.Error("spawned a daemon"); return nil }}
		if err := a.Daemon([]string{arg}); err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if !strings.Contains(out.String(), "not running") {
			t.Errorf("%s said %q", arg, out)
		}
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("created %v", ents)
	}
}

// No daemon directory is ever reachable from a sandbox: no mount
// createContainer makes, nor any directory the vm backend shares for them,
// is in, or holds, CABOOSE_HOME/daemon or CABOOSE_HOME/daemon-dev.
func TestDaemonDirsAreInNoMount(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, &backendtest.Fake{})
			b.Cfg.CabooseHome = b.tmp
			root := filepath.Join(b.tmp, "dev")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			b.Cfg.Roots = []config.Root{{Name: "dev", Host: root, Container: "/work/dev"}}
			for _, d := range daemon.Dirs(b.tmp) {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.createContainer(false); err != nil {
				t.Fatalf("createContainer: %v\n%s", err, b.errb)
			}
			spec, _ := b.box.Spec()
			var sources []string
			for _, m := range spec.Mounts {
				sources = append(sources, filepath.Clean(m.Source))
			}
			shares, err := backend.SharePaths(spec.Mounts)
			if err != nil {
				t.Errorf("the vm backend's shares: %v", err)
			}
			for _, src := range append(sources, shares...) {
				for _, d := range daemon.Dirs(b.tmp) {
					if config.Within(src, d) || config.Within(d, src) {
						t.Errorf("%s is mounted or shared, and reaches the daemon directory %s", src, d)
					}
				}
			}
		})
	}
}
