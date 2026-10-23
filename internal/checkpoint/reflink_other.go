//go:build unix && !linux && !darwin

package checkpoint

import "os"

// reflinkInto: no per-file clone on this platform.
func reflinkInto(src, dst *os.File, name string) (*os.File, error) {
	return nil, errNoReflink
}
