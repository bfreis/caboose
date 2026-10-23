package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// renameNoReplaceOS renames from in fromDir to to in toDir, failing with
// EEXIST when anything is at to: renameat2's RENAME_NOREPLACE, which the
// kernel checks and renames in one step. Neither name is followed.
func renameNoReplaceOS(fromDir *os.File, from string, toDir *os.File, to string) error {
	err := unix.Renameat2(int(fromDir.Fd()), from, int(toDir.Fd()), to, unix.RENAME_NOREPLACE)
	runtime.KeepAlive(fromDir)
	runtime.KeepAlive(toDir)
	return err
}
