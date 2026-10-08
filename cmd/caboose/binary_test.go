package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// TestInstalledBinary runs a real build of the launcher from a temp dir, the
// way a release or `go install` binary runs: no checkout next to it, none
// stamped in, no Makefile anywhere. The in-process tests go through run()
// and so never exercise how the executable finds (or fails to find) its
// checkout; this does.
func TestInstalledBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the launcher")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	bin := filepath.Join(t.TempDir(), "caboose")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	fakeDocker(t)
	home := sandboxEnv(t)
	root, err := config.Physical(filepath.Join(home, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "proj")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	runBin := func(dir string, args ...string) (int, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		// Whatever it says, it must not send someone with no checkout to a
		// Makefile they do not have.
		if strings.Contains(out.String(), "make ") {
			t.Errorf("caboose %s mentions make:\n%s", strings.Join(args, " "), out.String())
		}
		return code, out.String()
	}

	// An attach gets as far as installing caboose's instructions before
	// the (fake, failing) docker stops it: as the managed CLAUDE.md, never
	// into ~/.claude, which is the user's.
	if code, out := runBin(project, "claude", "--version"); code == 0 {
		t.Errorf("attach succeeded against a failing docker:\n%s", out)
	}
	data := filepath.Join(home, ".caboose", "envs", "default", "data")
	b, err := os.ReadFile(filepath.Join(data, "claude-code", "CLAUDE.md"))
	if err != nil {
		t.Fatalf("caboose's instructions not installed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(data, "home", ".claude", "CLAUDE.md")); err == nil {
		t.Error("a launch wrote ~/.claude/CLAUDE.md")
	}
	md := string(b)
	for _, want := range []string{datadir.UpstreamURL, "NOT MOUNTED", "`" + root + "`"} {
		if !strings.Contains(md, want) {
			t.Errorf("installed CLAUDE.md lacks %q", want)
		}
	}
	if strings.Contains(md, "@@") {
		t.Error("installed CLAUDE.md has an unfilled placeholder")
	}

	if code, out := runBin(project, "status"); code != 0 || !strings.Contains(out, "root      : "+root+" -> /work/dev") {
		t.Errorf("caboose status: exit %d\n%s", code, out)
	}

	// Outside the roots: told what they are and how to move them.
	code, out := runBin(t.TempDir(), "claude", "--version")
	if code != 1 || !strings.Contains(out, "is outside the mounted root, "+root+
		" at /work/dev (the default: config.toml has no [roots])") || !strings.Contains(out, "[roots]") {
		t.Errorf("outside the root: exit %d\n%s", code, out)
	}

	// A running container: the session starts at the project's path under
	// /work/dev, the same whatever the host path -- or, in a container
	// created with other roots than the configuration has now (here one at
	// /work), where that one mounted it, with a note that a restart moves it.
	for _, tc := range []struct{ mount, want, note string }{
		{"/work/dev", "/work/dev/proj", ""},
		{"/work", "/work/proj", "the container mounts " + root + " at /work; the configuration says " + root + " at /work/dev"},
	} {
		log := scriptedDocker(t, containerRunning+"\n"+containerLabels(tc.mount)+`
case "$*" in
  "inspect --type=container caboose-default --format "*) printf '%s\t%s\n' "`+tc.mount+`" "`+root+`"; exit 0 ;;
esac
[ "$1" = exec ] && exit 0`)
		for _, args := range [][]string{{"claude", "--version"}, {"shell", "-c", "true"}} {
			code, out := runBin(project, args...)
			if code != 0 {
				t.Errorf("caboose %v: exit %d\n%s", args, code, out)
			}
			if !strings.Contains(out, tc.note) || tc.note == "" && strings.Contains(out, "the container mounts") {
				t.Errorf("caboose %v with %s mounted: note %q missing or out of place\n%s", args, tc.mount, tc.note, out)
			}
			lines := dockerLog(t, log)
			if last := lines[len(lines)-1]; !strings.Contains(last, " -w "+tc.want+" caboose-default ") {
				t.Errorf("caboose %v with %s mounted: last docker call %q, want it at %s", args, tc.mount, last, tc.want)
			}
		}
	}
}
