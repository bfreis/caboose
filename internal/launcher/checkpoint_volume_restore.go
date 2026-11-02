package launcher

import (
	"context"
	"fmt"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// restoreVolume is `caboose checkpoint restore ID` for a checkpoint of a
// volume's disk: put back as the disk, with the VM stopped, the disk as
// it was set aside beside it (checkpoint.RestoreVolume).
func (a *App) restoreVolume(s *checkpoint.Store, id string, o checkpointOpts) error {
	pid, err := checkpoint.ParseID(id)
	if err != nil {
		return Die("%v", err)
	}
	e, err := s.Resolve(pid)
	if err != nil {
		return Die("%s", proposal.Printable(err.Error()))
	}
	if e.Err != nil || e.Meta == nil {
		return Die("checkpoint %s cannot be read: %s", e.ID, proposal.Printable(errString(e.Err)))
	}
	if e.Meta.Kind != checkpoint.KindVolume || e.Meta.Volume == nil {
		return Die("checkpoint %s is of a path, not a volume's disk: restore it into a new directory, 'caboose -e %s checkpoint restore %s --to DIR'", e.ID, a.Cfg.Env, e.ID)
	}
	name := e.Meta.Volume.Name
	if !config.ValidVolumeName(name) || name == dockerVolume {
		return Die("checkpoint %s names no volume caboose restores (%q)", e.ID, proposal.Printable(name))
	}
	b, ok := a.box().(backend.VolumeHolder)
	if !ok {
		return Die("checkpoint %s is of volume %s's disk, which only a VM has: this environment's isolation is %s", e.ID, name, isolationOf(a.Cfg))
	}
	dir, err := config.Physical(a.volumesDir())
	if err != nil {
		return Die("the volumes' directory %s: %v", a.volumesDir(), err)
	}
	r, err := checkpoint.RestoreVolume(context.Background(), s, e.ID.String(), checkpoint.VolumeRestoreOptions{
		Dir: dir, Name: name, Stopped: b.HoldStopped, LockWait: o.lockWait,
	})
	if err != nil {
		return Die("%s", proposal.Printable(err.Error()))
	}
	fmt.Fprintf(a.Stdout, "Restored %s (%s) as volume %s's disk, %s: the VM's next start mounts it.\n", r.ID, r.Path, name, gibString(r.Bytes))
	if r.Before != "" {
		fmt.Fprintf(a.Stdout, "The disk as it was is kept at %s: once the volume is right, delete it; to undo, with the VM stopped, move it back over %s.\n", r.Before, r.Disk)
	}
	return nil
}
