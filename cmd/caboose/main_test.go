package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

// fakeDocker puts a `docker` on an otherwise empty PATH. It logs its argv
// (and, for `build`, the context it was handed) and fails every call but
// build, so the container always reads as absent and docker as unreachable.
// No real docker is ever reached.
func fakeDocker(t *testing.T) (logPath string) {
	t.Helper()
	return scriptedDocker(t, "")
}

// scriptedDocker is fakeDocker with body, a piece of sh, run after the
// logging and before everything else: it answers the calls a test needs
// answered, and exits, leaving the rest to fail. A build writes a line to
// each of stdout and stderr, so tests can see where docker's output went.
func scriptedDocker(t *testing.T, body string) (logPath string) {
	t.Helper()
	bin := t.TempDir()
	logPath = filepath.Join(bin, "log")
	script := `#!/bin/sh
PATH=/usr/bin:/bin
echo "$*" >> "` + logPath + `"
` + body + `
if [ "$1" = build ]; then
    for a; do ctx="$a"; done
    ls "$ctx" >> "` + logPath + `"
    echo "docker build stdout"
    echo "docker build stderr" >&2
    exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return logPath
}

// sandboxEnv clears every setting the launcher reads, then applies kv.
func sandboxEnv(t *testing.T, kv ...string) (home string) {
	t.Helper()
	home = t.TempDir()
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "CABOOSE_") || k == "FORCE" || k == "TZ" {
			t.Setenv(k, "")
		}
	}
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
	return home
}

func runIt(argv ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(argv, &out, &errb)
	return code, out.String(), errb.String()
}

func TestMissingRepoRoot(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t, "CABOOSE_REPO_ROOT", "/does/not/exist")
	code, _, errs := runIt("status")
	want := "caboose: repo root /does/not/exist does not exist (set by CABOOSE_REPO_ROOT).\n" +
		config.RepoRootHelp + "\n"
	if code != 1 || errs != want {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

// TestMissingDefaultRepoRoot is the first run of anyone whose repos are not
// in ~/dev: it has to explain a setting they have never heard of.
func TestMissingDefaultRepoRoot(t *testing.T) {
	log := fakeDocker(t)
	home := sandboxEnv(t)
	if err := os.Remove(filepath.Join(home, "dev")); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runIt("claude", "--version")
	want := "caboose: repo root " + home + "/dev does not exist (the default: CABOOSE_REPO_ROOT is unset).\n" +
		config.RepoRootHelp + "\n"
	if code != 1 || errs != want {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("docker was run: %q", b)
	}
}

func TestSessionNeedsAName(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	code, _, errs := runIt("--session")
	if code != 2 || errs != "caboose: --session needs a name\n" {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

func TestDetachWhenNotRunning(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	code, _, errs := runIt("detach")
	if code != 0 || errs != "caboose: container is not running\n" {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

func TestAttachRefusesCwdOutsideRepoRoot(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	t.Chdir(t.TempDir())
	code, _, errs := runIt("claude", "--version")
	if code != 1 || !strings.Contains(errs, "is outside the mounted repo root") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

func TestEmptyHomeDies(t *testing.T) {
	log := fakeDocker(t)
	sandboxEnv(t, "HOME", "")
	code, _, errs := runIt("status")
	if code != 1 || !strings.HasPrefix(errs, "caboose: HOME is not set") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("docker was run: %q", b)
	}
}

func TestAttachRefusesProjectFile(t *testing.T) {
	log := fakeDocker(t)
	home := sandboxEnv(t)
	file := filepath.Join(home, "dev", "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CABOOSE_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("CABOOSE_PROJECT", file)
	code, _, errs := runIt("claude", "--version")
	if code != 1 || errs != "caboose: cd: "+file+": Not a directory\n" {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("docker was run: %q", b)
	}
	if _, err := os.Stat(filepath.Join(home, "data")); err == nil {
		t.Error("the data dir was created")
	}
}
