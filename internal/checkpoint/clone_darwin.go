package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// cloneTree clones srcBase in srcParent, a directory with everything below
// it or a file, to dstBase in dstParent in one call (clonefileat; APFS,
// one volume). CLONE_NOFOLLOW clones a symlink found at srcBase as a
// link, and Copy checks the clone's type after.
func cloneTree(srcParent *os.File, srcBase string, dstParent *os.File, dstBase string) error {
	err := unix.Clonefileat(int(srcParent.Fd()), srcBase, int(dstParent.Fd()), dstBase, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	runtime.KeepAlive(srcParent)
	runtime.KeepAlive(dstParent)
	return err
}
