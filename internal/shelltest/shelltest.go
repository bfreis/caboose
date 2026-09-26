// Package shelltest picks the POSIX shells that tests run caboose's shell
// scripts with: /bin/sh always, and whichever of the others this machine
// has, since images differ in theirs (dash on Debian, BusyBox ash on
// Alpine, bash as /bin/sh on some).
package shelltest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Shells returns each shell as the argv that runs a script with it.
//
// The tests fake an image's tools with stubs on PATH, so a BusyBox built
// to prefer its own applets over PATH (Debian's and Ubuntu's are) would
// test the host instead of the fake image: it is left out, and said so.
// Alpine's BusyBox, the one images have, runs what PATH finds.
func Shells(t *testing.T) [][]string {
	t.Helper()
	out := [][]string{{"/bin/sh"}}
	for _, s := range []string{"dash", "ash", "mksh", "yash", "posh"} {
		if p, err := exec.LookPath(s); err == nil {
			out = append(out, []string{p})
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		out = append(out, []string{p, "--posix"})
	}
	if p, err := exec.LookPath("busybox"); err == nil {
		if runsPath(t, p) {
			out = append(out, []string{p, "sh"})
		} else {
			t.Logf("%s runs its own applets before PATH: not testing with it", p)
		}
	}
	return out
}

// runsPath reports whether busybox's sh runs a uname found on PATH rather
// than its own applet.
func runsPath(t *testing.T, busybox string) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte("#!/bin/sh\necho stub\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(busybox, "sh", "-c", "uname")
	c.Env = append(os.Environ(), "PATH="+dir)
	out, _ := c.Output()
	return string(out) == "stub\n"
}
