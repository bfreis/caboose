package agent

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// A volume's checkpoint, the guest's half: the host asks for the volume's
// filesystem to be frozen (FIFREEZE: everything written is flushed to the
// disk, and every new write waits), clones the disk's file, and asks for
// it to be thawed (FITHAW). The filesystem is synced first (syncfs), with
// writes still going: FIFREEZE blocks new writes before it flushes, so
// whatever is still dirty then is written out with every writer waiting,
// and a sync first leaves it only what was written since. A filesystem is never left frozen: the agent
// thaws it itself agentproto.MaxFrozen after the freeze, when the
// connection that froze it ends, and before the guest powers off. A thaw
// that finds its freeze already ended says so (Expired), and the host
// drops the clone, which may then not be consistent.

// freezer is the guest's frozen filesystems, by target.
type freezer struct {
	// max is how long a filesystem stays frozen before the agent thaws
	// it itself.
	max time.Duration
	// sync is syncfs, and freeze and thaw are FIFREEZE and FITHAW, on
	// the filesystem mounted at target.
	sync, freeze, thaw func(target string) error
	logf               func(format string, args ...any)

	mu     sync.Mutex
	frozen map[string]*frozenFS
	// ended says how the freezes that ended before their thaw ended, by
	// token: the last maxEnded of them.
	ended map[string]string
	order []string
}

// frozenFS is one freeze: its token, and the timer that thaws it.
type frozenFS struct {
	token string
	// pending: the freeze has not returned yet; cancel: a thaw came
	// meanwhile, so it is undone once it does.
	pending, cancel bool
	timer           *time.Timer
}

// maxEnded bounds what a freezer remembers of freezes that ended early.
const maxEnded = 16

// guestFreezer is the guest's own.
var guestFreezer = newFreezer(agentproto.MaxFrozen, fssync, fifreeze, fithaw)

func newFreezer(max time.Duration, sync, freeze, thaw func(string) error) *freezer {
	return &freezer{max: max, sync: sync, freeze: freeze, thaw: thaw, frozen: map[string]*frozenFS{}, ended: map[string]string{}}
}

func (z *freezer) log(format string, args ...any) {
	if z.logf != nil {
		z.logf(format, args...)
	}
}

// Freeze freezes the filesystem mounted at target for token. One freeze
// of a target at a time: another, while one stands, is refused.
func (z *freezer) Freeze(target, token string) error {
	if token == "" || len(token) > agentproto.MaxVolumeToken {
		return fmt.Errorf("a freeze's token of %d bytes", len(token))
	}
	z.mu.Lock()
	if z.frozen[target] != nil {
		z.mu.Unlock()
		return fmt.Errorf("%s is being frozen or is frozen already, for another checkpoint", target)
	}
	f := &frozenFS{token: token, pending: true}
	z.frozen[target] = f
	z.mu.Unlock()

	// Unlocked: a freeze writes out everything the filesystem has cached,
	// which takes a while, and another target's thaw must not wait. The
	// sync first writes most of it out while writers still go on; one
	// that fails leaves the freeze to flush it all.
	if err := z.sync(target); err != nil {
		z.log("syncfs %s before its freeze: %v", target, err)
	}
	err := z.freeze(target)

	z.mu.Lock()
	defer z.mu.Unlock()
	f.pending = false
	if err != nil {
		delete(z.frozen, target)
		if errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("FIFREEZE %s: it is frozen already, by something in the guest", target)
		}
		return fmt.Errorf("FIFREEZE %s: %w", target, err)
	}
	if f.cancel {
		// The host gave up on it while the freeze ran.
		delete(z.frozen, target)
		if err := z.thaw(target); err != nil {
			z.log("thawing %s, whose checkpoint gave up on it: %v", target, err)
		}
		z.remember(token, "the host gave up on the freeze before it ended")
		return fmt.Errorf("%s: the host gave up on the freeze before it ended; thawed", target)
	}
	f.timer = time.AfterFunc(z.max, func() { z.expire(target, token) })
	return nil
}

