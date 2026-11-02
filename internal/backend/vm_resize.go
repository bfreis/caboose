package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/vm"
)

// VolumeResizer is a Backend whose volumes can change size while it runs:
// a VM's, on caboose-vmm's own disks (vm.LiveDiskMacOS).
type VolumeResizer interface {
	// VolumeState says how the sandbox has the volume called name.
	VolumeState(name string) (VolumeState, error)
	// ResizeVolume makes the running sandbox's volume name size bytes,
	// its filesystem with it, saying each step through say. Whatever the
	// error, the VolumeResize says whether the disk now has the new size,
	// which the caller records. ctx ending (an interrupt) stops the resize
	// where that is safe: before the volume is unmounted, nothing is done;
	// after, the step under way runs to its end, and the volume is mounted
	// again.
	ResizeVolume(ctx context.Context, name string, size int64, say func(format string, args ...any)) (VolumeResize, error)
}

// VolumeState is how a sandbox has a volume.
type VolumeState struct {
	// Running: the sandbox runs. Has: it was created with the volume.
	// Live: the volume is on a disk whose size can change while it runs.
	Running, Has, Live bool
}

// VolumeResize is what a resize did: the disk's size before, and whether
// it now has the size asked for (Applied).
type VolumeResize struct {
	From    int64
	Applied bool
}

var _ VolumeResizer = (*VM)(nil)

// The bounds of a resize's steps: the guest's quick ones; an unmount,
// which writes out what the volume has cached; the filesystem's check and
// shrink, which move what lies past the new end (the agent never stops
// them: past this bound they run on, and nothing more is done to the
// volume); and caboose-vmm's change of the disk, which waits for the
// requests in flight and the guest's word.
var (
	resizeStepWait    = time.Minute
	resizeUnmountWait = 5 * time.Minute
	resizeShrinkWait  = 2 * time.Hour
	resizeDiskWait    = time.Minute
)

// ShrinkMargin is the room a shrink leaves above what the volume's
// filesystem uses: a tenth of it, and at least 1 GiB.
func ShrinkMargin(used int64) int64 { return max(1<<30, used/10) }

// BackupSuffix names the clone of a volume's disk a shrink takes beside
// it, before its filesystem is changed.
const BackupSuffix = ".before-shrink"

// volumeAgent is the guest's agent, for a resize's steps.
type volumeAgent interface {
	Volume(op agentproto.VolumeOp, timeout time.Duration) (agentproto.VolumeInfo, error)
	Close() error
}

// resizeSeams are what a resize does to the host and the VM; the tests
// stand in for them.
type resizeSeams struct {
	disks  func(req vm.DiskRequest, timeout time.Duration) (vm.DiskAnswer, error)
	agent  func() (volumeAgent, error)
	clone  func(src, dst string) error
	remove func(path string) error
}

func (v *VM) seams() resizeSeams {
	if v.resize != nil {
		return *v.resize
	}
	return resizeSeams{
		disks: func(req vm.DiskRequest, timeout time.Duration) (vm.DiskAnswer, error) {
			return vm.AskDisks(v.Dir.Socket(), req, timeout)
		},
		agent:  func() (volumeAgent, error) { return v.Dir.Control(10 * time.Second) },
		clone:  vm.CloneFile,
		remove: os.Remove,
	}
}

// VolumeState is VolumeResizer's.
func (v *VM) VolumeState(name string) (VolumeState, error) {
	st, err := v.Dir.ReadState()
	if errors.Is(err, os.ErrNotExist) {
		return VolumeState{}, nil
	}
	if err != nil {
		return VolumeState{}, err
	}
	vs := VolumeState{Running: v.Dir.Serving(), Live: st.LiveVolume(name)}
	for _, n := range st.Volumes {
		vs.Has = vs.Has || n == name
	}
	return vs, nil
}

