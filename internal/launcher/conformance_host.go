package launcher

import (
	"github.com/bfreis/caboose/internal/conformance"
)

// confHost is the real conformance.Host: the guard's canaries under B,
// made and read from the host side without following a symlink
// (conformance.GuardHost), and the host-command, trust, egress
// and checkpoint calls (conformance_ops.go), for the suite's own
// environment, whose App is a, whose daemon is daemon and whose sandbox,
// by its pinned identity, is box.
type confHost struct {
	*conformance.GuardHost
	a      *App
	daemon *confDaemon
	box    *confBox
	// facts are what the daemon calls read of this machine
	// (conformance_daemonops.go).
	facts confHostFacts
}

var _ conformance.Host = (*confHost)(nil)

// The daemon checks read the placed probe's hash through it.
var _ interface{ ProbeHash() string } = (*confHost)(nil)

// The mount checks give the probe the run's nonce through it.
var _ interface{ Nonce() string } = (*confHost)(nil)

func newConfHost(g *conformance.Guard, a *App) *confHost {
	return &confHost{GuardHost: &conformance.GuardHost{G: g}, a: a, facts: realConfHostFacts()}
}
