package macui

import "github.com/bfreis/caboose/internal/menubar"

// Bounds on the menu the glue builds. The core's menu stays far inside
// them; past them the rest is left out rather than built.
const (
	maxMenuDepth = 4
	maxMenuItems = 1000
)

// flatItem is one NSMenuItem to build, in the order they are built: a
// parent always comes before its children.
type flatItem struct {
	// Parent is the index of the item whose submenu holds this one, or -1
	// for the status item's menu.
	Parent    int
	Separator bool
	Title     string
	// ID is what itemSelected: posts; "" for an item with no action.
	ID      string
	Enabled bool
	Checked bool
	// Submenu is set when the item has children, built as its submenu.
	Submenu bool
}

// flatten lays the menu's tree out as the items to build. Titles are
// cleaned and clipped again (the core already did), an info line is
// never enabled, and an item with no ID has no action.
func flatten(m menubar.Menu) []flatItem {
	var out []flatItem
	var walk func(items []menubar.Item, parent, depth int)
	walk = func(items []menubar.Item, parent, depth int) {
		for _, it := range items {
			if len(out) >= maxMenuItems {
				return
			}
			if it.Separator {
				out = append(out, flatItem{Parent: parent, Separator: true})
				continue
			}
			sub := len(it.Children) > 0 && depth+1 < maxMenuDepth
			out = append(out, flatItem{
				Parent:  parent,
				Title:   menubar.Clip(it.Title, menubar.MaxTitle),
				ID:      it.ID,
				Enabled: it.Enabled && !it.Info,
				Checked: it.Checked,
				Submenu: sub,
			})
			if sub {
				walk(it.Children, len(out)-1, depth+1)
			}
		}
	}
	walk(m.Items, -1, 0)
	return out
}
