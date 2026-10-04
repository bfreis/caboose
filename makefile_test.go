package caboose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVersionStampSettlesWithoutCommits runs the Makefile's version stamp
// rule, and only that rule (no Go build), where git has nothing to say: no
// repo at all, and a repo with no commits yet. The stamp must be written once
// and then left alone -- a stamp ending in empty lines, which `$(cat)`
// strips, would never compare equal, and ./caboose would relink on every
// make. And an
// empty repo must not stamp the literal "HEAD" that `git rev-parse HEAD`
// echoes there.
func TestVersionStampSettlesWithoutCommits(t *testing.T) {
	for _, tool := range []string{"make", "git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		init bool
	}{{"no git", false}, {"no commits", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Keep git from finding a repo above the temp dir, or one a
			// GIT_DIR in the environment points at.
			env := []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "GIT_") && !strings.HasPrefix(e, "MAKEFLAGS=") {
					env = append(env, e)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.init {
				cmd := exec.Command("git", "-C", dir, "init", "-q")
				cmd.Env = env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v\n%s", err, out)
				}
			}
			launcher := filepath.Join(dir, "c")
			stamp := launcher + ".version"
			makeStamp := func() {
				t.Helper()
				cmd := exec.Command("make", "-s", "LAUNCHER="+launcher, stamp)
				cmd.Dir, cmd.Env = dir, env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("make: %v\n%s", err, out)
				}
			}

			makeStamp()
			if b, err := os.ReadFile(stamp); err != nil || string(b) != "dev\n" {
				t.Fatalf("stamp = %q, %v; want \"dev\\n\"", b, err)
			}
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(stamp, old, old); err != nil {
				t.Fatal(err)
			}
			makeStamp()
			if fi, err := os.Stat(stamp); err != nil || !fi.ModTime().Equal(old) {
				t.Errorf("an unchanged version rewrote the stamp, which relinks ./caboose")
			}
		})
	}
}

// TestVMKernelFilesAgree runs make vm-kernel over a kernel that is already
// built (so no docker), the way a checkout holds one: its .SOURCE must be
// rewritten to name the pinned version even when an older kernel's (Kata's,
// once) sits there newer than everything else, and a kernel whose banner is
// not the pinned version, or with no .config, must fail rather than pass as
// built.
func TestVMKernelFilesAgree(t *testing.T) {
	for _, tool := range []string{"make", "grep", "head"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	src, err := filepath.Glob("vm/kernel/config-*")
	if err != nil || len(src) == 0 {
		t.Fatalf("no vm/kernel/config-*: %v", err)
	}
	src = append(src, "Makefile", "vm/kernel/Dockerfile", "vm/kernel/check-config")
	pin, err := exec.Command("make", "-s", "vm-kernel-pin").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.Fields(string(pin))[0]

	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "MAKEFLAGS=") && !strings.HasPrefix(e, "VM_") {
			env = append(env, e)
		}
	}
	old := time.Now().Add(-time.Hour)
	setup := func(t *testing.T, banner string, config bool) string {
		t.Helper()
		dir := t.TempDir()
		write := func(name string, b []byte, at time.Time) {
			t.Helper()
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, at, at); err != nil {
				t.Fatal(err)
			}
		}
		for _, f := range src {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			write(f, b, old)
		}
		// The kernel is newer than its sources and pin, so make leaves it
		// be; the stale .SOURCE is newer still.
		write("vm-dist/.kernel-pin", pin, old)
		now := time.Now()
		write("vm-dist/kernel-arm64", []byte("\x00\x01Linux version "+banner+" (caboose@caboose)\n\x00"), now.Add(-time.Minute))
		if config {
			write("vm-dist/kernel-arm64.config", []byte("CONFIG_ARM64=y\n"), now.Add(-time.Minute))
		}
		write("vm-dist/kernel-arm64.SOURCE", []byte("Kata Containers 3.x, Linux 6.18.35-202\n"), now)
		return dir
	}
	run := func(dir string) (string, error) {
		cmd := exec.Command("make", "-s", "vm-kernel", "VM_ARCH=arm64")
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("stale source replaced", func(t *testing.T) {
		dir := setup(t, version+"-caboose", true)
		if out, err := run(dir); err != nil {
			t.Fatalf("make vm-kernel: %v\n%s", err, out)
		}
		b, err := os.ReadFile(filepath.Join(dir, "vm-dist/kernel-arm64.SOURCE"))
		if err != nil {
			t.Fatal(err)
		}
		if want := "Linux " + version + ", unmodified, from\n"; !strings.HasPrefix(string(b), want) {
			t.Errorf(".SOURCE = %q; want it to start %q", b, want)
		}
	})
	for _, tc := range []struct {
		name, banner string
		config       bool
	}{
		{"older kernel", "6.18.35", true},
		{"longer version", version + "0", true},
		{"no config", version + "-caboose", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := run(setup(t, tc.banner, tc.config)); err == nil {
				t.Errorf("make vm-kernel passed a kernel it should refuse:\n%s", out)
			} else if !strings.Contains(out, "!!") {
				t.Errorf("make vm-kernel failed without saying why: %v\n%s", err, out)
			}
		})
	}
}
