package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// renameNoReplaceOS renames from in fromDir to to in toDir, failing with
// EEXIST when anything is at to: renameatx_np's RENAME_EXCL, which the
// kernel checks and renames in one step. Neither name is followed.
func renameNoReplaceOS(fromDir *os.File, from string, toDir *os.File, to string) error {
	err := unix.RenameatxNp(int(fromDir.Fd()), from, int(toDir.Fd()), to, unix.RENAME_EXCL)
	runtime.KeepAlive(fromDir)
	runtime.KeepAlive(toDir)
	return err
}
