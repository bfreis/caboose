//go:build darwin

package macui

import "github.com/bfreis/caboose/internal/menubar"

// Alerts. One is shown at a time, the rest wait (alertQueue). An alert is
// never run from inside a function the main dispatch queue runs: the run
// loop does not serve the main queue while one of its functions runs, so
// a modal alert started there would hold every later hop (Dismiss, the
// action button's timer, menu updates) until it closed. showNext runs
// instead from a run loop timer (performSelector:withObject:afterDelay:),
// in the default mode, so it never starts while a menu is tracked or
// another alert is modal.
//
// The safety rules, for every alert that approves or ends something:
// Cancel is the first button, keeps Return and is the window's initial
// first responder; the action is second, has no key equivalent, and is
// enabled only enableDelaySeconds after the alert appears; only the
// second button's result approves (approved).

// enableDelaySeconds keeps a click or a key meant for something else, as
// the alert pops up, from pressing the action.
const enableDelaySeconds = 1

// Confirm shows a confirmation; its answer comes through Events.Confirmed.
func (*App) Confirm(c menubar.Confirmation) {
	queueAlert(confirmReq(c))
}

// AskURL asks whether to open a URL; the answer comes through
// Events.URLAnswered.
func (*App) AskURL(d menubar.URLDialog) {
	queueAlert(urlReq(d))
}

// Alert shows a one-off informational alert, with only OK; its closing
// comes through Events.AlertClosed.
func (*App) Alert(id, text string) {
	queueAlert(infoReq(id, text))
}

// Dismiss closes the alert with this id, shown or waiting, and posts no
// answer for it.
func (*App) Dismiss(id string) {
	onApp(func() {
		if ui.alerts.dismiss(id) {
			ui.app.send(sel("abortModal"))
		}
	})
}

func queueAlert(r *alertReq) {
	onApp(func() {
		ui.alerts.push(r)
		scheduleNext()
	})
}

// scheduleNext has showNext run from the run loop, soon.
func scheduleNext() {
	ui.alertTarget.send(sel("performSelector:withObject:afterDelay:"), sel("showNext:"), obj(0), float64(0))
}

// showNext shows the next alert, when none is shown, and waits for it.
func showNext() {
	r := ui.alerts.next()
	if r == nil {
		return
	}
	withPool(func() {
		code := runAlert(r)
		ui.alerts.done(code, ui.events.get())
	})
	if ui.alerts.quitting {
		stopApp()
		return
	}
	scheduleNext()
}

// runAlert builds the alert, runs it modally and returns its result.
func runAlert(r *alertReq) int {
	alert := class("NSAlert").send(sel("new"))
	if alert == 0 {
		return responseAbort
	}
	defer alert.send(sel("release"))
	alert.send(sel("setAlertStyle:"), uint(alertStyleWarning))
	alert.send(sel("setMessageText:"), nsstring(r.text.Message))
	alert.send(sel("setInformativeText:"), nsstring(r.text.Informative))
	if r.kind == alertInfo {
		alert.send(sel("addButtonWithTitle:"), nsstring(okTitle))
	} else {
		cancel := alert.send(sel("addButtonWithTitle:"), nsstring(cancelTitle))
		action := alert.send(sel("addButtonWithTitle:"), nsstring(r.text.Action))
		if cancel == 0 || action == 0 {
			return responseAbort
		}
		// Return is Cancel's. Escape: NSAlert gives it to a button titled
		// "Cancel", but a button has one key equivalent and the first
		// keeps Return; Escape is then left to the alert's own handling
		// of a "Cancel" button (to be confirmed on a Mac). Whatever
		// AppKit does, the action takes neither key: its key equivalent
		// is cleared again after the layout.
		cancel.send(sel("setKeyEquivalent:"), nsstring("\r"))
		action.send(sel("setKeyEquivalent:"), nsstring(""))
		action.send(sel("setEnabled:"), false)
		alert.send(sel("layout"))
		action.send(sel("setKeyEquivalent:"), nsstring(""))
		alert.send(sel("window")).send(sel("setInitialFirstResponder:"), cancel)
		gen := r.gen
		afterOnMain(enableDelaySeconds, func() {
			ui.alerts.enableIfShowing(gen, func() { action.send(sel("setEnabled:"), true) })
		})
	}
	// A menu-bar app is never frontmost: without this the alert opens
	// behind the frontmost application's windows.
	ui.app.send(sel("activateIgnoringOtherApps:"), true)
	return alert.sendInt(sel("runModal"))
}
