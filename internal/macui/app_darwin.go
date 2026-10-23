//go:build darwin

package macui

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ebitengine/purego"

	"github.com/bfreis/caboose/internal/menubar"
)

// AppKit's constants the glue uses.
const (
	activationPolicyAccessory = 1    // NSApplicationActivationPolicyAccessory: no Dock icon
	variableStatusItemLength  = -1.0 // NSVariableStatusItemLength
	eventTypeApplicationDef   = 15   // NSEventTypeApplicationDefined
	controlStateOn            = 1    // NSControlStateValueOn
	controlStateOff           = 0
	alertStyleWarning         = 0 // NSAlertStyleWarning
)

// openTimeout bounds Open's wait for the main thread.
const openTimeout = 10 * time.Second

// App is the menu-bar app's AppKit glue. There is one NSApplication in a
// process, so every App shares its state; Run may be called once.
type App struct{}

var _ menubar.UI = (*App)(nil)

// New returns the glue. Nothing is loaded until Run.
func New() *App { return &App{} }

// The glue's state. ui.events is read on any thread; the rest only on the
// main thread.
var ui struct {
	started atomic.Bool
	running atomic.Bool
	events  sink

	app         obj
	statusItem  obj
	menu        obj
	menuTarget  obj
	alertTarget obj
	alerts      alertQueue
	menus       menuHold
	// icons are the status item's images by name, made once.
	icons map[string]obj
}

// Run takes over the main thread: it loads AppKit, shows the status item
// and runs the application until Quit. The caller must be on the main
// thread (the main goroutine, locked to it before main runs).
func (*App) Run(ev menubar.Events, ready func()) error {
	if !ui.started.CompareAndSwap(false, true) {
		return errors.New("the menu-bar app is already running")
	}
	if err := load(); err != nil {
		return err
	}
	if !onMainThread() {
		return errors.New("the menu-bar app must run on the main thread")
	}
	var err error
	withPool(func() { err = setUp(ev) })
	if err != nil {
		return err
	}
	ui.running.Store(true)
	if ready != nil {
		ready()
	}
	withPool(func() { ui.app.send(sel("run")) })
	ui.running.Store(false)
	ui.events.set(nil)
	withPool(func() {
		class("NSStatusBar").send(sel("systemStatusBar")).send(sel("removeStatusItem:"), ui.statusItem)
	})
	return nil
}

// setUp makes the application an accessory (no Dock icon, no menu bar of
// its own) and creates the status item with an empty menu.
func setUp(ev menubar.Events) error {
	ui.app = class("NSApplication").send(sel("sharedApplication"))
	if ui.app == 0 {
		return errors.New("no NSApplication")
	}
	ui.app.sendBool(sel("setActivationPolicy:"), activationPolicyAccessory)
	item := class("NSStatusBar").send(sel("systemStatusBar")).send(sel("statusItemWithLength:"), float64(variableStatusItemLength))
	if item == 0 {
		return errors.New("no status item")
	}
	ui.statusItem = item.send(sel("retain"))
	ui.menuTarget = class(menuTargetClass).send(sel("new"))
	ui.alertTarget = class(alertTargetClass).send(sel("new"))
	ui.menu = class("NSMenu").send(sel("alloc")).send(sel("initWithTitle:"), nsstring(""))
	ui.menu.send(sel("setAutoenablesItems:"), false)
	ui.menu.send(sel("setDelegate:"), ui.menuTarget)
	ui.statusItem.send(sel("setMenu:"), ui.menu)
	ui.events.set(ev)
	setUpNotifications()
	setIcon(menubar.IconState{Image: menubar.ImageIdle})
	return nil
}

// registerClasses registers the menu's target and delegate, and the
// alerts' target. Each method's IMP is a purego callback.
func registerClasses() error {
	item := purego.NewCallback(func(_, _, sender uintptr) {
		if sender != 0 {
			ui.events.selected(gostring(obj(sender).send(sel("representedObject"))))
		}
	})
	opened := purego.NewCallback(func(_, _, _ uintptr) { menuOpened() })
	closed := purego.NewCallback(func(_, _, _ uintptr) { menuClosed() })
	// worksWhenModal: while an alert runs modally, AppKit sends a menu
	// item's action to its target only when the target answers YES, and
	// drops it otherwise (the menu still opens and closes). Every item
	// stays live under an alert, Quit included: what an item does that
	// needs the user's word queues an alert of its own behind the one
	// shown.
	modal := purego.NewCallback(func(_, _ uintptr) uintptr { return 1 })
	if _, err := registerClass(menuTargetClass, "NSMenuDelegate", []method{
		{sel("itemSelected:"), "v@:@", item},
		{sel("menuWillOpen:"), "v@:@", opened},
		{sel("menuDidClose:"), "v@:@", closed},
		{sel("worksWhenModal"), "B@:", modal},
	}); err != nil {
		return err
	}
	next := purego.NewCallback(func(_, _, _ uintptr) { showNext() })
	if _, err := registerClass(alertTargetClass, "", []method{
		{sel("showNext:"), "v@:@", next},
	}); err != nil {
		return err
	}
	return registerNotificationClass()
}