// expire is a freeze's timer: the host's thaw did not come in time.
func (z *freezer) expire(target, token string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	f := z.frozen[target]
	if f == nil || f.token != token || f.pending {
		return
	}
	delete(z.frozen, target)
	err := z.thaw(target)
	why := fmt.Sprintf("the agent thawed it itself after %v, with no thaw from the host", z.max)
	if err != nil {
		why += fmt.Sprintf(" (FITHAW: %v)", err)
	}
	z.log("volume at %s: %s", target, why)
	z.remember(token, why)
}

// Thaw thaws the filesystem at target that token froze. expired says the
// freeze had ended before (why says how), which leaves it thawed all the
// same; err is a thaw that failed.
func (z *freezer) Thaw(target, token string) (expired bool, why string, err error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	f := z.frozen[target]
	if f == nil || f.token != token {
		if why, ok := z.ended[token]; ok {
			return true, why, nil
		}
		return true, "it was not frozen for this checkpoint", nil
	}
	if f.pending {
		f.cancel = true
		return true, "its freeze had not ended yet; it is thawed once it does", nil
	}
	f.timer.Stop()
	delete(z.frozen, target)
	if err := z.thaw(target); err != nil {
		if errors.Is(err, unix.EINVAL) {
			why := "it was no longer frozen: something in the guest thawed it"
			z.remember(token, why)
			return true, why, nil
		}
		return false, "", fmt.Errorf("FITHAW %s: %w", target, err)
	}
	return false, "", nil
}

// ThawAll thaws every filesystem frozen now: before the guest powers off,
// since a frozen one would hold up every process writing to it.
func (z *freezer) ThawAll() {
	z.mu.Lock()
	defer z.mu.Unlock()
	for target, f := range z.frozen {
		if f.pending {
			f.cancel = true
			continue
		}
		f.timer.Stop()
		delete(z.frozen, target)
		if err := z.thaw(target); err != nil {
			z.log("thawing %s: %v", target, err)
		}
		z.remember(f.token, "the guest is shutting down")
	}
}

// remember records how token's freeze ended, keeping the last maxEnded.
func (z *freezer) remember(token, why string) {
	if _, ok := z.ended[token]; !ok {
		z.order = append(z.order, token)
	}
	z.ended[token] = why
	for len(z.order) > maxEnded {
		delete(z.ended, z.order[0])
		z.order = z.order[1:]
	}
}

// The ioctls: FIFREEZE is _IOWR('X', 119, int), FITHAW _IOWR('X', 120,
// int), on any descriptor of the filesystem.
const (
	fiFreeze = 0xc0045877
	fiThaw   = 0xc0045878
)

func fifreeze(target string) error { return fsIoctl(target, fiFreeze) }

// fssync is syncfs(2) on the filesystem mounted at target.
func fssync(target string) error {
	f, err := os.OpenFile(target, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}
func fithaw(target string) error { return fsIoctl(target, fiThaw) }

func fsIoctl(target string, req uintptr) error {
	f, err := os.OpenFile(target, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, 0); e != 0 {
		return e
	}
	return nil
}

// sameFS says whether the filesystem mounted at target is the one on the
// block device dev: a freeze must hold the volume's, not whatever the
// guest mounted over its target since.
func sameFS(dev, target string) error {
	var d, t unix.Stat_t
	if err := unix.Stat(dev, &d); err != nil {
		return err
	}
	if err := unix.Stat(target, &t); err != nil {
		return err
	}
	if !onDevice(&d, &t) {
		return fmt.Errorf("what is mounted at %s is not the volume's filesystem (on %s)", target, dev)
	}
	return nil
}

// onDevice says whether t, a file's stat, is on the block device whose
// node's stat is d.
func onDevice(d, t *unix.Stat_t) bool {
	return d.Mode&unix.S_IFMT == unix.S_IFBLK && t.Dev == d.Rdev
}
