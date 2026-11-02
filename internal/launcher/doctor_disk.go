package launcher

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/vm"
)

// What doctor and status say of the VM disks: a stat of each disk file,
// never a walk, so that they stay fast whatever the checkpoint store
// holds. caboose disk measures the rest.

// Past these, doctor names a kind of disk with what frees it: a builder
// cache only grows, so a large one is worth a line; a leftover is worth
// one from a gigabyte.
const (
	builderCacheNote = 16 << 30
	leftoverNote     = 1 << 30
)

// vmDiskFile is one disk file of the data dir's vm/.
type vmDiskFile struct {
	path      string
	allocated int64
}

// vmDiskSummary is the environment's VM disks, by kind, and the leftovers
// doctor names.
type vmDiskSummary struct {
	total int64
	// byKind is the allocated size of each kind present, by its name in
	// caboose disk.
	byKind map[string]int64
	// builderCache is the builder's cache disk, when there is one.
	builderCache *vmDiskFile
	// staleScratch are the scratch disks of VMs not running.
	staleScratch []vmDiskFile
	// unusedRoots are the root disks no image or VM uses.
	unusedRoots []vmDiskFile
	// orphans are the volumes' disks no profile declares, by name.
	orphans map[string]vmDiskFile
}

// vmDisks gathers the summary; a data dir with no vm/ has none.
func (a *App) vmDisks() vmDiskSummary {
	s := vmDiskSummary{byKind: map[string]int64{}, orphans: map[string]vmDiskFile{}}
	root := a.vmRoot()
	stat := func(p string) (vmDiskFile, bool) {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return vmDiskFile{}, false
		}
		return vmDiskFile{path: p, allocated: diskAllocated(fi)}, true
	}
	add := func(kind string, f vmDiskFile) {
		s.byKind[kind] += f.allocated
		s.total += f.allocated
	}
	unused := map[string]bool{}
	for _, d := range a.vmImages().unused(root) {
		unused[d] = true
	}
	roots, _ := filepath.Glob(filepath.Join(root, "images", "root-*.img"))
	for _, p := range roots {
		if f, ok := stat(p); ok {
			add("root disks", f)
			if unused[p] {
				s.unusedRoots = append(s.unusedRoots, f)
			}
		}
	}
	empties, _ := filepath.Glob(filepath.Join(root, "images", "empty-ext4*.img"))
	for _, p := range empties {
		if f, ok := stat(p); ok {
			add("empty disks", f)
		}
	}
	scratch, _ := filepath.Glob(filepath.Join(root, "*", "scratch.img"))
	for _, p := range scratch {
		if f, ok := stat(p); ok {
			add("scratch disks", f)
			// Asked patiently: a vmm a Mac just woke may be slow to
			// answer, and a running VM's disk is no leftover.
			if !vm.Dir(filepath.Dir(p)).Answers() {
				s.staleScratch = append(s.staleScratch, f)
			}
		}
	}
	vols, _ := os.ReadDir(a.volumesDir())
	disks, _ := a.volumeDisks()
	for _, e := range vols {
		if f, ok := stat(filepath.Join(a.volumesDir(), e.Name())); ok {
			add("volumes", f)
		}
	}
	for name, d := range disks {
		if name != dockerVolume && len(a.declaring(name, false)) == 0 {
			s.orphans[name] = vmDiskFile{path: d.Path, allocated: d.Allocated}
		}
	}
	if f, ok := stat(filepath.Join(root, builderName, "cache.img")); ok {
		add("builder cache", f)
		s.builderCache = &f
	}
	return s
}

// line is the summary as one line: the total, then each kind.
func (s vmDiskSummary) line() string {
	var parts []string
	for _, k := range []string{"root disks", "empty disks", "scratch disks", "volumes", "builder cache"} {
		if n, ok := s.byKind[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %s", k, sizeString(n)))
		}
	}
	return fmt.Sprintf("%s allocated (%s)", sizeString(s.total), strings.Join(parts, ", "))
}

// sumAllocated is what files hold together.
func sumAllocated(files []vmDiskFile) int64 {
	var n int64
	for _, f := range files {
		n += f.allocated
	}
	return n
}

// doctorDisk is doctor's disk row: what the VM disks hold, and a line for
// each leftover past its threshold, with what frees it.
func (a *App) doctorDisk(c *checkup) {
	s := a.vmDisks()
	if len(s.byKind) == 0 {
		return
	}
	c.ok("disk", "VM disks: %s; 'caboose disk' shows the rest, checkpoints included, and what deleting each would free", s.line())
	if b := s.builderCache; b != nil && b.allocated >= builderCacheNote {
		c.note("disk", "the builder's cache, %s, holds %s and only grows: 'caboose prune --builder-cache' deletes it (asking first), and the next build makes it again, fetching everything anew", b.path, sizeString(b.allocated))
	}
	for _, f := range s.staleScratch {
		if f.allocated >= leftoverNote {
			c.note("disk", "%s, the scratch disk of a VM not running, holds %s: 'caboose prune' deletes it (a start makes a new one)", f.path, sizeString(f.allocated))
		}
	}
	if n := sumAllocated(s.unusedRoots); n >= leftoverNote {
		n1 := len(s.unusedRoots)
		c.note("disk", "%d root %s no image or VM uses %s %s: the next 'caboose build' deletes %s", n1, plural(n1, "disk", "disks"), plural(n1, "holds", "hold"), sizeString(n), plural(n1, "it", "them"))
	}
	for _, name := range slices.Sorted(maps.Keys(s.orphans)) {
		if f := s.orphans[name]; f.allocated >= leftoverNote {
			c.note("disk", "volume %s's disk holds %s and no profile declares it: 'caboose volume rm %s --delete-disk' deletes it", name, sizeString(f.allocated), name)
		}
	}
}

// diskStatusLine is status's disk line: the VM disks, and where the rest
// is; "" with none.
func (a *App) diskStatusLine() string {
	s := a.vmDisks()
	if len(s.byKind) == 0 {
		return ""
	}
	return fmt.Sprintf("VM disks %s allocated ('caboose disk' shows everything)", sizeString(s.total))
}
