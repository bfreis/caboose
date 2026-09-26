package launcher

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// platformFake is a docker for creating a container from image img: no
// container exists, `image inspect` prints labels, and `run` logs each -v,
// and each -e as "ENV VAR=value".
func platformFake(t *testing.T, labels string) (a *App, data, log string, errb *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	data = filepath.Join(tmp, "data")
	log = filepath.Join(tmp, "log")
	script := `#!/bin/sh
case "$1" in
  inspect) exit 1 ;;
  image) echo '` + labels + `'; exit 0 ;;
  run)
    while [ $# -gt 0 ]; do
      [ "$1" = -v ] && { printf '%s\n' "$2" >> "` + log + `"; shift; }
      [ "$1" = -e ] && { printf 'ENV %s\n' "$2" >> "` + log + `"; shift; }
      shift
    done ;;
esac
`
	fake := filepath.Join(tmp, "docker")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	errb = &bytes.Buffer{}
	a = &App{
		Cfg: &config.Config{Container: "box", Image: "img", Roots: []config.Root{{Host: tmp, Container: "/work"}},
			DataDir: data, KeepVersions: "2", Getenv: func(string) string { return "" }},
		Docker: &docker.CLI{Path: fake},
		Stdout: &bytes.Buffer{}, Stderr: errb,
	}
	return a, data, log, errb
}

// claudeMounts reads the three platform mounts back out of the fake's log,
// data-dir-relative.
func claudeMounts(t *testing.T, log, data string) map[string]string {
	t.Helper()
	b, _ := os.ReadFile(log)
	mounts := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		src, dst, _ := strings.Cut(l, ":")
		if strings.HasPrefix(dst, "/home/agent/.local/") || strings.HasPrefix(dst, "/home/agent/.cache/") {
			mounts[dst] = strings.TrimPrefix(src, data+"/")
		}
	}
	return mounts
}

