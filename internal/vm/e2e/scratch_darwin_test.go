package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm"
)

// allocated is what a file takes on this Mac's disk, as ls -s counts it.
func allocated(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

// TestScratch boots a VM with a scratch disk and a volume of sizes of
// their own, and checks that the guest's filesystems fill them, that what
// the guest deletes comes back to this Mac, and that a stop takes the
// scratch disk away and leaves the volume.
func TestScratch(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	// An empty disk of another size, as the 64G one disks were cloned
	// from before they were grown, would make every disk wrong.
	empty := host{dir: dir}.emptyDisk()
	if fi, err := os.Stat(empty); err != nil {
		t.Fatal(err)
	} else if fi.Size() != config.EmptyDiskSize {
		t.Fatalf("%s is %d bytes: the disks are made from the %dG empty ext4 the builder makes; copy a data dir's vm/images/empty-ext4-%dg.img over it",
			empty, fi.Size(), config.EmptyDiskSize>>30, config.EmptyDiskSize>>30)
	}
	work, err := os.MkdirTemp("/tmp", "cvs")
	if err != nil {
		t.Fatal(err)
	}
	scratch, volume := int64(16<<30), int64(8<<30)
	// CABOOSE_VM_E2E_SCRATCH_GIB tries another scratch size, up to
	// config.MaxScratchSize.
	if g, err := strconv.ParseInt(os.Getenv("CABOOSE_VM_E2E_SCRATCH_GIB"), 10, 64); err == nil {
		scratch = g << 30
	}
	h := host{dir: dir, work: work, scratch: scratch, volSize: volume}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-scratch", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "scratch-"+filepath.Base(f)), b, 0o644)
			}
		}
		_ = v.Remove()
		os.RemoveAll(work)
	}()
	if err := v.Create(backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Volumes: []backend.Volume{{Name: "vol", Target: "/vol"}},
		Labels:  []string{"caboose.isolation=vm"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	out := func(argv ...string) string {
		t.Helper()
		s, err := backend.Output(v, argv...)
		if err != nil {
			t.Errorf("%q: %v", argv, err)
		}
		return s
	}
	num := func(s string) int64 {
		t.Helper()
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			t.Fatalf("%q is no number", s)
		}
		return n
	}

	// The disks have the sizes asked for, and the filesystems fill them: a
	// filesystem keeps its own overhead (the journal and inode tables take
	// 3.5% of 4G), so "fill" is within 6%.
	for dev, want := range map[string]int64{"vdb": scratch, "vdc": volume} {
		if got := num(out("cat", "/sys/block/"+dev+"/size")) * 512; got != want {
			t.Errorf("%s is %d bytes, want %d", dev, got, want)
		}
	}
	for path, want := range map[string]int64{"/": scratch, "/vol": volume} {
		got := num(out("sh", "-c", "echo $(( $(stat -f -c %b "+path+") * $(stat -f -c %S "+path+") ))"))
		if got < want-want/16 || got > want {
			t.Errorf("the filesystem at %s has %d bytes, want about %d", path, got, want)
		}
	}
	// ext4's own record of each mount's options: the scratch disk's mount
	// is out of the sandbox's reach once the root has switched.
	for _, dev := range []string{"vdb", "vdc"} {
		if o := out("cat", "/proc/fs/ext4/"+dev+"/options"); !slices.Contains(strings.Fields(o), "discard") {
			t.Errorf("%s is not mounted with discard: %q", dev, o)
		}
	}
	scratchImg, volImg := vmdir.Scratch(), filepath.Join(work, "volumes", "vol.img")
	bootS, bootV := allocated(t, scratchImg), allocated(t, volImg)
	t.Logf("allocated after boot: scratch %d MiB, volume %d MiB", bootS>>20, bootV>>20)

	// A write takes space on this Mac; deleting it gives the space back,
	// with no trim by anyone.
	const data = 512 << 20
	for _, c := range []struct{ img, path string }{{scratchImg, "/var/tmp/big"}, {volImg, "/vol/big"}} {
		before := allocated(t, c.img)
		out("sh", "-c", "dd if=/dev/urandom of="+c.path+" bs=1M count=512 2>/dev/null; sync")
		full := allocated(t, c.img)
		if full-before < data*9/10 {
			t.Errorf("%s: %d MiB written took %d MiB", c.img, data>>20, (full-before)>>20)
		}
		out("sh", "-c", "rm "+c.path+"; sync")
		deadline := time.Now().Add(20 * time.Second)
		var after int64
		for {
			if after = allocated(t, c.img); after-before < data/10 || time.Now().After(deadline) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Logf("%s: allocated %d MiB, +512 MiB written %d MiB, deleted %d MiB", filepath.Base(c.img), before>>20, full>>20, after>>20)
		if after-before >= data/10 {
			t.Errorf("%s: deleting what was written gave back only %d of %d MiB", c.img, (full-after)>>20, data>>20)
		}
	}

	if err := v.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(scratchImg); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the scratch disk is still there after a stop: %v", err)
	}
	if _, err := os.Stat(volImg); err != nil {
		t.Errorf("the volume's disk is gone after a stop: %v", err)
	}
}
