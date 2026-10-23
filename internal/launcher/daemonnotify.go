package launcher

import (
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/hostlink"
)

// notifierActions are this machine's browser and dialogs, with the
// daemon's Notifier for notifications: a sandbox's told from the host's
// (hostlink.SandboxNotifier), so that the menu-bar app shows each as what
// it is.
type notifierActions struct {
	hostlink.Actions
	n daemon.EnvNotifier
}

func (a notifierActions) Notify(title, text string) error { return a.n.Notify(title, text) }

func (a notifierActions) SandboxNotify(title, text string) error {
	return a.n.SandboxNotify(title, text)
}

// daemonNotify is the daemon's notifications for env's src, as a
// component's Notify: made printable and cut short as hostlink.Notify
// does, then to the Notifier, whose fallback is the host's own.
func daemonNotify(n *daemon.Notifier, env string, src daemon.Source) func(title, text string) {
	a := notifierActions{Actions: hostlink.System{}, n: n.For(env, src)}
	return func(title, text string) { _ = hostlink.Notify(a, title, text) }
}
