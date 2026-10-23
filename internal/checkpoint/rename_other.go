//go:build unix && !linux && !darwin

package checkpoint

import "os"

// renameNoReplaceOS: this system has no rename that refuses to replace.
func renameNoReplaceOS(fromDir *os.File, from string, toDir *os.File, to string) error {
	return errNoReplace
}
