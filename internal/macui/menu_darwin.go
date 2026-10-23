//go:build darwin

package macui

import "github.com/bfreis/caboose/internal/menubar"

// SetMenu replaces the status item's menu with m. The core never calls it
// while the menu is open, so no item moves under the pointer.
func (*App) SetMenu(m menubar.Menu) { onApp(func() { applyMenu(m) }) }

// applyMenu builds m now, or holds it while the menu is open (menuHold).
func applyMenu(m menubar.Menu) {
	if ui.menus.set(m) {
		setMenu(m)
	}
}

// menuOpened and menuClosed are the menu delegate's. A tree held while
// the menu was open is applied on a later turn of the main queue, after
// AppKit has sent the chosen item's action.
func menuOpened() {
	ui.menus.opened()
	ui.events.menuOpened()
}

func menuClosed() {
	// Queue the held tree before telling the core: its answer may be a
	// newer SetMenu, which must land after this one.
	if m, ok := ui.menus.closed(); ok {
		onApp(func() { applyMenu(m) })
	}
	ui.events.menuClosed()
}

func setMenu(m menubar.Menu) {
	ui.menu.send(sel("removeAllItems"))
	items := flatten(m)
	submenus := make([]obj, len(items))
	for i, it := range items {
		parent := ui.menu
		if it.Parent >= 0 {
			parent = submenus[it.Parent]
		}
		if parent == 0 {
			continue
		}
		if it.Separator {
			parent.send(sel("addItem:"), class("NSMenuItem").send(sel("separatorItem")))
			continue
		}
		var action selector
		if it.ID != "" {
			action = sel("itemSelected:")
		}
		// No key equivalent, ever: nothing answers from the keyboard.
		mi := class("NSMenuItem").send(sel("alloc")).send(sel("initWithTitle:action:keyEquivalent:"),
			nsstring(it.Title), action, nsstring(""))
		if mi == 0 {
			continue
		}
		if it.ID != "" {
			mi.send(sel("setTarget:"), ui.menuTarget)
			mi.send(sel("setRepresentedObject:"), nsstring(it.ID))
		}
		mi.send(sel("setEnabled:"), it.Enabled)
		state := controlStateOff
		if it.Checked {
			state = controlStateOn
		}
		mi.send(sel("setState:"), state)
		if it.Submenu {
			sub := class("NSMenu").send(sel("alloc")).send(sel("initWithTitle:"), nsstring(it.Title))
			sub.send(sel("setAutoenablesItems:"), false)
			mi.send(sel("setSubmenu:"), sub)
			sub.send(sel("release"))
			submenus[i] = sub
		}
		parent.send(sel("addItem:"), mi)
		mi.send(sel("release"))
	}
}