// ResizeVolume is VolumeResizer's, for a volume on one of caboose-vmm's
// disks of the running VM. Growing: caboose-vmm makes the disk longer and
// tells the guest, then the agent grows the mounted filesystem. Shrinking
// (the filesystem is never shrunk mounted): the agent unmounts the volume,
// refusing while anything holds it; the disk's file is cloned beside it
// (BackupSuffix); the agent checks and shrinks the filesystem;
// caboose-vmm lowers the disk and cuts its file; the agent mounts the
// volume again. A failed step says what it left and what to do; the clone
// goes once the filesystem's steps are done and the volume is mounted.
// One resize of the VM runs at a time, and a start or stop of it waits for
// the resize to end.
func (v *VM) ResizeVolume(ctx context.Context, name string, size int64, say func(format string, args ...any)) (VolumeResize, error) {
	var res VolumeResize
	unlock, err := vm.TryLock(v.Dir.ResizeLock())
	if errors.Is(err, vm.ErrLocked) {
		return res, errors.New("another resize of this VM's volumes is running; wait for it to end")
	}
	if errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("there is no VM %s", v.Name)
	}
	if err != nil {
		return res, err
	}
	defer unlock()
	unstart, err := v.Dir.HoldStart()
	if err != nil {
		return res, err
	}
	defer unstart()
	st, err := v.Dir.ReadState()
	if err != nil {
		return res, err
	}
	if !v.Dir.Serving() {
		return res, fmt.Errorf("the VM %s is not running", v.Name)
	}
	if !st.LiveVolume(name) {
		return res, fmt.Errorf("volume %s is not on one of caboose-vmm's disks, which alone change size while the VM runs (from macOS %d)", name, vm.LiveDiskMacOS)
	}
	m, err := v.Dir.ReadMachine()
	if err != nil {
		return res, err
	}
	path := ""
	for _, b := range m.BlockDevices {
		if b.Serial == name {
			path = b.Path
		}
	}
	if path == "" {
		return res, fmt.Errorf("the VM's machine.json has no disk for volume %s", name)
	}
	r := &resizer{s: v.seams(), ctx: ctx, name: name, path: path, console: v.Dir.Console(), size: size, say: say}
	a, err := r.s.disks(vm.DiskRequest{Op: vm.DiskSize, Serial: name}, resizeStepWait)
	if err == nil && a.Error != "" {
		err = errors.New(guestText(a.Error))
	}
	if err != nil {
		return res, fmt.Errorf("caboose-vmm cannot say volume %s's size: %w", name, err)
	}
	res.From, r.from = a.Size, a.Size
	ag, err := r.s.agent()
	if err != nil {
		return res, err
	}
	defer ag.Close()
	r.ag = ag
	if ctx.Err() != nil {
		return res, errors.New("interrupted; nothing changed")
	}
	switch {
	case size > r.from:
		res.Applied, err = r.grow()
	case size < r.from:
		res.Applied, err = r.shrink()
	default:
		// Already that size: the filesystem may still lag the disk, after
		// a grow whose second half failed.
		res.Applied = true
		if _, err = r.step(agentproto.VolumeGrow, size, resizeStepWait, "the filesystem's grow", "nothing worse than a filesystem that may not fill its disk yet: it grows at the VM's next start"); err != nil {
			err = fmt.Errorf("volume %s's disk is %s already, and its filesystem did not grow to fill it: %w", name, sizeText(size), err)
		}
	}
	return res, err
}

// resizer is one resize's state.
type resizer struct {
	s   resizeSeams
	ctx context.Context
	ag  volumeAgent
	// path is the volume's disk, console the guest's console log.
	name, path, console string
	from, size          int64
	say                 func(format string, args ...any)
	// told: an interruption was said.
	told bool
}

// wait runs f, the step what, to its end. An interruption meanwhile is
// said, once, with what leaving at once would leave (abandon): the next
// interrupt ends the launcher there (the launcher stops catching them).
func (r *resizer) wait(what, abandon string, f func()) {
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
		return
	case <-r.ctx.Done():
	}
	if !r.told {
		r.told = true
		r.say("interrupted: %s runs to its end first, then the resize stops where it is safe to; interrupting again leaves at once, which leaves %s", what, abandon)
	}
	<-done
}

// interrupted is whether the resize was interrupted.
func (r *resizer) interrupted() bool { return r.ctx.Err() != nil }

// step asks the agent for op, and turns an answer's error into one; what
// and abandon are wait's.
func (r *resizer) step(op string, size int64, timeout time.Duration, what, abandon string) (agentproto.VolumeInfo, error) {
	var info agentproto.VolumeInfo
	var err error
	r.wait(what, abandon, func() {
		info, err = r.ag.Volume(agentproto.VolumeOp{Op: op, Name: r.name, Size: size}, timeout)
	})
	if errors.Is(err, vm.ErrNoVolumeAnswer) {
		return info, noAnswer(timeout)
	}
	if err != nil {
		return info, err
	}
	if info.Error != "" {
		return info, errors.New(guestText(info.Error))
	}
	return info, nil
}

// noAnswer is a step the agent did not answer in time.
type noAnswer time.Duration

func (n noAnswer) Error() string {
	return fmt.Sprintf("the guest's agent did not answer in %v (an agent from another caboose answers none: 'caboose restart' gives the VM this one's)", time.Duration(n))
}

