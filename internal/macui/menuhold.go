package macui

import "github.com/bfreis/caboose/internal/menubar"

// menuHold keeps a new menu from replacing the open one: while the menu
// is open (between menuWillOpen: and menuDidClose:) the newest tree is
// held, and applied once it closes, so that no item moves under the
// pointer. The core holds its own trees the same way, but learns of the
// menu's opening only later, after a SetMenu may already be on its way.
// It is only ever used on the main thread.
type menuHold struct {
	open bool
	held *menubar.Menu
}

// set reports whether m is to be applied now; while the menu is open it
// is held instead, replacing any tree held before.
func (h *menuHold) set(m menubar.Menu) bool {
	if h.open {
		h.held = &m
		return false
	}
	return true
}

func (h *menuHold) opened() { h.open = true }

// closed marks the menu closed and returns the tree held meanwhile, if
// any, to apply now.
func (h *menuHold) closed() (menubar.Menu, bool) {
	h.open = false
	if h.held == nil {
		return menubar.Menu{}, false
	}
	m := *h.held
	h.held = nil
	return m, true
}
