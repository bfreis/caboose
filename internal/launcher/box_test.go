package launcher

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/saferm"
)

// isolations are what a scenario about the sandbox runs under: each has
// its own words for it, and its own image store.
var isolations = []string{isolationContainer, isolationGVisor, isolationVM}

// boxImageID is the ID of the image a boxApp has.
const boxImageID = "sha256:0123456789abcdef0123"

// boxApp is an App whose sandbox is box, a backendtest.Fake, under
// isolation iso. Its image store holds image img, current, for
// linux-arm64 (setImage changes that). Under docker and gvisor the engine
// is a fake docker that answers for images, runtimes (runc and runsc) and
// the isolation's probe (the agent can write) only; under vm there is no
// engine. Anything else asked of docker -- the sandbox itself, or any
// engine under vm -- fails the test: the launcher reaches the sandbox
// only through Backend.
type boxApp struct {
	*App
	box       *backendtest.Fake
	iso       string
	tmp, data string
	engine    string // the fake engine's files
	out, errb *bytes.Buffer
	spawned   [][]string
	daemon    *fakeDaemon // the daemon a launch asks for the link
}

// shortTempDir is a new directory directly under the temp dir, with a
// short name, as its physical path, removed (and only it) when the test
// ends: t.TempDir's names carry the test's, which can leave no room for a
// Unix socket's path (at most 103 bytes on a Mac, whose temp dir is
// already about 50).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := saferm.RemoveRoot(dir); err != nil {
			t.Errorf("removing %s: %v", dir, err)
		}
	})
	// Physical, as the working directory and every path the code
	// resolves are: the temp dir may be reached through a symlink (a
	// Mac's /var is /private/var).
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// physicalTempDir is t.TempDir as its physical path, as the working
// directory and every path the code resolves are: the temp dir may be
// reached through a symlink (a Mac's /var is /private/var).
func physicalTempDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newBoxApp(t *testing.T, iso string, box *backendtest.Fake) *boxApp {
	t.Helper()
	// Status and doctor say the same on every system: Linux's, unless a
	// test pins the Mac (pinHost).
	pinHost(t, false)
	// Short, so that the sockets made under it (the daemon's, a VM's)
	// fit a Mac's sun_path wherever the temp dir is.
	tmp := shortTempDir(t)
	b := &boxApp{box: box, iso: iso, tmp: tmp, data: filepath.Join(tmp, "data"), engine: filepath.Join(tmp, "engine"),
		out: &bytes.Buffer{}, errb: &bytes.Buffer{}, daemon: &fakeDaemon{}}
	if err := os.Mkdir(b.engine, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
d="` + b.engine + `"
`
	if iso != isolationVM {
		script += `case "$1 $2" in
  "image inspect") [ -f "$d/labels" ] || { echo "Error: No such image: $5" >&2; exit 1; }; cat "$d/labels"; exit 0 ;;
  "inspect --type=image") [ -f "$d/id" ] || exit 1; cat "$d/id"; exit 0 ;;
  "info --format") case "$3" in *Runtimes*) cat "$d/runtimes" ;; *) cat "$d/os" 2>/dev/null || echo Linux ;; esac; exit 0 ;;
  "run --rm") printf '%s\n' "$@" > "$d/probe"; cat "$d/probe-answer"; exit 0 ;;
  "version "*) exit 0 ;;
esac
`
		b.write(t, "runtimes", withRunsc)
		b.write(t, "probe-answer", "yes")
	}
	script += `printf '%s\n' "$*" >> "$d/stray"
