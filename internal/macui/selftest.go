package macui

import (
	"fmt"
	"io"
	"strings"
)

// The self-check's table: every class the glue names and every selector
// it sends or implements. A test reads the package's files and fails
// when a sel("...") or class("...") literal is missing here, when either
// is called with anything but a literal, or when an entry here is used
// nowhere, so a selector cannot skip the check.
//
// Types is the method's Objective-C type encoding as
// method_getTypeEncoding gives it, without the frame offsets (normEncoding):
// return type, self ('@'), _cmd (':'), then each argument. BOOL is written
// 'B' (arm64's; x86_64 encodes it 'c', which normEncoding maps). "" checks
// that the method exists only.
//
// Struct arguments: NSEvent's otherEventWithType:..., which takes an
// NSPoint (CGPoint, two doubles) by value, and setSize:, an NSSize
// (CGSize, two doubles); purego passes them in floating point registers
// on arm64 and amd64 alike.

// selEntry is one selector the glue uses, on the class that has it.
type selEntry struct {
	Class string
	Name  string
	// Meta is set for a class method.
	Meta  bool
	Types string
	// Protocol, when set, is a protocol that declares the method (a
	// delegate method a registered class implements); its declaration's
	// encoding must match Types too.
	Protocol string
}

// Classes the glue registers at run time.
const (
	menuTargetClass  = "CabooseMenuTarget"
	alertTargetClass = "CabooseAlertTarget"
	// notificationDelegateClass is the notification center's delegate.
	notificationDelegateClass = "CabooseNotificationDelegate"
)

// Block arguments are written '@?' (a block is an object of no class);
// the delegate's and the center's methods take completion handlers.

var classTable = []string{
	"NSObject",
	"NSString",
	"NSURL",
	"NSApplication",
	"NSEvent",
	"NSStatusBar",
	"NSStatusItem",
	"NSButton",
	"NSImage",
	"NSBitmapImageRep",
	"NSData",
	"NSMenu",
	"NSMenuItem",
	"NSAlert",
	"NSWindow",
	"NSWorkspace",
	"NSBundle",
	"NSMutableArray",
	"NSError",
	"UNUserNotificationCenter",
	"UNMutableNotificationContent",
	"UNNotificationRequest",
	"UNNotificationResponse",
	"UNNotification",
	"SMAppService",
	menuTargetClass,
	alertTargetClass,
	notificationDelegateClass,
}

