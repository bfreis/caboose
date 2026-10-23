//go:build unix && !linux

package saferm

import "testing"

func setProc(t *testing.T, p string) {}
