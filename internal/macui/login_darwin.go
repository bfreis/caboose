//go:build darwin

package macui

import (
	"errors"
	"runtime"

	"github.com/bfreis/caboose/internal/menubar"
)

// The login item, through ServiceManagement (macOS 13+): the app registers
// itself as the main-app service.
//
// Threads: LoginItem and SetLoginItem are called synchronously from the
// core's goroutine, and they run there, on a locked OS thread inside an
// autorelease pool, not on the main thread. SMAppService is not AppKit and
// needs no main thread, and a hop to the main thread that waits could
// deadlock while the main thread is busy (an alert's modal loop, a menu
// being tracked); register may also block, which must not freeze the menu.

const noLoginItem = "the login item needs macOS 13 or later"

// onLoginThread runs fn on a locked thread inside a pool.
func onLoginThread(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	withPool(fn)
}

// loginService is [SMAppService mainAppService], 0 where the class is not
// there (before macOS 13).
func loginService() obj {
	c := class("SMAppService")
	if c == 0 {
		return 0
	}
	return c.send(sel("mainAppService"))
}

// LoginItem reports the login item's state.
func (*App) LoginItem() (menubar.LoginState, error) {
	if err := load(); err != nil {
		return menubar.LoginUnavailable, err
	}
	state := menubar.LoginUnavailable
	onLoginThread(func() {
		if svc := loginService(); svc != 0 {
			state = loginStateFromStatus(svc.sendInt(sel("status")))
		}
	})
	return state, nil
}

// SetLoginItem registers or unregisters the app as a login item; a failure
// carries the system's description of it.
func (*App) SetLoginItem(on bool) error {
	if err := load(); err != nil {
		return err
	}
	var err error
	onLoginThread(func() {
		svc := loginService()
		if svc == 0 {
			err = errors.New(noLoginItem)
			return
		}
		var nserr obj
		var ok bool
		if on {
			ok = svc.sendBool(sel("registerAndReturnError:"), &nserr)
		} else {
			ok = svc.sendBool(sel("unregisterAndReturnError:"), &nserr)
		}
		runtime.KeepAlive(&nserr)
		if !ok {
			err = loginError(on, gostring(nserr.send(sel("localizedDescription"))))
		}
	})
	return err
}

// OpenLoginSettings opens System Settings at the Login Items pane, from
// the main thread.
func (*App) OpenLoginSettings() error {
	if err := load(); err != nil {
		return err
	}
	c := class("SMAppService")
	if c == 0 {
		return errors.New(noLoginItem)
	}
	onApp(func() { c.send(sel("openSystemSettingsLoginItems")) })
	return nil
}
