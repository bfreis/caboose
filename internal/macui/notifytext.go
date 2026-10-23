package macui

import (
	"fmt"

	"github.com/bfreis/caboose/internal/menubar"
)

// What is decidable about notifications and the login item, apart from
// the system: constants, the mapping of a status, the bounds on text.

const (
	// authOptionAlert is UNAuthorizationOptionAlert: alerts only, no sound
	// and no badge.
	authOptionAlert = 1 << 2
	// presentBanner and presentList are UNNotificationPresentationOptions
	// BANNER and LIST: shown even while the app is frontmost.
	presentBanner = 1 << 4
	presentList   = 1 << 3
	presentAll    = presentBanner | presentList

	maxNotifTitle = 120
	maxNotifBody  = 400
	maxLoginError = 200
)

// authSignature is the type encoding of the authorization completion
// handler, void (^)(BOOL granted, NSError *error).
var authSignature = [...]byte{'v', '@', '?', 'B', '@', 0}

// boolArg reads a BOOL a callback received in a register: only its low
// byte is the value.
func boolArg(r uintptr) bool { return r&0xff != 0 }

// clipNotification bounds a notification's text and makes it printable.
func clipNotification(n menubar.Notification) menubar.Notification {
	return menubar.Notification{
		ID:    notificationID(n.ID),
		Title: menubar.Clip(n.Title, maxNotifTitle),
		Body:  menubar.Clip(n.Body, maxNotifBody),
	}
}

// maxNotifID bounds a notification's identifier.
const maxNotifID = 64

// notificationID is id when it can be an identifier: printable ASCII and
// at most maxNotifID bytes. Anything else is "", which posts and withdraws
// nothing, since an identifier that does not convert to an NSString would
// reach UserNotifications as nil and raise.
func notificationID(id string) string {
	if len(id) > maxNotifID {
		return ""
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return ""
		}
	}
	return id
}

// SMAppServiceStatus values.
const (
	smNotRegistered    = 0
	smEnabled          = 1
	smRequiresApproval = 2
	smNotFound         = 3
)

// loginStateFromStatus maps an SMAppServiceStatus; a value it does not
// know is Unavailable.
func loginStateFromStatus(status int) menubar.LoginState {
	switch status {
	case smNotRegistered, smNotFound:
		return menubar.LoginOff
	case smEnabled:
		return menubar.LoginOn
	case smRequiresApproval:
		return menubar.LoginNeedsApproval
	}
	return menubar.LoginUnavailable
}

// loginError is the error for a failed register or unregister, quoting
// the NSError's description, bounded and printable.
func loginError(register bool, desc string) error {
	verb := "unregister"
	if register {
		verb = "register"
	}
	d := menubar.Clip(desc, maxLoginError)
	if d == "" {
		return fmt.Errorf("could not %s the login item", verb)
	}
	return fmt.Errorf("could not %s the login item: %s", verb, d)
}
