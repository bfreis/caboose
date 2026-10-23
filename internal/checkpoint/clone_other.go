//go:build !darwin

package checkpoint

import "os"

// cloneTree: only macOS clones a whole tree in one call.
func cloneTree(srcParent *os.File, srcBase string, dstParent *os.File, dstBase string) error {
	return errNoWholeClone
}
