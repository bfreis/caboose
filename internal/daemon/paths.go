// Package daemon is caboose's host daemon: one process per daemon
// directory, serving the CLI (and later other clients) on a Unix socket
// that only its own user may connect to, started on demand by the first
// command that needs it and exiting a while after the last client and the
// last sandbox it serves are gone.
//
// The directory holds:
//
//	daemon.sock   the socket (0600), in a directory only the user may enter (0700)
//	daemon.lock   flock: one daemon per directory
//	start.lock    flock held by a client while it starts one, so that
//	              commands racing to start it end with one daemon
//	daemon.json   what the running daemon says of itself (State)
//	daemon.log    its log, capped
//
// The directory is CABOOSE_HOME/daemon for a release, and
// CABOOSE_HOME/daemon-dev for a build from a checkout, which runs a daemon
// of its own beside the release's. Neither is ever mounted into a sandbox.
package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// The daemon directories' names, under CABOOSE_HOME, and the files in them.
const (
	DirName    = "daemon"
	DevDirName = "daemon-dev"

	SocketFile    = "daemon.sock"
	LockFile      = "daemon.lock"
	StartLockFile = "start.lock"
	StateFile     = "daemon.json"
	LogFile       = "daemon.log"
)

// Dir is the daemon directory under cabooseHome: dev for a build from a
// checkout, which runs a daemon of its own.
func Dir(cabooseHome string, dev bool) string {
	if dev {
		return filepath.Join(cabooseHome, DevDirName)
	}
	return filepath.Join(cabooseHome, DirName)
}

// Dirs are every daemon directory under cabooseHome, release and dev.
func Dirs(cabooseHome string) []string {
	return []string{Dir(cabooseHome, false), Dir(cabooseHome, true)}
}

// Socket is the socket's path in the daemon directory dir.
func Socket(dir string) string { return filepath.Join(dir, SocketFile) }

// CheckSocket refuses a daemon directory whose socket path is too long for
// a Unix socket, naming CABOOSE_HOME, which decides it.
func CheckSocket(dir string) error {
	s := Socket(dir)
	if len(s) > daemonproto.MaxSocketPath {
		return fmt.Errorf("the daemon's socket path, %s, is %d bytes: a Unix socket's may be at most %d; set CABOOSE_HOME to a shorter path (it is %s)",
			s, len(s), daemonproto.MaxSocketPath, filepath.Dir(dir))
	}
	return nil
}