// resizeDisk asks caboose-vmm for the disk at size. When the answer is
// lost, the disk's size says whether it was applied.
func (r *resizer) resizeDisk(size int64, abandon string) (vm.DiskAnswer, error) {
	var a vm.DiskAnswer
	var err error
	r.wait("caboose-vmm's change of the disk's size", abandon, func() {
		a, err = r.s.disks(vm.DiskRequest{Op: vm.DiskResize, Serial: r.name, Size: size}, resizeDiskWait)
	})
	if err == nil {
		return a, nil
	}
	q, qerr := r.s.disks(vm.DiskRequest{Op: vm.DiskSize, Serial: r.name}, resizeStepWait)
	if qerr != nil || q.Error != "" {
		return a, fmt.Errorf("%w; and caboose-vmm cannot say the disk's size now", err)
	}
	return vm.DiskAnswer{Size: q.Size, Applied: q.Size == size, Error: err.Error()}, nil
}

// diskNotes says what an applied resize's answer adds: a guest that did
// not confirm, a file left at another size.
func (r *resizer) diskNotes(a vm.DiskAnswer) {
	if !a.Told {
		r.say("the guest did not confirm the disk's new size in time; it reads it when the device's notice lands")
	}
	if a.FileSize != 0 {
		r.say("the disk's file stays at %s, not %s (%s): the rest goes unused, and a start serves the file's size", sizeText(a.FileSize), sizeText(a.Size), guestText(a.FileError))
	}
}

func (r *resizer) grow() (bool, error) {
	r.say("growing volume %s's disk from %s to %s", r.name, sizeText(r.from), sizeText(r.size))
	a, err := r.resizeDisk(r.size, "a disk that may be grown already, its filesystem not: run this again, or it grows at the VM's next start")
	if err != nil {
		return false, fmt.Errorf("caboose-vmm: %w", err)
	}
	if !a.Applied {
		msg := fmt.Sprintf("volume %s's disk keeps its size, %s: %s", r.name, sizeText(r.from), guestText(a.Error))
		if a.FileSize != 0 {
			msg += fmt.Sprintf("; its file stays at %s, the end unused", sizeText(a.FileSize))
		}
		return false, errors.New(msg)
	}
	r.diskNotes(a)
	r.say("growing its filesystem, mounted")
	info, err := r.step(agentproto.VolumeGrow, r.size, resizeStepWait, "the filesystem's grow", "nothing worse than a filesystem that may not fill its disk yet: it grows at the VM's next start")
	if err != nil {
		return true, fmt.Errorf("volume %s's disk is %s now, but its filesystem did not grow to fill it (%v): run this again, or it grows at the VM's next start", r.name, sizeText(r.size), err)
	}
	r.say("its filesystem is %s, %s of it used", sizeText(info.FSBytes), sizeText(info.UsedBytes))
	return true, nil
}

