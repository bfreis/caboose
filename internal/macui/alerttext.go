package macui

import (
	"strings"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/menubar"
)

// NSAlert's results (NSModalResponse) for an alert whose buttons are, in
// order, Cancel and the action.
const (
	firstButton   = 1000  // NSAlertFirstButtonReturn: Cancel
	secondButton  = 1001  // NSAlertSecondButtonReturn: the action
	responseStop  = -1000 // NSModalResponseStop
	responseAbort = -1001 // NSModalResponseAbort: abortModal, as Dismiss does
)

// approved reports whether an alert's result is the action button, the
// only result that approves anything. Cancel, an aborted or stopped alert
// and any other value are a refusal.
func approved(code int) bool { return code == secondButton }

// Button titles, and the delay before an alert's action button is enabled.
const (
	cancelTitle = "Cancel"
	openTitle   = "Open"
	okTitle     = "OK"
	// confirmTitle names the action of a confirmation that names none.
	confirmTitle = "Confirm"
)

// maxInformative bounds an alert's informative text, which is host text
// but carries sandbox-supplied parts.
const maxInformative = 4000

// alertText is what an alert shows: its message (the bold line), its
// informative text and its action button, "" for an alert with only OK.
type alertText struct {
	Message     string
	Informative string
	Action      string
}

// confirmText is a confirmation's alert: the question; then, in this
// order, what the request does on the host, what this answer does, and
// the sandbox's own words under menubar.SandboxLabel; the action named in
// full.
func confirmText(c menubar.Confirmation) alertText {
	action := menubar.Clip(c.Action, menubar.MaxTitle)
	if action == "" {
		action = confirmTitle
	}
	return alertText{
		Message:     menubar.Clip(c.Message, menubar.MaxTitle*2),
		Informative: informative(cleanLines(c.Effect), cleanLines(c.AnswerEffect), sandboxBlock(c.SandboxSays)),
		Action:      action,
	}
}

// urlText is the URL dialog's alert: who asks, the host-checked URL on a
// line of its own, then the sandbox's own words.
func urlText(d menubar.URLDialog) alertText {
	who := menubar.Clip(d.Env, menubar.MaxTitle)
	msg := "A sandbox asks to open a URL"
	if who != "" {
		msg = who + " asks to open a URL"
	}
	return alertText{
		Message:     msg,
		Informative: informative(cleanLines(d.URL), sandboxBlock(d.SandboxSays)),
		Action:      openTitle,
	}
}

// infoText is a one-off informational alert, with only OK.
func infoText(text string) alertText {
	return alertText{Message: "caboose", Informative: informative(cleanLines(text))}
}

// informative joins the non-empty parts with a blank line, within
// maxInformative.
func informative(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return clipRunes(strings.Join(keep, "\n\n"), maxInformative)
}

// sandboxBlock is the sandbox's own words as an alert shows them: under
// menubar.SandboxLabel (added when the core's text does not start with
// it), cleaned line by line, within menubar.MaxSandboxSays; "" when the
// sandbox said nothing.
func sandboxBlock(s string) string {
	s = strings.TrimSpace(cleanLines(s))
	if s == "" || s == menubar.SandboxLabel {
		return ""
	}
	if !strings.HasPrefix(s, menubar.SandboxLabel) {
		s = menubar.SandboxLabel + "\n" + s
	}
	return clipRunes(s, menubar.MaxSandboxSays)
}

// cleanLines is s with each line made printable (menubar.Clean) and its
// line breaks kept: "\r\n" and "\r" become "\n".
func cleanLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(menubar.Clean(l), " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// clipRunes cuts s to at most max characters, ending in "…" when cut.
func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max < 1 {
		return ""
	}
	rs := []rune(s)
	return string(rs[:max-1]) + "…"
}

// cText is s as a C string's contents: NUL, which would end it early, is
// '?'. purego adds the terminating NUL.
func cText(s string) string {
	return strings.ReplaceAll(s, "\x00", "?")
}
