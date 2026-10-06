package launcher

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm"
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
