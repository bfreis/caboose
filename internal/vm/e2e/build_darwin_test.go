package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/builder"
)

// TestBuild builds caboose's own image in the builder guest, from the
// contexts this binary embeds (as the launcher's), boots a sandbox VM from
// the root disk it wrote, and builds again warm. It needs DIR's
// builder-arm64.img too (make vm-builder).
func TestBuild(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	work, err := os.MkdirTemp("/tmp", "cvb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	h := host{dir: dir, work: work}
	initramfs, err := h.initramfs()
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	// The cache is kept next to DIR's files, so a second run is warm.
	b := &builder.Builder{
		Dir:    vm.Dir(filepath.Join(dir, "builder")),
		Kernel: filepath.Join(dir, "Image"), Initramfs: initramfs,
		Image: filepath.Join(dir, "builder-arm64.img"),
		CPUs:  min(runtime.NumCPU(), 8), MemoryMiB: 8192,
		StartVMM: h.StartVMM, Self: self, Stderr: &logWriter{t: t},
	}
	keepLogs := func() {
		for _, f := range []string{b.Dir.Log(), b.Dir.Console()} {
			if data, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "builder-"+filepath.Base(f)), data, 0o644)
			}
		}
	}
	defer keepLogs()

	base, layer := filepath.Join(work, "base"), filepath.Join(work, "layer")
	for d, write := range map[string]func(string) error{base: assets.WriteBaseContext, layer: assets.WriteLayerContext} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := write(d); err != nil {
			t.Fatal(err)
		}
	}
	probe, err := assets.ProbeScript()
	if err != nil {
		t.Fatal(err)
	}
	root, empty := filepath.Join(work, "root.img"), filepath.Join(work, "empty.img")
	req := builder.Request{
		BaseContext: base, BaseTag: "caboose-e2e-base",
		Probe: probe, UID: os.Getuid(), GID: os.Getgid(),
		LayerContext: layer, LayerFile: assets.LayerDockerfile, Image: "caboose-e2e",
	}

	build := func(name, template string) builder.Result {
		t.Helper()
		start := time.Now()
		g, err := b.Start(root, template)
		if err != nil {
			t.Fatalf("%s: starting the builder: %v", name, err)
		}
		defer g.Stop()
		step(t, start, "%s: the builder is up", name)
		r, err := g.Build(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		step(t, start, "%s: built %s on %s (%s); config %+v", name, r.ImageID[:19], r.Base, r.BaseID[:19], r.Config)
		if template != "" {
			if err := g.Template(); err != nil {
				t.Fatalf("%s: the template: %v", name, err)
			}
		}
		if err := g.Stop(); err != nil {
			t.Fatalf("%s: stopping the builder: %v", name, err)
		}
		step(t, start, "%s: done", name)
		return r
	}
	r := build("first build", empty)
	for _, f := range []string{root, empty} {
		var st os.FileInfo
		if st, err = os.Stat(f); err != nil {
			t.Fatal(err)
		}
		step(t, time.Now(), "%s: %d MiB apparent", filepath.Base(f), st.Size()>>20)
	}

	// The sandbox, from the root disk the builder wrote and a scratch disk
	// cloned from its template: the image's env, but a sleep for its
	// entrypoint, which would install Claude Code.
	h.root, h.empty, h.env = root, empty, r.Config.Env
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e", vmdir, h)
	v.Self = self
	v.Ready = ready
	defer v.Remove()
	start := time.Now()
	if err := v.Create(backend.Spec{Image: "e2e", Hostname: "caboose", User: "0:0", Env: []string{"IS_SANDBOX=1"}}); err != nil {
		t.Fatalf("booting the built image: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	step(t, start, "the built image booted")
	out := func(argv ...string) string {
		t.Helper()
		s, err := backend.Output(v, argv...)
		if err != nil {
			t.Errorf("%q: %v", argv, err)
		}
		return s
	}
	uid := out("id", "-u", "agent")
	if want := strconv.Itoa(os.Getuid()); uid != want {
		t.Errorf("the agent user is %s, want %s", uid, want)
	}
	if got := out("stat", "-c", "%u", "/home/agent"); got != uid {
		t.Errorf("/home/agent is %s's, want %s's", got, uid)
	}
	if got := out("sh", "-c", "test -x /usr/local/bin/caboose-entrypoint && test -x /usr/local/bin/caboose-agent && echo yes"); got != "yes" {
		t.Errorf("the entrypoint and agent: %q", got)
	}
	if got := out("sh", "-c", "echo $PATH"); !strings.HasPrefix(got, "/home/agent/.local/bin:") {
		t.Errorf("PATH %q", got)
	}
	step(t, start, "tools: %s", out("sh", "-c", "git --version; tmux -V; bash --version | head -1; ls /usr/local/go/bin 2>/dev/null | tr '\\n' ' '"))
	step(t, start, "root disk: %s", out("sh", "-c", "df -h / /tmp 2>/dev/null | tail -n +2 | tr -s ' ' | tr '\\n' ';'"))
	if err := v.Remove(); err != nil {
		t.Fatal(err)
	}

	// A warm build: what a caboose update costs.
	build("warm build", "")
}

// logWriter sends the builder's output to the test log, a line at a time.
type logWriter struct {
	t   *testing.T
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			return len(p), nil
		}
		w.t.Logf("  | %s", w.buf[:i])
		w.buf = w.buf[i+1:]
	}
}
