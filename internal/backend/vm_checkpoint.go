package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/vm"
)

// VolumeHolder is a Backend whose volumes' disks can be cloned while it
// runs, their filesystems held still meanwhile, and replaced while it is
// kept stopped: a VM's.
type VolumeHolder interface {
	// HoldVolume runs during, which clones the disk of the volume name,
	// while nothing changes that disk: the running VM has the agent freeze
	// the volume's filesystem (everything written is flushed to the disk,
	// and every write waits) and thaw it after; a VM that does not run,
	// or runs without the volume, is kept from starting instead. An error
	// is one of during, or a hold that did not last through it: what
	// during made is then not to be used.
	HoldVolume(ctx context.Context, name string, during func() error) (VolumeHeld, error)
	// HoldStopped runs during with the VM stopped and kept from
	// starting, and refuses while it runs.
	HoldStopped(during func() error) error
}

// VolumeHeld is how a volume was held still.
type VolumeHeld struct {
	// Frozen: the VM ran, and its agent held the filesystem frozen for
	// FrozenFor, as the host measured it (freeze answered to thaw
	// answered).
	Frozen    bool
	FrozenFor time.Duration
}

var _ VolumeHolder = (*VM)(nil)

// The bounds of a hold: the waits for the volumes' lock and the VM's
// start lock, which a start, a stop or a restore holds for seconds; the
// freeze, which writes out what the volume's filesystem has cached first;
// during, a clone of the disk's file, given half the time the agent waits
// before thawing the filesystem itself (agentproto.MaxFrozen), so that
// the host's own thaw comes first; and the thaw.
var (
	holdLockWait = 2 * time.Minute
	freezeWait   = time.Minute
	holdWait     = agentproto.MaxFrozen / 2
	thawWait     = 10 * time.Second
)

// HoldVolume is VolumeHolder's. It takes the volumes' lock (VolumesLock),
// so that no start opens a disk meanwhile, even of a VM not made yet; then
// the VM's start lock, so that the VM neither starts nor stops meanwhile,
// refusing while a resize holds it (resizes and checkpoints exclude each
// other through it, which both hold throughout); and its checkpoint lock,
// which says to a start or stop waiting why. Each wait ends with ctx, or
// after holdLockWait. The checkpoint store's lock stays held meanwhile:
// TakeLocked's caller holds it across the whole take, Publish included,
// so a hold cannot let go of it; the waits are bounded instead. A freeze
// that fails, or is not answered in time, leaves the volume to the agent,
// which thaws what a connection froze when it ends (this one is closed on
// the way out) and, failing that, after agentproto.MaxFrozen. After a
// freeze the thaw always runs, whatever during did; during running past
// holdWait is not waited for before the thaw, but is before returning, so
// whatever it writes is done once this returns.
func (v *VM) HoldVolume(ctx context.Context, name string, during func() error) (held VolumeHeld, err error) {
	unvol, err := v.holdVolumes(ctx, holdLockWait)
	if err != nil {
		return held, err
	}
	defer unvol()
	unstart, err := vm.TryLock(v.Dir.StartLock())
	if errors.Is(err, vm.ErrLocked) {
		if v.Dir.Resizing() {
			return held, errors.New("a resize of this VM's volumes is running: no checkpoint of a volume is taken meanwhile")
		}
		unstart, err = vm.WaitLock(ctx, v.Dir.StartLock(), holdLockWait, func() { v.note("waiting for the VM's start or stop to end") })
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No VM, so no disk in use; the volumes' lock keeps a first start
		// from opening it.
		return held, during()
	case err != nil:
		return held, fmt.Errorf("waiting for the VM's start or stop: %w", err)
	}
	defer unstart()
	// Only a start or stop's probe (Checkpointing) takes it besides.
	uncp, err := vm.WaitLock(ctx, v.Dir.CheckpointLock(), holdLockWait, nil)
	if err != nil {
		return held, err
	}
	defer uncp()
	st, err := v.Dir.ReadState()
	if errors.Is(err, os.ErrNotExist) {
		return held, during()
	}
	if err != nil {
		return held, err
	}
	target := ""
	for i, n := range st.Volumes {
		if n == name && i < len(st.Boot.Disks) {
			target = st.Boot.Disks[i].Target
		}
	}
	if !v.Dir.Serving() || target == "" {
		// Stopped, and kept so; or running without the volume, whose disk
		// it then has not attached.
		return held, during()
	}
	if err := ctx.Err(); err != nil {
		return held, err
	}
	ag, err := v.seams().agent()
	if err != nil {
		return held, err
	}
	// Closing the connection has the agent thaw whatever it froze, and
	// has not thawed since: after a freeze that failed, or was answered
	// too late.
	defer ag.Close()
	op := agentproto.VolumeOp{Op: agentproto.VolumeFreeze, Name: name, Target: target, Token: newToken()}
	info, err := ag.Volume(op, freezeWait)
	switch {
	case errors.Is(err, vm.ErrNoVolumeAnswer):
		return held, fmt.Errorf("freezing volume %s's filesystem: %w", name, noAnswer(freezeWait))
	case err != nil:
		return held, fmt.Errorf("freezing volume %s's filesystem: %w", name, err)
	case info.Error != "":
		return held, fmt.Errorf("freezing volume %s's filesystem: %s", name, guestText(info.Error))
	}
	frozenAt := time.Now()
	var thawErr error
	thawed := false
	thaw := func() {
		if thawed {
			return
		}
		thawed = true
		op.Op = agentproto.VolumeThaw
		info, err := ag.Volume(op, thawWait)
		held = VolumeHeld{Frozen: true, FrozenFor: time.Since(frozenAt)}
		switch {
		case errors.Is(err, vm.ErrNoVolumeAnswer):
			thawErr = fmt.Errorf("thawing volume %s's filesystem: %w; the agent thaws it once this connection ends, or at most %v after the freeze", name, noAnswer(thawWait), agentproto.MaxFrozen)
		case err != nil:
			thawErr = fmt.Errorf("thawing volume %s's filesystem: %w; the agent thaws it once this connection ends, or at most %v after the freeze", name, err, agentproto.MaxFrozen)
		case info.Error != "":
			thawErr = fmt.Errorf("thawing volume %s's filesystem: %s", name, guestText(info.Error))
		case info.Expired:
			thawErr = fmt.Errorf("volume %s's filesystem was thawed before its clone was done (%s), so the clone may not be consistent", name, guestText(info.Note))
		}
	}
	defer func() {
		// The thaw, whatever happened since the freeze, unless it ran
		// already (during ran past its bound).
		thaw()
		if thawErr != nil {
			err = errors.Join(err, thawErr)
		}
	}()
	err = runWithin(during, holdWait, thaw)
	return held, err
}

