package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
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
// The image is a dockerfile profile, [dockerfile.default], whose dir holds
// testDockerfile, unless kv says otherwise: CABOOSE_BASE_IMAGE makes it a
// ref ([ref.default] image), and CABOOSE_APKO leaves it to the default,
// apko.default. Other keys that are config.toml settings are written to
// the default environment's config.toml too: CABOOSE_NO_AUTO_BUILD
// ([build] auto_build), CABOOSE_READY_TIMEOUT and CABOOSE_NO_TMUX
// ([session]). Names of the container and image are not settings:
// caboose-default and caboose:default.
func sandboxEnv(t *testing.T, kv ...string) (home string) {
	t.Helper()
	home = t.TempDir()
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "CABOOSE_") || k == "TZ" {
			t.Setenv(k, "")
		}
	}
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	top := []string{`image = "dockerfile.default"`}
	profile := "[dockerfile.default]\n"
	var build, session []string
	for i := 0; i+1 < len(kv); i += 2 {
		k, v := kv[i], kv[i+1]
		switch k {
		case "CABOOSE_BASE_IMAGE":
			top = []string{`image = "ref.default"`}
			profile = "[ref.default]\nimage = " + strconv.Quote(v) + "\n"
		case "CABOOSE_APKO":
			top, profile = nil, ""
		case "CABOOSE_NO_AUTO_BUILD":
			build = append(build, "auto_build = false")
		case "CABOOSE_READY_TIMEOUT":
			session = append(session, "ready_timeout = "+v)
		case "CABOOSE_NO_TMUX":
			session = append(session, "tmux = false")
		default:
			t.Setenv(k, v)
		}
	}
	var toml string
	for _, l := range top {
		toml += l + "\n"
	}
	toml += profile
	if len(build) > 0 {
		toml += "[build]\n" + strings.Join(build, "\n") + "\n"
	}
	if len(session) > 0 {
		toml += "[session]\n" + strings.Join(session, "\n") + "\n"
	}
	if toml != "" {
		writeConfig(t, home, toml)
	}
	if strings.HasPrefix(profile, "[dockerfile.") {
		dir := imageDir(home)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestDockerfile(t, dir)
	}
	return home
}

// testDockerfile is what sandboxEnv's dockerfile profile builds.
const testDockerfile = "FROM ubuntu:26.04\n"

// imageDir is the default environment's dockerfile profile's dir.
func imageDir(home string) string {
	return filepath.Join(home, ".caboose", "envs", "default", "dockerfile", "default")
}

// writeTestDockerfile writes testDockerfile into dir, a directory, with
// the modes whatever the umask, so that its DirHash is dockerfileHash.
func writeTestDockerfile(t *testing.T, dir string) {
	t.Helper()
	if err := writeDockerfileIn(dir); err != nil {
		t.Fatal(err)
	}
}

func writeDockerfileIn(dir string) error {
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, []byte(testDockerfile), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(df, 0o644); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// dockerfileHash is DirHash of a dir holding testDockerfile alone, as
// writeTestDockerfile writes it: what a build of sandboxEnv's dockerfile
// profile labels the layer with.
var dockerfileHash = sync.OnceValue(func() string {
	dir, err := os.MkdirTemp("", "caboose-test-image-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	if err := writeDockerfileIn(dir); err != nil {
		panic(err)
	}
	h, err := assets.DirHash(dir)
	if err != nil {
		panic(err)
	}
	return h
})

// containerLabels is a piece of sh for scriptedDocker answering the running
// container's labels as a current caboose made it, for roots mounted at the
// container paths named.
func containerLabels(roots ...string) string {
	paths, _ := json.Marshal(roots)
	labels, _ := json.Marshal(map[string]string{
		assets.LabelCompat: strconv.Itoa(assets.Compat),
		assets.LabelRoots:  string(paths),
	})
	return `[ "$*" = "inspect --type=container -f {{json .Config.Labels}} caboose-default" ] && { echo '` + string(labels) + `'; exit 0; }`
}

// writeConfig writes the default environment's config.toml.
func writeConfig(t *testing.T, home, toml string) string {
	t.Helper()
	cfg := filepath.Join(home, ".caboose", "envs", "default", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runIt(argv ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(argv, &out, &errb)
	return code, out.String(), errb.String()
}

func TestMissingRoot(t *testing.T) {
	fakeDocker(t)
	home := sandboxEnv(t)
	cfg := filepath.Join(home, ".caboose", "envs", "default", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[roots]\nsrc = \"/does/not/exist\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runIt("status")
	want := "caboose: root src, /does/not/exist, does not exist (set by [roots] in " + cfg + ").\n" +
		config.RootsHelp + "\n"
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
	want := "caboose: root dev, " + home + "/dev, does not exist (the default: config.toml has no [roots]).\n" +
		config.RootsHelp + "\n"
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

func TestAttachRefusesCwdOutsideRoots(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	t.Chdir(t.TempDir())
	code, _, errs := runIt("claude", "--version")
	if code != 1 || !strings.Contains(errs, "is outside the mounted root, ") {
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