func (r *resizer) shrink() (bool, error) {
	u, err := r.step(agentproto.VolumeUsage, 0, resizeStepWait, "the guest's report of the volume", "nothing changed")
	if err != nil {
		return false, err
	}
	if !u.Mounted {
		return false, fmt.Errorf("volume %s is not mounted in the guest, so how much it holds cannot be told: 'caboose restart' mounts it again", r.name)
	}
	if u.Missing != "" {
		return false, fmt.Errorf("the sandbox's image has no %s, which a shrink runs in the guest to check and shrink the volume's filesystem: add e2fsprogs to the image (e2fsprogs and e2fsprogs-extra on Wolfi and Alpine), then run this again; nothing changed", guestText(u.Missing))
	}
	margin := ShrinkMargin(u.UsedBytes)
	if need := u.UsedBytes + margin; r.size < need {
		return false, fmt.Errorf("volume %s holds %s, and a shrink leaves %s above that (a tenth, at least 1G): it can go down to %s, not %s",
			r.name, sizeText(u.UsedBytes), sizeText(margin), sizeText(roundUpMiB(need)), sizeText(r.size))
	}
	backup := r.path + BackupSuffix
	if _, err := os.Lstat(backup); err == nil {
		return false, fmt.Errorf("an earlier shrink's backup of volume %s is at %s: once the volume's files are as they should be, delete it and run this again (to restore it instead, stop the VM and move it over %s)", r.name, backup, r.path)
	}
	if r.interrupted() {
		return false, errors.New("interrupted; nothing changed")
	}
	r.say("shrinking volume %s from %s to %s: it holds %s", r.name, sizeText(r.from), sizeText(r.size), sizeText(u.UsedBytes))
	r.say("unmounting %s in the guest", guestText(u.Target))
	info, err := r.step(agentproto.VolumeUnmount, 0, resizeUnmountWait, "the guest's unmount",
		"a volume that may end up unmounted in the guest, which 'caboose restart' mounts again; nothing else changed")
	if err != nil {
		return false, r.unmountFailed(info, err)
	}
	if info.Note != "" {
		r.say("%s", guestText(info.Note))
	}
	if r.interrupted() {
		return false, fmt.Errorf("interrupted before anything was shrunk; %s", said(r.remount(r.from, "")))
	}
	// Once the volume is unmounted, the guest has written out all of it:
	// the clone is the filesystem as it was, whole.
	r.say("backing its disk up: %s", backup)
	var cerr error
	r.wait("the backup's clone", fmt.Sprintf("the volume unmounted in the guest, which 'caboose restart' mounts again, and maybe a backup at %s, to delete", backup), func() { cerr = r.s.clone(r.path, backup) })
	if cerr != nil {
		return false, fmt.Errorf("backing volume %s's disk up to %s failed (%v), so it was not shrunk; %s", r.name, backup, cerr, said(r.remount(r.from, "")))
	}
	if r.interrupted() {
		return false, fmt.Errorf("interrupted before anything was shrunk; %s", r.remountDropping(r.from, backup))
	}
	stillRuns := fmt.Sprintf("e2fsck and resize2fs running on in the guest, the volume's filesystem maybe half shrunk and unmounted: leave the VM running until the guest's console log, %s, says \"volume %s: shrinkfs: done\" (or failed), then 'caboose restart', which mounts the volume at its disk's size, %s; if its files are wrong after, %s",
		r.console, r.name, sizeText(r.from), restoreText(backup, r.path))
	r.say("checking and shrinking its filesystem (e2fsck, resize2fs): what lies past %s moves, which takes a while on a full volume", sizeText(r.size))
	info, err = r.step(agentproto.VolumeShrinkFS, r.size, resizeShrinkWait, "the filesystem's check and shrink", stillRuns)
	switch {
	case errors.As(err, new(noAnswer)):
		return false, fmt.Errorf("the guest's e2fsck and resize2fs did not end in %v, and may still run: so the volume was not mounted again, and its backup stays at %s. That leaves %s",
			resizeShrinkWait, backup, stillRuns)
	case err != nil && info.Dirty:
		return false, fmt.Errorf("shrinking volume %s's filesystem failed (%v), and may have left it half shrunk, so it was not mounted again: %s; 'caboose restart' mounts it as it is, which is safe only if the failure changed nothing",
			r.name, err, restoreText(backup, r.path))
	case err != nil:
		return false, fmt.Errorf("shrinking volume %s's filesystem failed (%v); its disk keeps its size, %s; %s", r.name, err, sizeText(r.from), r.remount(r.from, backup))
	}
	if r.interrupted() {
		// The filesystem grows back to fill the disk as it mounts.
		return false, fmt.Errorf("interrupted once its filesystem was shrunk, before its disk was lowered: the disk keeps %s; %s", sizeText(r.from), r.remountDropping(r.from, backup))
	}
	r.say("lowering its disk to %s", sizeText(r.size))
	a, err := r.resizeDisk(r.size, fmt.Sprintf("caboose-vmm lowering the disk or not, the volume unmounted in the guest: 'caboose restart' mounts it at its disk's size (config.toml still has %s, and a start grows a disk to it); the backup stays at %s, to delete once its files are right (if they are wrong, %s)",
		sizeText(r.from), backup, restoreText(backup, r.path)))
	if err != nil || !a.Applied {
		why := err
		if why == nil {
			why = errors.New(guestText(a.Error))
		}
		// The filesystem grows back to fill the disk as it mounts.
		return false, fmt.Errorf("caboose-vmm did not lower volume %s's disk (%v): it keeps %s, its filesystem grown back to fill it; %s", r.name, why, sizeText(r.from), r.remountDropping(r.from, backup))
	}
	r.diskNotes(a)
	if msg := r.remount(r.size, ""); msg != mountedAgain {
		return true, fmt.Errorf("volume %s's disk and filesystem are %s now, but %s; the backup stays at %s", r.name, sizeText(r.size), msg, backup)
	}
	r.drop(backup)
	r.say("volume %s is %s now", r.name, sizeText(r.size))
	return true, nil
}