// runWithin runs f, and returns its error. Should f take longer than
// wait, late is called, and f is still waited for, but its result is an
// error: the hold it ran under did not last through it.
func runWithin(f func() error, wait time.Duration, late func()) error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
	}
	late()
	err := <-done
	return errors.Join(fmt.Errorf("the clone took longer than %v, the most a volume is held frozen for it", wait), err)
}

// HoldStopped is VolumeHolder's: under the volumes' lock, so that no
// start opens a disk meanwhile, even of a VM not made yet, and the VM's
// start lock, refusing while it runs.
func (v *VM) HoldStopped(during func() error) error {
	ctx := context.Background()
	unvol, err := v.holdVolumes(ctx, holdLockWait)
	if err != nil {
		return err
	}
	defer unvol()
	unstart, err := vm.WaitLock(ctx, v.Dir.StartLock(), holdLockWait, func() { v.note("waiting for the VM's start or stop to end") })
	if errors.Is(err, os.ErrNotExist) {
		return during()
	}
	if err != nil {
		return fmt.Errorf("waiting for the VM's start or stop: %w", err)
	}
	defer unstart()
	if v.Dir.Serving() {
		return fmt.Errorf("the VM %s is running: a volume's disk is replaced only while it is stopped ('caboose stop' stops it)", v.Name)
	}
	return during()
}

// holdVolumes takes VolumesLock, when one is set, waiting for it at most
// bound (0: for as long as it takes, as a start waits for the start
// lock), and making its directory, the volumes', when missing. It is
// taken before the start lock, never after: a start holds both.
func (v *VM) holdVolumes(ctx context.Context, bound time.Duration) (func(), error) {
	if v.VolumesLock == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(v.VolumesLock), 0o700); err != nil {
		return nil, err
	}
	unlock, err := vm.WaitLock(ctx, v.VolumesLock, bound, func() { v.note("waiting for a start of the VM, or a checkpoint or restore of a volume, to end") })
	if err != nil {
		return nil, fmt.Errorf("waiting for the volumes' disks: %w", err)
	}
	return unlock, nil
}

// note tells Note, when set.
func (v *VM) note(format string, args ...any) {
	if v.Note != nil {
		v.Note(format, args...)
	}
}

// newToken ties a thaw to its freeze.
func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
