//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

// peerOf cannot tell the peer's user on this system, so every connection
// is refused.
func peerOf(net.Conn) (peerCred, error) {
	return peerCred{}, errors.New("cannot read a peer's credentials on this system")
}

// executableOf cannot be read on this system.
func executableOf(int) string { return "" }
