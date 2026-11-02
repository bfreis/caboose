package launcher

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
)

// A volume's disk can have files beside it, in vm/volumes, that are no
// disk of a volume and that nothing deletes on its own: the disk as it was
// before a checkpoint's restore (NAME.img.before-restore), a shrink's
// backup of it (NAME.img.before-shrink), and the copy of a checkpoint a
// restore interrupted before its last step left
// (.NAME.img.restoring-<ns>). `caboose volume ls` and `caboose prune`
// name them, with their sizes and how to remove them.

// The kinds of leftover file.
const (
	leftoverBeforeRestore = "before-restore"
	leftoverBeforeShrink  = "before-shrink"
	leftoverRestoring     = "restoring"
)

// listedLeftover is one of those files.
type listedLeftover struct {
	Path      string `json:"path"`
	Volume    string `json:"volume"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	Allocated int64  `json:"allocated"`
	// SameAsDisk: a before-restore that is the volume's disk itself under
	// a second name, which a restore interrupted between its two steps
	// leaves.
	SameAsDisk bool `json:"same_as_disk,omitempty"`
}

// leftoverOf says which volume's leftover file name is, and of which
// kind.
func leftoverOf(file string) (volume, kind string, ok bool) {
	if v, found := strings.CutSuffix(file, ".img"+checkpoint.BeforeRestoreSuffix); found && validDiskName(v) {
		return v, leftoverBeforeRestore, true
	}
	if v, found := strings.CutSuffix(file, ".img"+backend.BackupSuffix); found && validDiskName(v) {
		return v, leftoverBeforeShrink, true
	}
	if rest, found := strings.CutPrefix(file, checkpoint.RestoringPrefix); found {
		if v, _, found := strings.Cut(rest, ".img"+checkpoint.RestoringInfix); found && validDiskName(v) && checkpoint.IsRestoringOf(file, v) {
			return v, leftoverRestoring, true
		}
	}
	return "", "", false
}

func validDiskName(name string) bool {
	return name == dockerVolume || config.ValidVolumeName(name)
}

// volumeLeftovers is the leftover files in vm/volumes, by name.
func (a *App) volumeLeftovers() ([]listedLeftover, error) {
	entries, err := os.ReadDir(a.volumesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []listedLeftover
	for _, e := range entries {
		vol, kind, ok := leftoverOf(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		l := listedLeftover{Path: a.volumesDir() + "/" + e.Name(), Volume: vol, Kind: kind, Size: fi.Size(), Allocated: diskAllocated(fi)}
		if kind == leftoverBeforeRestore {
			if di, err := os.Stat(a.volumeDisk(vol)); err == nil && os.SameFile(di, fi) {
				l.SameAsDisk = true
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// leftoversOf is ls's leftovers of volume name.
func leftoversOf(ls []listedLeftover, name string) []listedLeftover {
	return slices.DeleteFunc(slices.Clone(ls), func(l listedLeftover) bool { return l.Volume != name })
}

// leftoverLine says what l is, its size, and how to remove it.
func (a *App) leftoverLine(l listedLeftover) string {
	size := fmt.Sprintf("%s, %s allocated", gibString(l.Size), sizeString(l.Allocated))
	disk := a.volumeDisk(l.Volume)
	switch {
	case l.Kind == leftoverBeforeRestore && l.SameAsDisk:
		return fmt.Sprintf("%s is volume %s's disk itself under a second name, which a restore interrupted before its last step left: 'rm %s' removes the name, not the disk (the next restore removes it too)", l.Path, l.Volume, l.Path)
	case l.Kind == leftoverBeforeRestore:
		return fmt.Sprintf("%s (%s) is volume %s's disk as it was before a checkpoint's restore: once the volume is right, 'rm %s' deletes it; to undo the restore instead, with the VM stopped, 'mv %s %s'", l.Path, size, l.Volume, l.Path, l.Path, disk)
	case l.Kind == leftoverBeforeShrink:
		return fmt.Sprintf("%s (%s) is a shrink's backup of volume %s's disk: 'rm %s' deletes it once you no longer need it", l.Path, size, l.Volume, l.Path)
	default:
		return fmt.Sprintf("%s (%s) is a copy of a checkpoint that an interrupted restore of volume %s left (the checkpoint keeps it): 'rm %s' deletes it", l.Path, size, l.Volume, l.Path)
	}
}

// errRestoreLeft is a start refused because volume name has no disk but
// an interrupted restore's files: making an empty disk would hide them.
var errRestoreLeft = errors.New("an interrupted restore left the volume without its disk")

// checkRestoreLeft refuses a start that would make volume name's disk,
// missing, while an interrupted restore left the disk as it was
// (NAME.img.before-restore) or a checkpoint's copy beside it: it says how
// to put either in place, or to start the volume empty.
func (a *App) checkRestoreLeft(name string) error {
	all, err := a.volumeLeftovers()
	if err != nil {
		return fmt.Errorf("volume %s: listing %s: %v", name, a.volumesDir(), err)
	}
	var before string
	var copies []string
	for _, l := range leftoversOf(all, name) {
		switch l.Kind {
		case leftoverBeforeRestore:
			before = l.Path
		case leftoverRestoring:
			copies = append(copies, l.Path)
		}
	}
	if before == "" && len(copies) == 0 {
		return nil
	}
	disk := a.volumeDisk(name)
	var b strings.Builder
	fmt.Fprintf(&b, "volume %s has no disk (%s), and a start would make an empty one: %v. ", name, disk, errRestoreLeft)
	rmCopies := ""
	if len(copies) > 0 {
		rmCopies = "'rm " + strings.Join(copies, " ") + "'"
	}
	if before != "" {
		fmt.Fprintf(&b, "%s is the disk as it was before the restore: 'mv %s %s' puts it back", before, before, disk)
		if rmCopies != "" {
			fmt.Fprintf(&b, ", and %s deletes the checkpoint's %s that restore made (the checkpoint keeps it)", rmCopies, plural(len(copies), "copy", "copies"))
		}
		b.WriteString("; then start again, and restore again if you still want the checkpoint")
	} else {
		// The newest copy, by the time in its name: the last restore's.
		newest := slices.Max(copies)
		fmt.Fprintf(&b, "the volume had no disk before the restore, and %s is the checkpoint's copy it made: 'mv %s %s' finishes the restore", newest, newest, disk)
		if rest := slices.DeleteFunc(slices.Clone(copies), func(c string) bool { return c == newest }); len(rest) > 0 {
			fmt.Fprintf(&b, " ('rm %s' deletes older copies)", strings.Join(rest, " "))
		}
		fmt.Fprintf(&b, ", or %s starts the volume empty", rmCopies)
	}
	return errors.New(b.String())
}