var selTable = []selEntry{
	// NSObject.
	{Class: "NSObject", Name: "alloc", Meta: true, Types: "@@:"},
	{Class: "NSObject", Name: "new", Meta: true, Types: "@@:"},
	{Class: "NSObject", Name: "retain", Types: "@@:"},
	{Class: "NSObject", Name: "release", Types: "v@:"},
	{Class: "NSObject", Name: "performSelector:withObject:afterDelay:", Types: "v@::@d"},

	// Foundation.
	{Class: "NSString", Name: "stringWithUTF8String:", Meta: true, Types: "@@:*"},
	{Class: "NSString", Name: "UTF8String", Types: "*@:"},
	{Class: "NSURL", Name: "URLWithString:", Meta: true, Types: "@@:@"},

	// The application.
	{Class: "NSApplication", Name: "sharedApplication", Meta: true, Types: "@@:"},
	{Class: "NSApplication", Name: "setActivationPolicy:", Types: "B@:q"},
	{Class: "NSApplication", Name: "run", Types: "v@:"},
	{Class: "NSApplication", Name: "stop:", Types: "v@:@"},
	{Class: "NSApplication", Name: "abortModal", Types: "v@:"},
	{Class: "NSApplication", Name: "activateIgnoringOtherApps:", Types: "v@:B"},
	{Class: "NSApplication", Name: "postEvent:atStart:", Types: "v@:@B"},
	{Class: "NSEvent", Name: "otherEventWithType:location:modifierFlags:timestamp:windowNumber:context:subtype:data1:data2:",
		Meta: true, Types: "@@:Q{CGPoint=dd}Qdq@sqq"},
	{Class: "NSWorkspace", Name: "sharedWorkspace", Meta: true, Types: "@@:"},
	{Class: "NSWorkspace", Name: "openURL:", Types: "B@:@"},

	// The status item.
	{Class: "NSStatusBar", Name: "systemStatusBar", Meta: true, Types: "@@:"},
	{Class: "NSStatusBar", Name: "statusItemWithLength:", Types: "@@:d"},
	{Class: "NSStatusBar", Name: "removeStatusItem:", Types: "v@:@"},
	{Class: "NSStatusItem", Name: "button", Types: "@@:"},
	{Class: "NSStatusItem", Name: "setMenu:", Types: "v@:@"},
	{Class: "NSButton", Name: "setImage:", Types: "v@:@"},
	{Class: "NSButton", Name: "setTitle:", Types: "v@:@"},
	{Class: "NSButton", Name: "setEnabled:", Types: "v@:B"},
	{Class: "NSButton", Name: "setKeyEquivalent:", Types: "v@:@"},
	{Class: "NSImage", Name: "initWithData:", Types: "@@:@"},
	{Class: "NSImage", Name: "addRepresentation:", Types: "v@:@"},
	{Class: "NSImage", Name: "setSize:", Types: "v@:{CGSize=dd}"},
	{Class: "NSImage", Name: "setTemplate:", Types: "v@:B"},
	{Class: "NSImage", Name: "setAccessibilityDescription:", Types: "v@:@"},
	{Class: "NSBitmapImageRep", Name: "imageRepWithData:", Meta: true, Types: "@@:@"},
	{Class: "NSBitmapImageRep", Name: "setSize:", Types: "v@:{CGSize=dd}"},
	{Class: "NSData", Name: "dataWithBytes:length:", Meta: true, Types: "@@:^vQ"},

	// The menu.
	{Class: "NSMenu", Name: "initWithTitle:", Types: "@@:@"},
	{Class: "NSMenu", Name: "addItem:", Types: "v@:@"},
	{Class: "NSMenu", Name: "removeAllItems", Types: "v@:"},
	{Class: "NSMenu", Name: "setAutoenablesItems:", Types: "v@:B"},
	{Class: "NSMenu", Name: "setDelegate:", Types: "v@:@"},
	{Class: "NSMenuItem", Name: "separatorItem", Meta: true, Types: "@@:"},
	{Class: "NSMenuItem", Name: "initWithTitle:action:keyEquivalent:", Types: "@@:@:@"},
	{Class: "NSMenuItem", Name: "setTarget:", Types: "v@:@"},
	{Class: "NSMenuItem", Name: "setRepresentedObject:", Types: "v@:@"},
	{Class: "NSMenuItem", Name: "representedObject", Types: "@@:"},
	{Class: "NSMenuItem", Name: "setEnabled:", Types: "v@:B"},
	{Class: "NSMenuItem", Name: "setState:", Types: "v@:q"},
	{Class: "NSMenuItem", Name: "setSubmenu:", Types: "v@:@"},

	// Alerts.
	{Class: "NSAlert", Name: "setMessageText:", Types: "v@:@"},
	{Class: "NSAlert", Name: "setInformativeText:", Types: "v@:@"},
	{Class: "NSAlert", Name: "setAlertStyle:", Types: "v@:Q"},
	{Class: "NSAlert", Name: "addButtonWithTitle:", Types: "@@:@"},
	{Class: "NSAlert", Name: "layout", Types: "v@:"},
	{Class: "NSAlert", Name: "window", Types: "@@:"},
	{Class: "NSAlert", Name: "runModal", Types: "q@:"},
	{Class: "NSWindow", Name: "setInitialFirstResponder:", Types: "v@:@"},

	// Notifications.
	{Class: "NSBundle", Name: "mainBundle", Meta: true, Types: "@@:"},
	{Class: "NSBundle", Name: "bundleIdentifier", Types: "@@:"},
	{Class: "NSMutableArray", Name: "array", Meta: true, Types: "@@:"},
	{Class: "NSMutableArray", Name: "addObject:", Types: "v@:@"},
	{Class: "UNUserNotificationCenter", Name: "currentNotificationCenter", Meta: true, Types: "@@:"},
	{Class: "UNUserNotificationCenter", Name: "setDelegate:", Types: "v@:@"},
	{Class: "UNUserNotificationCenter", Name: "requestAuthorizationWithOptions:completionHandler:", Types: "v@:Q@?"},
	{Class: "UNUserNotificationCenter", Name: "addNotificationRequest:withCompletionHandler:", Types: "v@:@@?"},
	{Class: "UNUserNotificationCenter", Name: "removeDeliveredNotificationsWithIdentifiers:", Types: "v@:@"},
	{Class: "UNUserNotificationCenter", Name: "removePendingNotificationRequestsWithIdentifiers:", Types: "v@:@"},
	{Class: "UNMutableNotificationContent", Name: "setTitle:", Types: "v@:@"},
	{Class: "UNMutableNotificationContent", Name: "setBody:", Types: "v@:@"},
	{Class: "UNNotificationRequest", Name: "requestWithIdentifier:content:trigger:", Meta: true, Types: "@@:@@@"},
	{Class: "UNNotificationRequest", Name: "identifier", Types: "@@:"},
	{Class: "UNNotificationResponse", Name: "notification", Types: "@@:"},
	{Class: "UNNotification", Name: "request", Types: "@@:"},

	// The login item. The error out-parameter is an NSError**.
	{Class: "SMAppService", Name: "mainAppService", Meta: true, Types: "@@:"},
	{Class: "SMAppService", Name: "status", Types: "q@:"},
	{Class: "SMAppService", Name: "registerAndReturnError:", Types: "B@:^@"},
	{Class: "SMAppService", Name: "unregisterAndReturnError:", Types: "B@:^@"},
	{Class: "SMAppService", Name: "openSystemSettingsLoginItems", Meta: true, Types: "v@:"},
	{Class: "NSError", Name: "localizedDescription", Types: "@@:"},

	// The classes the glue registers, and what they implement.
	{Class: menuTargetClass, Name: "itemSelected:", Types: "v@:@"},
	{Class: menuTargetClass, Name: "menuWillOpen:", Types: "v@:@", Protocol: "NSMenuDelegate"},
	{Class: menuTargetClass, Name: "menuDidClose:", Types: "v@:@", Protocol: "NSMenuDelegate"},
	{Class: menuTargetClass, Name: "worksWhenModal", Types: "B@:"},
	{Class: alertTargetClass, Name: "showNext:", Types: "v@:@"},
	{Class: notificationDelegateClass, Name: "userNotificationCenter:didReceiveNotificationResponse:withCompletionHandler:",
		Types: "v@:@@@?", Protocol: "UNUserNotificationCenterDelegate"},
	{Class: notificationDelegateClass, Name: "userNotificationCenter:willPresentNotification:withCompletionHandler:",
		Types: "v@:@@@?", Protocol: "UNUserNotificationCenterDelegate"},
}

