//go:build darwin

package macui

// The main-queue hop: everything AppKit is touched on the main thread,
// and the core calls the glue from its own goroutine. A function is
// queued on the main dispatch queue with dispatch_async_f, whose single C
// callback finds it by the number it is handed.

var dispatched registry

// onMain runs fn on the main thread, later, inside an autorelease pool.
func onMain(fn func()) {
	ctx := dispatched.put(fn)
	rt.dispatchAsyncF(rt.mainQueue, ctx, rt.dispatchCallback)
}

// afterOnMain runs fn on the main thread after seconds.
func afterOnMain(seconds int64, fn func()) {
	ctx := dispatched.put(fn)
	const dispatchTimeNow = 0
	when := rt.dispatchTime(dispatchTimeNow, seconds*1e9)
	rt.dispatchAfterF(when, rt.mainQueue, ctx, rt.dispatchCallback)
}

// runDispatched is the dispatch queue's C callback.
func runDispatched(ctx uintptr) {
	if fn := dispatched.take(ctx); fn != nil {
		withPool(fn)
	}
}

// onMainThread reports whether the calling thread is the process's main
// thread.
func onMainThread() bool { return rt.pthreadMainNP() == 1 }
