package daemon

import "github.com/bfreis/caboose/internal/daemonproto"

// Dialogs is the URL dialog queue: the requests whose dialog is to be
// shown, one at a time across environments, by this machine's own
// dialog, or by the attached menu-bar app while one holding
// daemonproto.CapDialogs is attached. The daemon tells it of the app's
// attachments, each by its number (never 0), and hands it the app's
// polls; what to show, and every answer, stay the host's.
type Dialogs interface {
	// SetApp says whether attachment n holds dialogs now. on hands the
	// queue to n (an attachment it held before goes away first); off,
	// when n holds it, ends that: the request in flight with n is denied
	// ("the app went away; ask again"), and those still queued go back to
	// this machine's dialog. An off for an attachment that no longer
	// holds the queue changes nothing, so a late one is harmless. An
	// attachment that holds the queue and stops calling Next is let go
	// of as at an off, after a while.
	SetApp(n uint64, on bool)
	// Next is the dialogs method for attachment n: the request in flight
	// with it, else the next queued one, which is then in flight; nil
	// when none waits. An attachment that does not hold the queue is
	// refused.
	Next(n uint64) (*daemonproto.DialogRef, error)
}