// onApp queues fn on the main thread while the application runs, and
// drops it otherwise.
func onApp(fn func()) {
	if ui.running.Load() {
		onMain(func() {
			if ui.running.Load() {
				fn()
			}
		})
	}
}

// SetIcon sets the status item's image and title.
func (*App) SetIcon(s menubar.IconState) { onApp(func() { setIcon(s) }) }

func setIcon(s menubar.IconState) {
	button := ui.statusItem.send(sel("button"))
	if button == 0 {
		return
	}
	title := menubar.Clip(s.Title, menubar.MaxTitle)
	img := icon(s.Image)
	if img != 0 {
		img.send(sel("setAccessibilityDescription:"), nsstring(menubar.Clip(s.Description, menubar.MaxTitle)))
	} else {
		title = "caboose" + title // no such image: a word instead
	}
	button.send(sel("setImage:"), img)
	button.send(sel("setTitle:"), nsstring(title))
}

// icon is the named image, made on first use from its embedded PNGs (one
// representation for each scale, the image iconPointsW by iconPointsH
// points) and kept; 0 when there is none.
func icon(name string) obj {
	if img, ok := ui.icons[name]; ok {
		return img
	}
	x1, x2, ok := iconPNG(name)
	if !ok {
		return 0
	}
	size := nsSize{iconPointsW, iconPointsH}
	img := class("NSImage").send(sel("alloc")).send(sel("initWithData:"), nsdata(x1))
	if img != 0 {
		if rep := class("NSBitmapImageRep").send(sel("imageRepWithData:"), nsdata(x2)); rep != 0 {
			rt.sendSize(rep, sel("setSize:"), size)
			img.send(sel("addRepresentation:"), rep)
		}
		rt.sendSize(img, sel("setSize:"), size)
		img.send(sel("setTemplate:"), true)
	}
	if ui.icons == nil {
		ui.icons = map[string]obj{}
	}
	ui.icons[name] = img
	return img
}

// Open opens url in its default application (the browser) through
// NSWorkspace, on the main thread, and waits for it.
func (*App) Open(url string) error {
	if !ui.running.Load() {
		return errors.New("the menu-bar app is not running")
	}
	done := make(chan error, 1)
	onApp(func() {
		u := class("NSURL").send(sel("URLWithString:"), nsstring(url))
		switch {
		case u == 0:
			done <- fmt.Errorf("not a URL: %q", menubar.Clip(url, menubar.MaxTitle))
		case !class("NSWorkspace").send(sel("sharedWorkspace")).sendBool(sel("openURL:"), u):
			done <- errors.New("the system could not open the URL")
		default:
			done <- nil
		}
	})
	select {
	case err := <-done:
		return err
	case <-time.After(openTimeout):
		return errors.New("opening the URL timed out")
	}
}

// Quit ends Run: an alert shown is closed without an answer, and the
// application's run loop stops. Under a modal alert the hop still runs:
// runModal is entered from a run loop timer, never from a function of the
// main queue, so the queue is served in the modal panel's mode, and
// abortModal is the call AppKit documents for ending a modal loop from
// outside its own events.
func (*App) Quit() {
	onApp(func() {
		if ui.alerts.quit() {
			ui.app.send(sel("abortModal")) // showNext stops the application once runModal returns
			return
		}
		stopApp()
	})
}

// stopApp stops [NSApp run]. stop: takes effect after the next event, and
// a dispatched function is none, so an application-defined event follows.
func stopApp() {
	ui.app.send(sel("stop:"), obj(0))
	e := rt.sendEvent(class("NSEvent"),
		sel("otherEventWithType:location:modifierFlags:timestamp:windowNumber:context:subtype:data1:data2:"),
		eventTypeApplicationDef, nsPoint{}, 0, 0, 0, 0, 0, 0, 0)
	if e != 0 {
		ui.app.send(sel("postEvent:atStart:"), e, true)
	}
}