echo "not the engine's to answer" >&2
exit 99
`
	fake := filepath.Join(tmp, "docker")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.App = &App{
		Cfg: &config.Config{Env: "default", Container: "box", Trust: config.TrustFallible, Image: "img", Roots: []config.Root{{Host: tmp, Container: "/work"}},
			DataDir: b.data, KeepVersions: 2, ReadyTimeout: 0, Egress: true, Isolation: iso, Getenv: func(string) string { return "" },
			ImageProfile: config.ImageProfile{Kind: config.ImageKindRef, Name: "default", Ref: boxBase}},
		Docker:   &docker.CLI{Path: fake},
		HostPart: func() string { return "laptop" },
		Backend:  box,
		Stdout:   b.out, Stderr: b.errb,
		// Where findVMFiles looks
		// beside the launcher: neither is this test binary's.
		Executable: func() (string, error) { return filepath.Join(tmp, "caboose"), nil },
		Spawn: func(exe string, args ...string) error {
			b.spawned = append(b.spawned, append([]string{exe}, args...))
			return nil
		},
		vmFiles: func() (vmFiles, error) { return vmFiles{Arch: runtime.GOARCH}, nil },
		// Where startLink would ask the daemon for the link: never a real one.
		DaemonDial: b.daemon.dial,
		// The host's zone is the test's, never the machine's /etc/localtime
		// or /etc/timezone: none, unless a test says otherwise.
		tzReadlink: func(string) (string, error) { return "", fs.ErrNotExist },
		tzReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		// Nor this machine's global git config (the host's signing):
		// none, unless a test says so. Its ssh is TestMain's (hostSSH).
		hostGit: noGlobalGit,
	}
	b.setImage(t, boxImageID, currentLabels("linux-arm64"))
	box.ImageFor = func(name string) (string, map[string]string) {
		labels, _, _ := b.images().ImageLabels(name)
		return b.images().ImageID(name), labels
	}
	t.Cleanup(func() {
		if s, err := os.ReadFile(filepath.Join(b.engine, "stray")); err == nil {
			t.Errorf("under %s, docker was asked what is not the engine's:\n%s", iso, s)
		}
	})
	return b
}

// pinHost has the launcher see the host facts of Linux, or of a Mac
// with darwin, whatever the build's GOOS and this machine
// (PlayHostForTests), restored at the test's end; no test of this
// package runs in parallel, so none sees another's.
func pinHost(t *testing.T, darwin bool) {
	t.Helper()
	os_ := "linux"
	if darwin {
		os_ = "darwin"
	}
	t.Cleanup(PlayHostForTests(os_))
}

// noGlobalGit is the host's git with no global config: the sandbox's
// config files are read with it, never the machine's ~/.gitconfig.
func noGlobalGit() datadir.Git {
	g := datadir.FindGit()
	if g == nil {
		return nil
	}
	return globalless{g}
}

type globalless struct{ datadir.Git }

func (globalless) GetGlobal(string) string { return "" }

func (b *boxApp) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.engine, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// boxBase is a boxApp's base, a ref: the fake engine gives every image
// the ID boxImageID.
const boxBase = "base:box"

// currentLabels are the labels of an image this launcher built, for
// platform, on a boxApp's base, for this user.
func currentLabels(platform string) map[string]string {
	l := labelsFor(assets.BaseKindRef, "", boxBase)
	l[assets.LabelBaseID] = boxImageID
	l[assets.LabelPlatform] = platform
	l[assets.LabelCompat] = strconv.Itoa(assets.Compat)
	return l
}

// setImage makes the store's image img have id and labels; a nil labels
// is an image caboose build did not label at all.
func (b *boxApp) setImage(t *testing.T, id string, labels map[string]string) {
	t.Helper()
	if b.iso != isolationVM {
		j, err := json.Marshal(labels)
		if err != nil {
			t.Fatal(err)
		}
		b.write(t, "labels", string(j))
		b.write(t, "id", id)
		return
	}
	store := b.vmImages()
	rec := vmImage{Name: b.Cfg.Image, ID: id, Labels: labels, Disk: diskName(id)}
	if err := store.put(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.disk(rec), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// noun is what the sandbox is called under b's isolation.
func (b *boxApp) noun() string {
	if b.iso == isolationVM {
		return "VM"
	}
	return "container"
}

// said is what b wrote on stdout and stderr since the last reset, which
// it does.
func (b *boxApp) said() (out, errs string) {
	out, errs = b.out.String(), b.errb.String()
	b.out.Reset()
	b.errb.Reset()
	return out, errs
}

// runningBox is a sandbox that runs, created under iso from boxImageID,
// with the platform dir local (data-dir-relative) at ~/.local and tmp at
// /work -- which boxApp's root is, once it has one.
func runningBox(iso string) *backendtest.Fake {
	return &backendtest.Fake{Status: "running", ImageID: boxImageID,
		SandboxLabels: map[string]string{assets.LabelIsolation: iso, assets.LabelProfile: iso, assets.LabelUser: "", assets.LabelRunArgs: "", assets.LabelHostname: hostnameOf(config.DefaultEnv, "laptop"),
			assets.LabelCompat: strconv.Itoa(assets.Compat), assets.LabelEgress: egressLabel(iso == isolationVM),
			assets.LabelPluginDirs: datadir.PluginDirs, assets.LabelTrust: config.TrustFallible, assets.LabelNetwork: map[bool]string{true: "nat"}[iso == isolationVM]}}
}

// mountLocal gives b's sandbox local (data-dir-relative) at ~/.local/bin,
// b's root at /work and caboose's instructions, as a container created by
// this launcher has.
func (b *boxApp) mountLocal(local string) {
	b.box.SandboxMounts = append(b.box.SandboxMounts,
		mount(filepath.Join(b.data, local, "bin"), "/home/agent/.local/bin"),
		mount(b.tmp, "/work"),
		backend.Mount{Source: filepath.Join(b.data, datadir.ManagedDir), Target: datadir.ManagedTarget, ReadOnly: true})
}

func mount(src, dst string) backend.Mount { return backend.Mount{Source: src, Target: dst} }
