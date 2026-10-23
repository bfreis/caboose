//go:build !unix

package conformance

import "io/fs"

// On a platform without Unix file ownership nothing is owned by the user,
// so the marker check, and with it the suite, refuses.

var currentUID = func() int { return -1 }

func ownerOf(fs.FileInfo) (int, bool) { return 0, false }

func sysIdentity(fs.FileInfo) (dev, ino, nlink uint64, ok bool) { return 0, 0, 0, false }
