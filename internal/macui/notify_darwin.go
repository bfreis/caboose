//go:build darwin

package macui

import (
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/bfreis/caboose/internal/menubar"
)

// Notifications, through UserNotifications. +currentNotificationCenter
// raises an exception in a process with no bundle identifier (an unbundled
// `caboose app`), so without one nothing here touches the framework: Notify
// and Withdraw do nothing and no authorization answer is ever posted, so
// the core keeps notifications off.

// block is a pointer to an Objective-C block literal, 0 for nil.
type block uintptr

var notif struct {
	// enabled is set, on the main thread, once the delegate is set and
	// the authorization asked for.
	enabled  bool
	delegate obj

	// The authorization completion handler: a global block, allocated here
	// once and never moved or freed, whose invoke function is one callback
	// created once (registerNotificationClass).
	authBlock  blockLiteral
	authDesc   blockDescriptor
	authInvoke uintptr
}

// registerNotificationClass registers the center's delegate and the
// callback of the authorization block. It runs once, with the others.
func registerNotificationClass() error {
	notif.authInvoke = purego.NewCallback(func(_, granted, _ uintptr) {
		ui.events.notificationAuth(boolArg(granted))
	})
	clicked := purego.NewCallback(func(_, _, _, response, handler uintptr) {
		didReceive(response, handler)
	})
	present := purego.NewCallback(func(_, _, _, _, handler uintptr) {
		invokeBlock(handler, presentAll)
	})
	_, err := registerClass(notificationDelegateClass, "UNUserNotificationCenterDelegate", []method{
		{sel("userNotificationCenter:didReceiveNotificationResponse:withCompletionHandler:"), "v@:@@@?", clicked},
		{sel("userNotificationCenter:willPresentNotification:withCompletionHandler:"), "v@:@@@?", present},
	})
	return err
}

// didReceive posts a click on a notification, and always calls the
// completion handler: the system waits for it.
func didReceive(response, handler uintptr) {
	defer invokeBlock(handler)
	withPool(func() {
		id := gostring(obj(response).send(sel("notification")).send(sel("request")).send(sel("identifier")))
		ui.events.notificationClicked(id)
	})
}

// invokeBlock calls a block the system handed us, through its invoke
// pointer, with the block first.
func invokeBlock(b uintptr, args ...uintptr) {
	fn := readInvoke(b)
	if fn == 0 {
		return
	}
	purego.SyscallN(fn, append([]uintptr{b}, args...)...)
}

// hasBundleID reports whether the process has a bundle identifier.
func hasBundleID() bool {
	b := class("NSBundle").send(sel("mainBundle"))
	return b != 0 && gostring(b.send(sel("bundleIdentifier"))) != ""
}

func notificationCenter() obj {
	return class("UNUserNotificationCenter").send(sel("currentNotificationCenter"))
}

// setUpNotifications sets the delegate and asks for authorization (alerts
// only), on the main thread, inside Run's pool. Anything missing leaves
// notifications off.
func setUpNotifications() {
	if !hasBundleID() {
		return
	}
	isa, err := purego.Dlsym(purego.RTLD_DEFAULT, "_NSConcreteGlobalBlock")
	if err != nil || isa == 0 {
		return
	}
	center := notificationCenter()
	if center == 0 {
		return
	}
	notif.delegate = class(notificationDelegateClass).send(sel("new"))
	center.send(sel("setDelegate:"), notif.delegate)
	initGlobalBlock(&notif.authBlock, &notif.authDesc, isa, notif.authInvoke, &authSignature[0])
	center.send(sel("requestAuthorizationWithOptions:completionHandler:"),
		uint(authOptionAlert), block(uintptr(unsafe.Pointer(&notif.authBlock))))
	notif.enabled = true
}

// Notify posts a notification; a second with the same identifier replaces
// the first. Nothing without a bundle.
func (*App) Notify(n menubar.Notification) {
	n = clipNotification(n)
	if n.ID == "" {
		return
	}
	onApp(func() {
		if !notif.enabled {
			return
		}
		content := class("UNMutableNotificationContent").send(sel("new"))
		if content == 0 {
			return
		}
		defer content.send(sel("release"))
		content.send(sel("setTitle:"), nsstring(n.Title))
		content.send(sel("setBody:"), nsstring(n.Body))
		id := nsstring(n.ID)
		if id == 0 {
			return
		}
		req := class("UNNotificationRequest").send(sel("requestWithIdentifier:content:trigger:"),
			id, content, obj(0))
		if req == 0 {
			return
		}
		notificationCenter().send(sel("addNotificationRequest:withCompletionHandler:"), req, block(0))
	})
}

// Withdraw removes the notifications with these identifiers, delivered or
// still pending.
func (*App) Withdraw(ids []string) {
	var keep []string
	for _, id := range ids {
		if id = notificationID(id); id != "" {
			keep = append(keep, id)
		}
	}
	if len(keep) == 0 {
		return
	}
	onApp(func() {
		if !notif.enabled {
			return
		}
		arr := class("NSMutableArray").send(sel("array"))
		for _, id := range keep {
			if s := nsstring(id); s != 0 {
				arr.send(sel("addObject:"), s)
			}
		}
		center := notificationCenter()
		center.send(sel("removeDeliveredNotificationsWithIdentifiers:"), arr)
		center.send(sel("removePendingNotificationRequestsWithIdentifiers:"), arr)
	})
}