// unmountFailed says what a failed unmount left. The agent refused, or
// failed, and the volume is as its answer says; or it did not answer, and
// is asked again.
func (r *resizer) unmountFailed(info agentproto.VolumeInfo, err error) error {
	if errors.As(err, new(noAnswer)) {
		u, uerr := r.step(agentproto.VolumeUsage, 0, resizeStepWait, "the guest's report of the volume", "a volume that may be unmounted in the guest, which 'caboose restart' mounts again")
		switch {
		case uerr != nil:
			return fmt.Errorf("%v, and may still be unmounting the volume (%v): once it has, 'caboose restart' mounts it again; nothing else changed", err, uerr)
		case u.Mounted:
			return fmt.Errorf("%v; the volume is still mounted, and nothing changed", err)
		}
		return fmt.Errorf("%v, and the volume was unmounted; nothing else changed; %s", err, said(r.remount(r.from, "")))
	}
	switch {
	case len(info.Holders) > 0:
		return fmt.Errorf("%v; nothing changed. Stop what holds it, and run this again:\n%s", err, holders(info))
	case info.Mounted:
		return fmt.Errorf("%v; nothing changed", err)
	}
	return fmt.Errorf("%v; nothing was shrunk, and the volume is not mounted in the guest: 'caboose restart' mounts it again", err)
}

// remountDropping mounts the volume again, and once it is, with the
// filesystem's steps done or not begun, deletes the backup; else the
// backup stays.
func (r *resizer) remountDropping(size int64, backup string) string {
	msg := r.remount(size, "")
	if msg != mountedAgain {
		return msg + "; the backup stays at " + backup
	}
	r.drop(backup)
	return said(msg)
}

// drop deletes the backup, once the filesystem's steps are done and the
// volume is mounted again.
func (r *resizer) drop(backup string) {
	if err := r.s.remove(backup); err != nil {
		r.say("could not delete the backup %s (%v): delete it by hand", backup, err)
	}
}

// said is remount's word as the end of a sentence.
func said(msg string) string {
	if msg == mountedAgain {
		return "it is mounted again"
	}
	return msg
}

// mountedAgain is remount's word for a volume mounted again, nothing
// more to say.
const mountedAgain = "mounted again"

// remount mounts the volume again, its filesystem grown to fill the disk
// once the guest sees it at size, and says how that went. backup, when
// not "", is the clone kept because a step of the filesystem's failed.
func (r *resizer) remount(size int64, backup string) string {
	abandon := "a volume that may end up mounted in the guest or not ('caboose restart' mounts it)"
	if backup != "" {
		abandon += fmt.Sprintf(", and the backup at %s: delete it once its files are right (if they are wrong, %s)", backup, restoreText(backup, r.path))
	}
	info, err := r.step(agentproto.VolumeMount, size, resizeStepWait, "mounting the volume again", abandon)
	if err != nil {
		msg := fmt.Sprintf("mounting it again failed (%v): 'caboose restart' mounts it", err)
		if backup != "" {
			msg += "; if its files are wrong, " + restoreText(backup, r.path)
		}
		return msg
	}
	if info.Note != "" {
		r.say("%s", guestText(info.Note))
	}
	if backup != "" {
		return "it is mounted again; check its files, then delete the backup " + backup + " (if they are wrong, " + restoreText(backup, r.path) + ")"
	}
	r.say("mounted again")
	return mountedAgain
}

// restoreText says how to put a shrink's backup back.
func restoreText(backup, path string) string {
	return fmt.Sprintf("stop the VM ('caboose stop') and move the backup %s over %s, which gives the volume back as it was before the shrink", backup, path)
}

// holders lists an unmount's holders, the guest's words cut short.
func holders(info agentproto.VolumeInfo) string {
	var lines []string
	for i, h := range info.Holders {
		if i == agentproto.MaxVolumeHolders {
			break
		}
		h = cutText(guestText(h), 200)
		lines = append(lines, "  "+h)
	}
	if n := info.HoldersMore; n > 0 {
		lines = append(lines, fmt.Sprintf("  and %d more", n))
	}
	return strings.Join(lines, "\n")
}

// guestText is s, which the guest or caboose-vmm said, made printable and
// cut short.
func guestText(s string) string {
	return cutText(proposal.Printable(s), agentproto.MaxVolumeText)
}

// cutText is s cut to at most n bytes, where a character starts, and
// "..." after it when cut.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// roundUpMiB is n rounded up to a MiB, the unit config.toml's sizes come
// in.
func roundUpMiB(n int64) int64 { return (n + 1<<20 - 1) >> 20 << 20 }

// sizeText is n bytes as config.toml writes a size: "64G", "1536M", or
// "10.5G" where neither is whole.
func sizeText(n int64) string {
	switch {
	case n%(1<<30) == 0:
		return fmt.Sprintf("%dG", n>>30)
	case n%(1<<20) == 0 && n < 10<<30:
		return fmt.Sprintf("%dM", n>>20)
	}
	return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
}
