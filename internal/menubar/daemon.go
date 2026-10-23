package menubar

import (
	"context"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/selfupdate"
)

// Daemon is how the core reaches the daemon.
type Daemon interface {
	// Connect connects to the running daemon, never starting one.
	Connect(ctx context.Context) (Conn, error)
	// Ensure connects, starting the daemon when nothing answers. The core
	// calls it only after Connect found nothing answering, and only when
	// MayStart allows. It must never replace a running daemon: the core
	// asks an older one to make way itself, without force.
	Ensure(ctx context.Context) (Conn, error)
	// Replaced is the daemon directory's replaced.json; ok is false when
	// there is none, or it cannot be read.
	Replaced() (r daemon.Replaced, ok bool)
}

// Conn is one connection; *daemonproto.Client is one.
type Conn interface {
	Daemon() daemonproto.Frame
	Compat() daemonproto.Compat
	Call(ctx context.Context, method string, params, result any) error
	Close() error
}

var _ Conn = (*daemonproto.Client)(nil)

// newer reports whether a caboose of protocol p and version v is newer
// than this app's: a higher protocol, or the same one and a later
// release. A version that is not a release tag (a checkout's build) is
// compared by protocol alone.
func newer(p int, v string, ownProto int, ownVersion string) bool {
	if p != ownProto {
		return p > ownProto
	}
	return selfupdate.Valid(v) && selfupdate.Valid(ownVersion) && selfupdate.Compare(v, ownVersion) > 0
}

// MayStart reports whether the app may start a daemon: never once it has
// seen a newer caboose's daemon in this run, nor when replaced.json names
// a newer caboose than the app's, so that an app never brings back an
// older daemon than one that replaced it.
func MayStart(seenNewer bool, replaced daemon.Replaced, haveReplaced bool, ownProto int, ownVersion string) bool {
	if seenNewer {
		return false
	}
	return !haveReplaced || !newer(replaced.Protocol, replaced.Version, ownProto, ownVersion)
}

// SpawnFacts are what SpawnExe chooses from.
type SpawnFacts struct {
	// Checkout is set for an app built from a checkout.
	Checkout bool
	// Own is the app's executable; OwnVersion its version.
	Own, OwnVersion string
	// Installed is the installed CLI's current binary, "" when there is
	// no managed install; InstalledVersion is its release tag.
	Installed, InstalledVersion string
}

// SpawnExe is the executable the app starts the daemon from: for a
// release app, the installed CLI's current binary when it is not older
// than the app, else the app's own; for a checkout's app, its own.
func SpawnExe(f SpawnFacts) string {
	if f.Checkout || f.Installed == "" {
		return f.Own
	}
	if !selfupdate.Valid(f.InstalledVersion) || !selfupdate.Valid(f.OwnVersion) || selfupdate.Compare(f.InstalledVersion, f.OwnVersion) < 0 {
		return f.Own
	}
	return f.Installed
}
