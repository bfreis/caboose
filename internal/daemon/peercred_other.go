//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

// peerUID cannot tell the peer's user on this system, so every connection
// is refused.
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("cannot read a peer's credentials on this system")
}
