package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// A volume's checkpoint is a copy-on-write clone of its disk's image file,
// made while the VM holds the volume's filesystem still (frozen, when the
// VM runs) so that the clone is a consistent filesystem: one call
// (fclonefileat on APFS), which costs the host only the blocks either
// copy changes later. It is never copied byte by byte: that would hold
// the filesystem still far longer than a clone, and write the whole disk
// again.

// Held is what a HoldFunc did.
type Held struct {
	// Frozen: the VM ran, and held the filesystem still for FrozenFor.
	Frozen    bool
	FrozenFor time.Duration
}

// Holder holds a volume's filesystem still: Hold runs during meanwhile,
// and says how (frozen while the VM runs, else the VM kept from
// starting). An error is a hold that failed or did not last through
// during, whose result is then not used. during runs at most once.
type Holder interface {
	Hold(ctx context.Context, during func() error) (Held, error)
}

// cloneFile makes name in dir a copy-on-write clone of the open file src,
// in one call; a test seam, since off APFS few filesystems clone.
var cloneFile = cloneInto

func cloneInto(src, dir *os.File, name string) error {
	f, err := reflinkInto(src, dir, name)
	if f != nil {
		f.Close()
	}
	return err
}

// ErrIsVolume is a checkpoint of a volume's disk asked to be copied into a
// directory: a volume's is restored in place.
var ErrIsVolume = errors.New("checkpoint: it is of a volume's disk, which is restored in place, with the VM stopped")

// ErrNoClone is a volume's disk that cannot be cloned into the store.
var ErrNoClone = errors.New("checkpoint: the volume's disk cannot be cloned into the store")

// noCloneHelp says what a volume's clone needs, and which setting moves
// which side: the store is in CABOOSE_HOME (its envs/<env>/checkpoints),
// the disk in the data dir, which CABOOSE_DATA_DIR names when set.
func noCloneHelp(disks, store string) string {
	return fmt.Sprintf("a volume is checkpointed only by a copy-on-write clone, which needs the store (%s, under CABOOSE_HOME) and the volume's disk (in %s, the data dir's vm/volumes, which CABOOSE_DATA_DIR moves) on one APFS volume: set CABOOSE_HOME or CABOOSE_DATA_DIR so that both are on one, or take the volume's path out of [checkpoints] paths", store, disks)
}

// cloneMethod is how a clone is made here.
func cloneMethod() string {
	if runtime.GOOS == "darwin" {
		return MethodClonefile
	}
	return MethodReflink
}

// cloneVolume makes tmp/TreeName a clone of the volume source src's disk,
// opened without following anything, while src.Hold holds its filesystem
// still.
func cloneVolume(ctx context.Context, src Source, tmp string) (Result, *VolumeMeta, error) {
	if src.Hold == nil || src.Volume == "" || !src.File {
		return Result{}, nil, fmt.Errorf("checkpoint: volume source %q has no volume to hold", src.Path)
	}
	h, err := openSource(src)
	if err != nil {
		return Result{}, nil, err
	}
	defer h.close()
	dir, err := openDirPath(tmp)
	if err != nil {
		return Result{}, nil, fmt.Errorf("checkpoint: %w", err)
	}
	defer dir.Close()
	if err := ctx.Err(); err != nil {
		return Result{}, nil, err
	}
	held, err := src.Hold.Hold(ctx, func() error {
		if err := cloneFile(h.f, dir, TreeName); err != nil {
			if diskFull(err) {
				return fmt.Errorf("%w (%v)", ErrDiskFull, err)
			}
			return fmt.Errorf("%w (%v): %s", ErrNoClone, err, noCloneHelp(src.Top, filepath.Dir(tmp)))
		}
		return nil
	})
	if err != nil {
		return Result{}, nil, fmt.Errorf("volume %s: %w", src.Volume, err)
	}
	if err := checkCloned(dir, TreeName, true); err != nil {
		return Result{}, nil, fmt.Errorf("checkpoint: %s: %w", filepath.Join(tmp, TreeName), err)
	}
	var st unix.Stat_t
	if err := fstatat(dir, TreeName, &st); err != nil {
		return Result{}, nil, fmt.Errorf("checkpoint: %w", err)
	}
	vm := &VolumeMeta{Name: src.Volume, Bytes: st.Size, Frozen: held.Frozen}
	if held.Frozen {
		vm.FrozenMS = max(1, held.FrozenFor.Milliseconds())
	}
	return Result{Method: cloneMethod(), Entries: 1, Bytes: st.Size}, vm, nil
}
