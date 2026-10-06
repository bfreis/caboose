package launcher

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
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
}

func newBoxApp(t *testing.T, iso string, box *backendtest.Fake) *boxApp {
	t.Helper()
	tmp := t.TempDir()
	b := &boxApp{box: box, iso: iso, tmp: tmp, data: filepath.Join(tmp, "data"), engine: filepath.Join(tmp, "engine"),
		out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
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
  "info --format") case "$3" in *Runtimes*) cat "$d/runtimes" ;; *) echo Linux ;; esac; exit 0 ;;
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
		Cfg: &config.Config{Env: "default", Container: "box", Image: "img", Roots: []config.Root{{Host: tmp, Container: "/work"}},
			DataDir: b.data, KeepVersions: 2, ReadyTimeout: 0, Egress: true, Isolation: iso, Getenv: func(string) string { return "" }},
		Docker:   &docker.CLI{Path: fake},
		HostPart: func() string { return "laptop" },
		Backend:  box,
		Stdout:   b.out, Stderr: b.errb,
		// Where startLink would spawn caboose link, and findVMFiles look
		// beside the launcher: neither is this test binary's.
		Executable: func() (string, error) { return filepath.Join(tmp, "caboose"), nil },
		Spawn: func(exe string, args ...string) error {
			b.spawned = append(b.spawned, append([]string{exe}, args...))
			return nil
		},
		vmFiles: func() (vmFiles, error) { return vmFiles{Arch: runtime.GOARCH}, nil },
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

func (b *boxApp) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.engine, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// currentLabels are the labels of an image this launcher built, for
// platform, on the embedded Dockerfile's base, for this user.
func currentLabels(platform string) map[string]string {
	l := labelsFor(assets.BaseKindDefault, assets.BaseHash(), "")
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
		SandboxLabels: map[string]string{assets.LabelIsolation: iso, assets.LabelUser: "", assets.LabelRunArgs: "", assets.LabelHostname: hostnameOf(config.DefaultEnv, "laptop"),
			assets.LabelCompat: strconv.Itoa(assets.Compat), assets.LabelEgress: egressLabel(iso == isolationVM)}}
}

// mountLocal gives b's sandbox local (data-dir-relative) at ~/.local/bin,
// and b's root at /work, as a container created by this launcher has.
func (b *boxApp) mountLocal(local string) {
	b.box.SandboxMounts = append(b.box.SandboxMounts,
		mount(filepath.Join(b.data, local, "bin"), "/home/agent/.local/bin"),
		mount(b.tmp, "/work"))
}

func mount(src, dst string) backend.Mount { return backend.Mount{Source: src, Target: dst} }
