package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createdThenRunning is a container that is absent until `docker run -d`
// creates it (or exited until `docker start` starts it, with exited), and
// running from then on, but never ready: the ready marker's exec fails. Its
// ~/.local/bin is mounted from bin.
func createdThenRunning(t *testing.T, exited bool, bin string) string {
	t.Helper()
	flag := filepath.Join(t.TempDir(), "up")
	before := ""
	if exited {
		before = "echo exited; exit 0"
	}
	return `case "$*" in
  "run -d "*|"start box") touch "` + flag + `"; exit 0 ;;
  "inspect --type=container -f {{.State.Status}} box") if [ -f "` + flag + `" ]; then echo running; exit 0; fi; ` + before + ` ;;
  "inspect --type=container box --format "*) printf '/home/agent/.local/bin\t%s\n' "` + bin + `"; exit 0 ;;
esac`
}

// The wait for the ready marker says an install is coming when the dir the
// container mounts as ~/.local has no Claude Code in it yet -- a first run on
// that platform, which a data dir used with another image can have too --
// and not otherwise.
func TestReadyWaitSaysWhatItWaitsFor(t *testing.T) {
	const link = "/home/agent/.local/share/claude/versions/2.1.9"
	for _, tc := range []struct {
		name      string
		exited    bool
		installed bool
		install   bool
	}{
		{"created, first on the platform", false, false, true},
		{"created, installed", false, true, false},
		{"started, emptied since", true, false, true},
		{"started, installed", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := sandboxEnv(t, "CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box", "CABOOSE_READY_TIMEOUT", "1")
			local := filepath.Join(home, ".caboose", "envs", "default", "data", "dot_local", "linux-arm64")
			// Another platform's dir, from a data dir used with another image.
			if err := os.MkdirAll(filepath.Join(home, ".caboose", "envs", "default", "data", "dot_local", "linux-arm64-musl", "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.installed {
				version := filepath.Join(local, "share/claude/versions/2.1.9")
				if err := os.MkdirAll(filepath.Dir(version), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(version, nil, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(local, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(link, filepath.Join(local, "bin/claude")); err != nil {
					t.Fatal(err)
				}
			}
			scriptedDocker(t, createdThenRunning(t, tc.exited, filepath.Join(local, "bin"))+"\n"+imageLabels(defaultLabels("v9")))
			inProject(t, home)
			code, _, errs := runIt("claude", "-p", "hi")
			if code != 1 || !strings.Contains(errs, "caboose: not ready after 1s") {
				t.Fatalf("exit %d, stderr:\n%s", code, errs)
			}
			note := "caboose: first run on linux-arm64 — installing Claude Code into " + local + ", this takes a minute\n"
			if got := strings.Contains(errs, note); got != tc.install {
				t.Errorf("install note %v, want %v; stderr:\n%s", got, tc.install, errs)
			}
			if strings.Contains(errs, "in this data dir") || (!tc.install && strings.Contains(errs, "installing")) {
				t.Errorf("stderr:\n%s", errs)
			}
		})
	}
}