// lookup is what the self-check asks of the Objective-C runtime; the
// darwin build answers it from the running system, a test from a fake.
type lookup interface {
	HasClass(className string) bool
	Responds(className, selName string, meta bool) bool
	// Encoding is the method's type encoding, false when there is none.
	Encoding(className, selName string, meta bool) (string, bool)
	// ProtocolEncoding is the encoding the protocol declares for an
	// optional or required instance method, false when the protocol or
	// the method is not found.
	ProtocolEncoding(proto, selName string) (string, bool)
}

// check holds the table to the runtime: it writes each problem to w, or
// one line saying how much it checked, and returns an error when there
// is any problem.
func check(l lookup, w io.Writer) error {
	var problems []string
	bad := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	classes := map[string]bool{}
	for _, c := range classTable {
		ok := l.HasClass(c)
		classes[c] = ok
		if !ok {
			bad("missing class %s", c)
		}
	}
	for _, e := range selTable {
		name := e.Class + " " + methodName(e)
		if !classes[e.Class] {
			continue // already said
		}
		if !l.Responds(e.Class, e.Name, e.Meta) {
			bad("missing selector %s", name)
			continue
		}
		if e.Types == "" {
			continue
		}
		want := normEncoding(e.Types)
		got, ok := l.Encoding(e.Class, e.Name, e.Meta)
		switch {
		case !ok:
			bad("no type encoding for %s", name)
		case normEncoding(got) != want:
			bad("type encoding of %s: got %q, want %q", name, normEncoding(got), want)
		}
		if e.Protocol != "" {
			got, ok := l.ProtocolEncoding(e.Protocol, e.Name)
			switch {
			case !ok:
				bad("protocol %s declares no %s", e.Protocol, e.Name)
			case normEncoding(got) != want:
				bad("type encoding of %s in %s: got %q, want %q", e.Name, e.Protocol, normEncoding(got), want)
			}
		}
	}
	for _, p := range problems {
		fmt.Fprintln(w, p)
	}
	if len(problems) > 0 {
		return fmt.Errorf("self-check: %d problems", len(problems))
	}
	fmt.Fprintf(w, "ok: %d classes, %d selectors\n", len(classTable), len(selTable))
	return nil
}

// methodName is how an entry reads: "-name" or "+name".
func methodName(e selEntry) string {
	if e.Meta {
		return "+" + e.Name
	}
	return "-" + e.Name
}

// normEncoding is a type encoding as the table compares it: frame
// offsets (digits) and type qualifiers (const, in, out, oneway ...)
// dropped, a quoted class or field name dropped, and x86_64's 'c' for BOOL
// written 'B'. A struct's or union's name is kept as it is.
func normEncoding(s string) string {
	var b strings.Builder
	inName := false // between '{' or '(' and '='
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inName:
			b.WriteByte(c)
			if c == '=' {
				inName = false
			}
			if c == '}' || c == ')' {
				inName = false // a struct with no field list: "{Name}"
			}
		case c == '<':
			// A block's signature after "@?": "@?<v@?Q>". Dropped, nested
			// brackets included.
			depth := 0
			for ; i < len(s); i++ {
				if s[i] == '<' {
					depth++
				} else if s[i] == '>' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
		case c == '"':
			if j := strings.IndexByte(s[i+1:], '"'); j >= 0 {
				i += j + 1
			}
		case c >= '0' && c <= '9':
		case strings.IndexByte("rnNoORV", c) >= 0:
		case c == 'c':
			b.WriteByte('B')
		case c == '{' || c == '(':
			b.WriteByte(c)
			inName = true
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