func wantPlatformMounts(t *testing.T, got map[string]string, platform string) {
	t.Helper()
	want := map[string]string{
		"/home/agent/.local/bin":          "dot_local/" + platform + "/bin",
		"/home/agent/.local/share/claude": "dot_local/" + platform + "/share/claude",
		"/home/agent/.cache/claude":       "dot_local/" + platform + "/cache/claude",
	}
	for dst, w := range want {
		if got[dst] != w {
			t.Errorf("%s: mounted %q, want %q", dst, got[dst], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("mounts = %v", got)
	}
}

// The image's platform label names the dir.
func TestCreateContainerPlatformFromLabel(t *testing.T) {
	a, data, log, errb := platformFake(t, `{"`+assets.LabelPlatform+`":"linux-x64-musl"}`)
	if err := a.createContainer(false); err != nil {
		t.Fatalf("%v\n%s", err, errb)
	}
	wantPlatformMounts(t, claudeMounts(t, log, data), "linux-x64-musl")
}

// The musl build is told to use the image's ripgrep, at creation and from
// the label; a glibc image is told nothing.
func TestCreateContainerRipgrepOnMusl(t *testing.T) {
	for platform, want := range map[string]bool{"linux-x64-musl": true, "linux-arm64-musl": true, "linux-x64": false, "linux-arm64": false} {
		a, _, log, errb := platformFake(t, `{"`+assets.LabelPlatform+`":"`+platform+`"}`)
		if err := a.createContainer(false); err != nil {
			t.Fatalf("%v\n%s", err, errb)
		}
		b, _ := os.ReadFile(log)
		if got := strings.Contains(string(b), "ENV USE_BUILTIN_RIPGREP=0\n"); got != want {
			t.Errorf("%s: USE_BUILTIN_RIPGREP=0 set %v, want %v", platform, got, want)
		}
		if n := strings.Count(string(b), "USE_BUILTIN_RIPGREP"); n > 1 {
			t.Errorf("%s: set %d times", platform, n)
		}
	}
}

// An image without a platform it can name is none caboose build made: no
// container is created from it, and no platform dir either.
func TestCreateContainerRefusesUnknownPlatform(t *testing.T) {
	for _, tc := range []struct{ name, labels, want string }{
		{"no labels", "null", "image 'img' was not built by caboose build (it has no platform label): run 'caboose build'"},
		{"no platform label", `{"` + assets.LabelVersion + `":"v1"}`, "it has no platform label"},
		{"another platform", `{"` + assets.LabelPlatform + `":"windows-x64"}`, `its platform label says "windows-x64"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, data, log, _ := platformFake(t, tc.labels)
			err := a.createContainer(false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if mounts := claudeMounts(t, log, data); len(mounts) != 0 {
				t.Errorf("docker run happened: %v", mounts)
			}
			if _, err := os.Lstat(filepath.Join(data, "dot_local")); err == nil {
				t.Error("a platform dir was created")
			}
		})
	}
}

// A launch against a running container creates no platform dir: those are
// made only for the image a container is created from.
func TestLaunchCreatesNoPlatformDirUnderARunningContainer(t *testing.T) {
	a, data, _ := runningFake(t, "dot_local/linux-arm64")
	for _, d := range []string{"dot_local/linux-arm64/bin", "dot_local/linux-arm64/share/claude"} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ensureRunning(false); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(data, "dot_local"))
	if len(entries) != 1 {
		t.Errorf("dot_local now holds %v", entries)
	}
}

// runningFake is a docker whose container box is running with local (a
// data-dir-relative dir holding bin/) mounted at ~/.local, and which answers
// everything else with success and no output.
func runningFake(t *testing.T, local string) (a *App, data string, out *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	data = filepath.Join(tmp, "data")
	script := `#!/bin/sh
case "$*" in
  "inspect --type=container -f {{.State.Status}} box") echo running ;;
  "inspect --type=container box --format "*)
    printf '%s\t%s\n' /home/agent/.local/bin "` + data + "/" + local + `/bin" /work "` + tmp + `" ;;
esac
exit 0
`
	fake := filepath.Join(tmp, "docker")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out = &bytes.Buffer{}
	a = &App{
		Cfg: &config.Config{Container: "box", Image: "img", Roots: []config.Root{{Host: tmp, Container: "/work"}}, ReadyTimeout: "0",
			DataDir: data, KeepVersions: "2", Getenv: func(string) string { return "" }},
		Docker: &docker.CLI{Path: fake},
		Stdout: out, Stderr: &bytes.Buffer{},
	}
	return a, data, out
}

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusShowsPlatforms(t *testing.T) {
	a, data, out := runningFake(t, "dot_local/linux-arm64")
	mkdirs(t, data, "dot_local/linux-arm64/share/claude", "dot_local/linux-arm64/bin", "dot_local/linux-x64-musl/share/claude")
	if err := a.Status(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, w := range []string{
		// The lines tests/run.sh parses stay as they were.
		"container : box (running)\n",
		"data dir  : " + data + "\n",
		"platform  : linux-arm64\n",
		"local dir : " + data + "/dot_local/linux-arm64\n",
		"\t" + data + "/dot_local/linux-arm64/share/claude\n",
		"\ndisk used per platform (dot_local/<platform>, each with its own versions):\n",
		"\tlinux-arm64  (mounted)\n",
		"\tlinux-x64-musl\n",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("status lacks %q:\n%s", w, s)
		}
	}
	if strings.Index(s, "linux-arm64  (mounted)") > strings.Index(s, "\tlinux-x64-musl\n") {
		t.Errorf("platforms not sorted:\n%s", s)
	}
}

// caboose prune reports the mounted platform's usage, and points at the other
// platforms' dirs without touching them.
func TestPruneNotesOtherPlatforms(t *testing.T) {
	a, data, out := runningFake(t, "dot_local/linux-arm64")
	mkdirs(t, data, "dot_local/linux-arm64/share/claude", "dot_local/linux-arm64/bin", "dot_local/linux-x64-musl/share/claude")
	var errb bytes.Buffer
	a.Stderr = &errb
	if err := a.Prune(); err != nil {
		t.Fatalf("%v\n%s", err, errb.String())
	}
	if !strings.HasPrefix(out.String(), "caboose: now using ") || !strings.Contains(out.String(), data+"/dot_local/linux-arm64/share/claude") {
		t.Errorf("stdout %q", out.String())
	}
	e := errb.String()
	if !strings.Contains(e, "caboose: "+data+"/dot_local/linux-x64-musl also holds ") ||
		!strings.Contains(e, "delete it by hand if no image of yours needs it any more") ||
		strings.Contains(e, "dot_local/linux-arm64 also") {
		t.Errorf("stderr:\n%s", e)
	}
	if _, err := os.Stat(filepath.Join(data, "dot_local/linux-x64-musl/share/claude")); err != nil {
		t.Error("another platform's dir was touched")
	}
}

// caboose status reads the container's environment and lists its versions with
// bash alone: printenv and ls are not image requirements (a BYO image may
// lack them), bash is. The fake runs each `docker exec box bash -c` for
// real, in an environment of its own, and fails anything else it is asked
// to exec.
func TestStatusNeedsOnlyBash(t *testing.T) {
	tmp := t.TempDir()
	data := filepath.Join(tmp, "data")
	home := filepath.Join(tmp, "home")
	mkdirs(t, home, ".local/share/claude/versions/2.1.9", ".local/share/claude/versions/2.1.10")
	log := filepath.Join(tmp, "log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + log + `"
case "$*" in
  "inspect --type=container -f {{.State.Status}} box") echo running; exit 0 ;;
  "inspect --type=container box --format "*)
    printf '%s\t%s\n' /home/agent/.local/bin "` + data + `/dot_local/linux-arm64/bin" /work "` + tmp + `"; exit 0 ;;
esac
if [ "$1 $2 $3 $4" = "exec box bash -c" ]; then
  shift 4
  exec env -i HOME="` + home + `" TZ=Europe/Paris CABOOSE_KEEP_VERSIONS=3 bash -c "$1"
fi
[ "$1" = exec ] && exit 1
exit 0
`
	fake := filepath.Join(tmp, "docker")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	a := &App{
		Cfg: &config.Config{Container: "box", Image: "img", Roots: []config.Root{{Host: tmp, Container: "/work"}}, DataDir: data, KeepVersions: "2",
			Getenv: func(string) string { return "" }, TZ: "Europe/Paris"},
		Docker: &docker.CLI{Path: fake},
		Stdout: &out, Stderr: &errb,
	}
	if err := a.Status(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, w := range []string{
		"timezone  : Europe/Paris (host: Europe/Paris)\n",
		"(retaining 3):\n",
		"  2.1.10\n  2.1.9\n",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("status lacks %q:\n%s", w, s)
		}
	}
	if !strings.Contains(errb.String(), "container was created with CABOOSE_KEEP_VERSIONS=3, shell has 2.") {
		t.Errorf("stderr:\n%s", errb.String())
	}
	b, _ := os.ReadFile(log)
	for _, bad := range []string{"printenv", "ls -1"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("status ran %s:\n%s", bad, b)
		}
	}
}
