// Package macui is the menu-bar app's glue to AppKit on macOS: it
// implements menubar.UI by drawing what the core says (the status item,
// its menu, alerts) and posting back, through menubar.Events, what the
// user did. It decides nothing about the daemon's data: every decision is
// the core's (internal/menubar).
//
// AppKit is reached through purego, without cgo, by the Objective-C
// runtime's C functions, which this package loads itself: nothing happens
// when the package is linked (no init), and the frameworks are loaded the
// first time Run or SelfTest needs them. A selector or class name is
// checked only at run time, so every one the glue uses is a literal in
// one table (selftest.go), which SelfTest checks against the running
// system and a test holds to the code.
//
// What can be decided without AppKit lives in files with no build tag
// and is tested on any platform: the alert's text and the meaning of its
// result (approved), the menu's flattening, the alert queue, the
// callbacks' registry and the self-check's comparison.
package macui

import "errors"

// ErrMacOnly is what the app returns on any system but macOS.
var ErrMacOnly = errors.New("the menu-bar app runs on macOS only")
