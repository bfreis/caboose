// Package uiport is where the host's caboose web UI listens: a fixed
// port on 127.0.0.1, one for a release's daemon and one for a checkout's
// build, which runs a daemon of its own beside the release's. The page
// approves what a sandbox asks for, so no sandbox may ever reach it
// through caboose: its ports are never forwarded, opened from the
// sandbox, reached through the outbound proxy or published by run_args.
// Those refusals all ask Is. It imports nothing of caboose's, so every
// package that refuses can use it.
package uiport

import (
	"net/netip"
	"strconv"
)

const (
	// Release is the port a release's daemon serves the page on.
	Release = 7171
	// Dev is the port a checkout's build serves it on.
	Dev = 7172
)

// Is reports whether port is one of the web UI's, a release's or a
// checkout's: what a sandbox may never listen on, open or reach.
func Is(port int) bool { return port == Release || port == Dev }

// Covering is the first of the web UI's ports within lo..hi, and whether
// there is one.
func Covering(lo, hi int) (int, bool) {
	for _, p := range []int{Release, Dev} {
		if lo <= p && p <= hi {
			return p, true
		}
	}
	return 0, false
}

// Port is the port the page is served on: Dev for a checkout's build.
func Port(dev bool) int {
	if dev {
		return Dev
	}
	return Release
}

// Addr is the address the page is served on, 127.0.0.1 and Port.
func Addr(dev bool) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(Port(dev)))
}

// Origin is the page's origin, as a browser writes it:
// "http://127.0.0.1:7171".
func Origin(dev bool) string { return "http://127.0.0.1:" + strconv.Itoa(Port(dev)) }

// What is what a refusal says a UI port is: why no sandbox may have it.
const What = "the host's caboose web UI, which approves what a sandbox asks for"
