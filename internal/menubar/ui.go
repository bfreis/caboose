// Package menubar is the menu-bar app's core: everything it decides, with
// no user interface of its own. It talks to the daemon (Daemon), keeps
// the model the menu is built from, and drives a UI, which a platform's
// glue implements by drawing what it is told and posting back, through
// Events, what the user did. The glue decides nothing: it maps item IDs
// back to Selected and buttons to Confirmed or URLAnswered.
package menubar

// UI is what the glue implements. Every method is called from the core's
// own goroutine, never the main thread: the glue hops to it. None may
// block on the user (Confirm, AskURL and Notify answer through Events).
type UI interface {
	// Run takes over the calling (main) thread until Quit. It calls ready
	// once the status item exists; ev is where it posts what the user
	// does from then on.
	Run(ev Events, ready func()) error
	SetIcon(IconState)
	// SetMenu replaces the menu. The core never calls it while the menu
	// is open (between MenuOpened and MenuClosed), so that no item moves
	// under the pointer.
	SetMenu(Menu)
	Confirm(Confirmation) // async; answered through Events.Confirmed
	AskURL(URLDialog)     // async; answered through Events.URLAnswered
	Dismiss(id string)    // close an open alert (what it asked went away)
	Notify(Notification)  // async; clicks through Events.NotificationClicked
	Withdraw(ids []string)
	Open(url string) error
	LoginItem() (LoginState, error)
	SetLoginItem(on bool) error
	// OpenLoginSettings opens the system's login items settings, where a
	// login item that needs approval is approved.
	OpenLoginSettings() error
	// Alert shows a one-off informational alert, with only OK; its
	// closing by the user comes through Events.AlertClosed. One closed
	// by Quit posts nothing.
	Alert(id, text string)
	Quit()
}

// Events is what the glue posts. Every method returns at once: it only
// queues the event for the core's goroutine, never calls the daemon.
type Events interface {
	Selected(itemID string)
	MenuOpened()
	MenuClosed()
	Confirmed(id string, ok bool)
	URLAnswered(id string, open bool)
	NotificationClicked(id string)
	NotificationAuth(granted bool)
	AlertClosed(id string)
}

// Icon images: caboose's railcar, as an outline (idle) or filled
// (running), which the glue carries and draws as template images.
const (
	ImageIdle    = "railcar"
	ImageRunning = "railcar.fill"
)

// IconState is what the status item shows.
type IconState struct {
	Image string // ImageIdle or ImageRunning
	// Title is the text beside the image: "", " N" or " !".
	Title string
	// Description is the accessibility description.
	Description string
}

// Menu is the whole menu, top to bottom.
type Menu struct {
	Items []Item
}

// Item is one menu item. A separator has only Separator set. An Info item
// is a disabled line that only informs.
type Item struct {
	ID        string // opaque to the glue; "" for an item nothing selects
	Title     string
	Enabled   bool
	Checked   bool
	Info      bool
	Separator bool
	Children  []Item
}

// Confirmation is an alert that asks before the core does something: an
// answer to a request, a stop or a restart. Its buttons are Cancel (the
// default) and Action; only Action, pressed deliberately, confirms.
type Confirmation struct {
	ID      string
	Message string // the question: "Allow #12 for 8 hours?"
	// Effect is what the request does on the host (host text).
	Effect string
	// AnswerEffect is what this answer does (host text).
	AnswerEffect string
	// SandboxSays is the sandbox's own words, under SandboxLabel, or "".
	SandboxSays string
	Action      string // the action button's title, in full
}

// URLDialog asks whether to open a URL a sandbox asked to open.
type URLDialog struct {
	ID          string
	Env         string
	URL         string // the host-checked URL, or the host's effect line
	SandboxSays string
}

// Notification is one notification to post. A notification with the ID
// of one posted before replaces it.
type Notification struct {
	ID    string
	Title string
	Body  string
}

// LoginState is the app's login item, as the system has it.
type LoginState int

const (
	LoginUnavailable   LoginState = iota // the system offers none
	LoginOff                             // not registered
	LoginOn                              // registered and enabled
	LoginNeedsApproval                   // registered, waiting for the user's approval
)
