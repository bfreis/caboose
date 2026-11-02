package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/builder"
)

// Under vm, caboose logs is the VM's console: its last 100 lines, or
// --tail N's, and no docker logs flags, which there is no docker to take.
func TestVMLogs(t *testing.T) {
	box := runningBox(isolationVM)
	for i := 1; i <= 120; i++ {
		box.Log += "line " + strconv.Itoa(i) + "\n"
	}
	b := newBoxApp(t, isolationVM, box)
	if err := b.Logs(nil); err != nil {
		t.Fatal(err)
	}
	if out, _ := b.said(); !strings.HasPrefix(out, "line 21\n") || !strings.HasSuffix(out, "line 120\n") {
		t.Errorf("logs:\n%s", out)
	}
	for _, flag := range []string{"--tail", "-n"} {
		if err := b.Logs([]string{flag, "2"}); err != nil {
			t.Fatal(err)
		}
		if out, _ := b.said(); out != "line 119\nline 120\n" {
			t.Errorf("%s 2: %q", flag, out)
		}
	}
	for args, want := range map[string]string{
		"--tail x":      `--tail takes a number of lines, not "x"`,
		"--tail 0":      `--tail takes a number of lines, not "0"`,
		"--since 1h":    `under isolation vm, caboose logs takes only --tail N: there is no docker logs to pass "--since 1h" to`,
		"-f --tail 3 x": `there is no docker logs to pass "-f --tail 3 x" to`,
	} {
		if err := b.Logs(strings.Fields(args)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", args, err)
		}
	}
	if !slices.Equal(box.Calls, []string{"logs 100", "logs 2", "logs 2"}) {
		t.Errorf("calls %q", box.Calls)
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int{"8G": 8192, "8GiB": 8192, "8gb": 8192, "4096M": 4096, "4096MiB": 4096, "4096": 4096} {
		if got, err := parseMemory(in); err != nil || got != want {
			t.Errorf("%q: %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "lots", "0", "-1G", "256M", "8T"} {
		if _, err := parseMemory(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestVMSize(t *testing.T) {
	cpus, mem, err := vmSize(&config.Config{})
	if err != nil || cpus < 1 || mem < 512 || mem > 8192 {
		t.Errorf("defaults: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
	cpus, mem, err = vmSize(&config.Config{VMCPUs: 3, VMMemory: "6G"})
	if err != nil || cpus != 3 || mem != 6144 {
		t.Errorf("configured: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
	if _, _, err := vmSize(&config.Config{VMMemory: "lots"}); err == nil {
		t.Error("memory \"lots\" accepted")
	}
}

// The store answers as docker does: an image's ID and labels by name, none
// once its disk is gone; and a prune keeps the disks a record or a VM
// names.
func TestVMImageStore(t *testing.T) {
	root := t.TempDir()
	s := &vmImageStore{dir: filepath.Join(root, "images")}
	if _, exists, err := s.ImageLabels("caboose"); exists || err != nil {
		t.Fatalf("empty store: %v, %v", exists, err)
	}
	rec := vmImage{Name: "ghcr.io/x/caboose:1", ID: "sha256:0123456789abcdef0123", Labels: map[string]string{"a": "b"},
		Disk: diskName("sha256:0123456789abcdef0123")}
	if rec.Disk != "root-0123456789abcdef.img" {
		t.Errorf("disk name %q", rec.Disk)
	}
	if err := s.put(rec); err != nil {
		t.Fatal(err)
	}
	if _, exists, _ := s.ImageLabels(rec.Name); exists {
		t.Error("an image whose disk is missing exists")
	}
	for _, d := range []string{rec.Disk, "root-old.img", "root-inuse.img"} {
		if err := os.WriteFile(filepath.Join(s.dir, d), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	labels, exists, err := s.ImageLabels(rec.Name)
	if err != nil || !exists || labels["a"] != "b" || s.ImageID(rec.Name) != rec.ID {
		t.Errorf("recorded: %v %v %v %q", labels, exists, err, s.ImageID(rec.Name))
	}
	if s.recordPath("a/b") == s.recordPath("a_b") {
		t.Error("two names share a record")
	}
	box := vm.Dir(filepath.Join(root, "box"))
	if err := box.WriteState(vm.State{Machine: vm.Machine{Disks: []vm.Disk{{Path: filepath.Join(s.dir, "root-inuse.img")}}}}); err != nil {
		t.Fatal(err)
	}
	s.prune(root)
	for d, want := range map[string]bool{rec.Disk: true, "root-inuse.img": true, "root-old.img": false} {
		if _, err := os.Stat(filepath.Join(s.dir, d)); (err == nil) != want {
			t.Errorf("%s kept: %v, want %v", d, err == nil, want)
		}
	}
}

// scratchHost is a vmHost over a data dir with an empty-disk template of
// the size the builder makes (a sparse file: the host never formats one),
// and cfg.
func scratchHost(t *testing.T, cfg *config.Config) (*vmHost, string) {
	t.Helper()
	cfg.DataDir = t.TempDir()
	h := &vmHost{a: &App{Cfg: cfg}}
	tmpl := h.a.vmImages().template()
	if err := os.MkdirAll(filepath.Dir(tmpl), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(config.EmptyDiskSize); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return h, filepath.Join(t.TempDir(), "scratch.img")
}

// The scratch disk is scratch_size long, 64G without one, and made anew
// over what is there; a size is read at every start.
func TestNewScratchSize(t *testing.T) {
	for _, tc := range []struct {
		size string
		want int64
	}{{"", 64 << 30}, {"4G", 4 << 30}, {"200G", 200 << 30}, {"1024G", 1024 << 30}} {
		h, path := scratchHost(t, &config.Config{VMScratchSize: tc.size})
		if err := os.WriteFile(path, []byte("an old scratch disk"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.NewScratch(path); err != nil {
			t.Fatalf("%q: %v", tc.size, err)
		}
		if fi, err := os.Stat(path); err != nil || fi.Size() != tc.want {
			t.Errorf("scratch_size %q: %v, %v; want %d", tc.size, fi, err, tc.want)
		}
		// Another size at the next start: the config is read again.
		h.a.Cfg.VMScratchSize = "8G"
		if err := h.NewScratch(path); err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(path); fi.Size() != 8<<30 {
			t.Errorf("after scratch_size 8G: %d", fi.Size())
		}
	}
}

// A scratch disk is refused, saying where, when its size cannot be had:
// a size outside what is allowed, or a template longer than the size; a
// refused one leaves no disk at its path, the last start's included, for
// a VM to boot on.
func TestNewScratchRefuses(t *testing.T) {
	h, path := scratchHost(t, &config.Config{Profile: "vm.big", VMScratchSize: "2G"})
	if err := h.NewScratch(path); err == nil || !strings.Contains(err.Error(), "scratch_size in [vm.big]") {
		t.Errorf("2G: %v", err)
	}
	h.a.Cfg.VMScratchSize = ""
	if err := os.Truncate(h.a.vmImages().template(), config.DefaultScratchSize+1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("the last start's scratch disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.NewScratch(path); err == nil || !strings.Contains(err.Error(), "shorter than the empty disk") {
		t.Errorf("a template longer than the size: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused scratch disk left a disk at its path: %v", err)
	}
}

// templateGuest is the builder guest as vmTemplate uses it: Template
// writes the empty disk Start attached, of the builder's size.
type templateGuest struct {
	t           *testing.T
	boots       int
	out, path   string
	made, stops int
	err         error
	// final is where the template goes, and early is set when it was
	// there before the builder stopped.
	final string
	early bool
}

func (g *templateGuest) Build(builder.Request) (builder.Result, error) {
	g.t.Error("a build, where only the template was asked for")
	return builder.Result{}, nil
}

func (g *templateGuest) Template() error {
	g.made++
	if g.err != nil {
		return g.err
	}
	return os.Truncate(g.path, config.EmptyDiskSize)
}

// Stop notes whether the template was in place before the builder
// stopped: the disk is the builder's until then.
func (g *templateGuest) Stop() error {
	if g.stops == 0 && g.final != "" && isFile(g.final) {
		g.early = true
	}
	g.stops++
	return nil
}

func (g *templateGuest) boot(out, template string) (vmBuildGuest, error) {
	g.boots++
	g.out, g.path = out, template
	return g, os.WriteFile(template, nil, 0o600)
}

// A start with no template makes it first, in the builder, saying so,
// and later starts use it; a volume's disk does the same.
func TestNewScratchMakesTemplate(t *testing.T) {
	h, path := scratchHost(t, &config.Config{})
	tmpl := h.a.vmImages().template()
	if err := os.Remove(tmpl); err != nil {
		t.Fatal(err)
	}
	var errb strings.Builder
	h.a.Stderr = &errb
	g := &templateGuest{t: t, final: tmpl}
	h.a.buildGuest = g.boot
	if err := h.NewScratch(path); err != nil {
		t.Fatal(err)
	}
	if g.boots != 1 || g.out != "" || g.path == "" || g.made != 1 || g.stops == 0 || g.early {
		t.Errorf("the builder: %+v", g)
	}
	if fi, err := os.Stat(tmpl); err != nil || fi.Size() != config.EmptyDiskSize {
		t.Fatalf("the template: %v, %v", fi, err)
	}
	if _, err := os.Stat(g.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the template's temporary disk is still there: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != config.DefaultScratchSize {
		t.Errorf("the scratch disk: %v, %v", fi, err)
	}
	if !strings.Contains(errb.String(), "the empty disk scratch disks and volumes are made from, in the builder guest") {
		t.Errorf("said: %q", errb.String())
	}
	if err := h.NewScratch(path); err != nil || g.boots != 1 {
		t.Errorf("the next start: %v, %d builder boots", err, g.boots)
	}

	if err := os.Remove(tmpl); err != nil {
		t.Fatal(err)
	}
	if p, err := h.Volume("docker"); err != nil || g.boots != 2 || !isFile(p) {
		t.Errorf("a volume with no template: %q, %v, %d builder boots", p, err, g.boots)
	}
}

// A template the builder fails to make fails the start, saying what it
// was making, and leaves nothing behind to be taken for one.
func TestNewScratchTemplateFails(t *testing.T) {
	h, path := scratchHost(t, &config.Config{})
	tmpl := h.a.vmImages().template()
	if err := os.Remove(tmpl); err != nil {
		t.Fatal(err)
	}
	h.a.Stderr = &strings.Builder{}
	g := &templateGuest{t: t, err: errors.New("mkfs failed")}
	h.a.buildGuest = g.boot
	if err := h.NewScratch(path); err == nil || !strings.Contains(err.Error(), "the empty disk for scratch disks and volumes: mkfs failed") {
		t.Errorf("%v", err)
	}
	for _, p := range []string{tmpl, g.path, path} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// doctor names a missing template and what makes it, and a scratch_size
// past the space the data dir's volume has, which it allows.
func TestDoctorVMDisks(t *testing.T) {
	h, _ := scratchHost(t, &config.Config{VMScratchSize: "64G"})
	free := vmFreeSpace
	t.Cleanup(func() { vmFreeSpace = free })
	vmFreeSpace = func(string) (int64, error) { return 1 << 40, nil }
	doctor := func() *checkup {
		c := &checkup{}
		h.a.doctorVMDisks(c)
		return c
	}
	if c := doctor(); c.row(levelOK, "a scratch disk of up to 64G at each start") == nil {
		t.Errorf("fits:\n%s", c)
	}
	vmFreeSpace = func(string) (int64, error) { return 10<<30 + 1<<29, nil }
	if c := doctor(); c.row(levelNote, "scratch_size is 64G, and the volume of the data dir") == nil || c.row(levelNote, "has 10.5G free") == nil {
		t.Errorf("past the free space:\n%s", c)
	}
	if err := os.Remove(h.a.vmImages().template()); err != nil {
		t.Fatal(err)
	}
	if c := doctor(); c.row(levelNote, "is missing: the next start makes it in the builder guest, or 'caboose build' now") == nil {
		t.Errorf("no template:\n%s", c)
	}
}

// A new volume is volumeSize long, made from the same template, and an
// existing one is left as it is.
func TestVolumeSize(t *testing.T) {
	h, _ := scratchHost(t, &config.Config{})
	p, err := h.Volume("docker")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Size() != volumeSize {
		t.Fatalf("%v, %v", fi, err)
	}
	if err := os.Truncate(p, 10<<30); err != nil {
		t.Fatal(err)
	}
	if q, err := h.Volume("docker"); err != nil || q != p {
		t.Fatalf("%q, %v", q, err)
	}
	if fi, _ := os.Stat(p); fi.Size() != 10<<30 {
		t.Errorf("an existing volume was made again: %d", fi.Size())
	}
}

// The template's name carries its size, so one of another size is never
// taken for it.
func TestTemplateNameHasItsSize(t *testing.T) {
	s := &vmImageStore{dir: "/images"}
	if got, want := s.template(), "/images/empty-ext4-4g.img"; got != want {
		t.Errorf("template %q, want %q", got, want)
	}
}
