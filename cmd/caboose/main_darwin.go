package main

import "runtime"

// AppKit runs on the process's main thread only: keep main, and with it
// caboose app's run loop, on it. Harmless for every other command, and
// done for all of them, so nothing depends on reading the command line
// this early.
func init() { runtime.LockOSThread() }
