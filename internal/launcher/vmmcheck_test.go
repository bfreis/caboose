package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/version"
)

// fakeVMM is a caboose-vmm that runs script as sh.
func fakeVMM(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "caboose-vmm")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// checkOut is a --check's output of a Mac that can run vm, each line of
// change replacing its key's.
func checkOut(change ...string) string {
	lines := []string{"caboose-vmm-check=1", "version=" + version.Get().Version, "os=darwin", "arch=arm64",
		"macos=15.1", "translated=no", "framework=ok", "supported=yes", "entitled=yes", "valid=ok"}
	for _, c := range change {
		k, _, _ := strings.Cut(c, "=")
		found := false
		for i, l := range lines {
			if strings.HasPrefix(l, k+"=") {
				lines[i], found = c, true
			}
		}
		if !found {
			lines = append(lines, c)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// Each way caboose-vmm --check can fail is its own problem, with what to
// do about it.
func TestCheckVMM(t *testing.T) {
	a := &App{Cfg: &config.Config{}}
	checkout := &App{Cfg: &config.Config{}, Checkout: "/src/caboose"}
	for _, tc := range []struct {
		name, script string
		app          *App
		problem, fix string // substrings; problem "" means none
	}{
		{"usable", "printf '%s' '" + checkOut() + "'", a, "", ""},
		{"unsigned", "printf '%s' '" + checkOut("entitled=no", "valid=Invalid virtual machine configuration. The process doesn’t have the “com.apple.security.virtualization” entitlement. (VZErrorDomain 2)") + "'; exit 1", checkout,
			"not signed with the virtualization entitlement", "run 'make vmm' in /src/caboose"},
		{"unsigned, released", "printf '%s' '" + checkOut("entitled=no") + "'; exit 1", a,
			"not signed", "reinstall caboose"},
		{"unknown entitlement", "printf '%s' '" + checkOut("entitled=unknown") + "'; exit 1", a,
			"could not tell", "reinstall caboose"},
		{"old vmm", "echo 'usage: caboose-vmm DIR' >&2; exit 2", a, "has no --check (it said: usage: caboose-vmm DIR)", "reinstall caboose"},
		{"other version", "printf '%s' '" + checkOut("version=v0.0.1-other") + "'", a, "is version v0.0.1-other", "reinstall caboose"},
		{"rosetta", "printf '%s' '" + checkOut("arch=amd64", "translated=yes") + "'; exit 1", a, "under Rosetta", "reinstall caboose"},
		{"intel", "printf '%s' '" + checkOut("arch=amd64") + "'; exit 1", a, "Apple silicon only", `set isolation = "docker" or "gvisor"`},
		{"old macOS", "printf '%s' '" + checkOut("macos=12.7", "framework=this macOS has no VZX") + "'; exit 1", a, "macOS 12.7, and vm needs macOS 13", "update macOS"},
		{"no framework", "printf '%s' '" + checkOut("framework=loading x: gone") + "'; exit 1", a, "loading x: gone", "update macOS"},
		{"unsupported", "printf '%s' '" + checkOut("supported=no") + "'; exit 1", a, "cannot run VMs", `set isolation = "docker"`},
		{"linux build", "printf '%s' '" + checkOut("os=linux") + "'; exit 1", a, "built for linux", "reinstall caboose"},
		{"hung framework", "printf '%s' '" + checkOut("error=Virtualization.framework did not answer in 5s") + "'; exit 1", a, "did not answer in 5s", "restart the Mac"},
		{"killed", "kill -9 $$", a, "macOS stopped", "security tool"},
		{"not a program", "", a, "", ""}, // replaced below
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeVMM(t, tc.script)
			if tc.name == "not a program" {
				if err := os.Chmod(p, 0o644); err != nil {
					t.Fatal(err)
				}
				tc.problem, tc.fix = "cannot run", "reinstall caboose"
			}
			v := tc.app.checkVMM(p)
			if tc.problem == "" {
				if v.Problem != "" {
					t.Fatalf("a usable caboose-vmm: %s (%s)", v.Problem, v.Fix)
				}
				return
			}
			if !strings.Contains(v.Problem, tc.problem) || !strings.Contains(v.Fix, tc.fix) {
				t.Errorf("problem %q\nfix %q\nwant %q, %q", v.Problem, v.Fix, tc.problem, tc.fix)
			}
		})
	}
}

// A caboose-vmm that never answers is cut off.
func TestCheckVMMTimesOut(t *testing.T) {
	old := vmmCheckTimeout
	vmmCheckTimeout = 200 * time.Millisecond
	t.Cleanup(func() { vmmCheckTimeout = old })
	start := time.Now()
	v := (&App{Cfg: &config.Config{}}).checkVMM(fakeVMM(t, "exec sleep 30"))
	if !strings.Contains(v.Problem, "did not finish in 200ms") {
		t.Errorf("problem %q", v.Problem)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("waited %v", d)
	}
}

func TestMacOSMajor(t *testing.T) {
	for in, want := range map[string]int{"14.5": 14, "13": 13, "26.0.1": 26, "": 0, "x.1": 0} {
		if got := macOSMajor(in); got != want {
			t.Errorf("%q: %d", in, got)
		}
	}
}
