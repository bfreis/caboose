package fswatch

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// FSEvents, from CoreServices, called through purego: the launcher is
// built without cgo. A stream watches whole trees, and the kernel keeps
// its events, so this costs the same on any size of tree.

const (
	coreServicesPath   = "/System/Library/Frameworks/CoreServices.framework/CoreServices"
	coreFoundationPath = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	libSystemPath      = "/usr/lib/libSystem.B.dylib"

	kCFStringEncodingUTF8 = 0x08000100
	eventIDSinceNow       = ^uint64(0)
	// latency is how long FSEvents gathers events before it calls back.
	latency = 0.1

	flagNoDefer    = 0x2
	flagWatchRoot  = 0x4
	flagFileEvents = 0x10

	eventMustScanSubDirs = 0x1
	eventUserDropped     = 0x2
	eventKernelDropped   = 0x4
	eventHistoryDone     = 0x10
	eventRootChanged     = 0x20
)

var fsevents struct {
	once sync.Once
	err  error

	typeArrayCallBacks uintptr
	callback           uintptr

	CFStringCreateWithCString     func(alloc uintptr, s string, enc uint32) uintptr
	CFArrayCreate                 func(alloc uintptr, values unsafe.Pointer, n int, callbacks uintptr) uintptr
	CFRelease                     func(ref uintptr)
	FSEventStreamCreate           func(alloc, callback uintptr, ctx unsafe.Pointer, paths uintptr, since uint64, latency float64, flags uint32) uintptr
	FSEventStreamSetDispatchQueue func(stream, queue uintptr)
	FSEventStreamStart            func(stream uintptr) bool
	FSEventStreamStop             func(stream uintptr)
	FSEventStreamInvalidate       func(stream uintptr)
	FSEventStreamRelease          func(stream uintptr)
	dispatch_queue_create         func(label string, attr uintptr) uintptr
	dispatch_release              func(obj uintptr)
}

// streams are the running watchers, by the id their stream's context
// carries: the one callback serves them all, since purego never frees one.
var (
	streams  sync.Map // uintptr -> *Watcher
	streamID atomic.Uintptr
)

// streamContext is FSEventStreamContext.
type streamContext struct {
	version         int
	info            uintptr
	retain          uintptr
	release         uintptr
	copyDescription uintptr
}

func load() error {
	f := &fsevents
	f.once.Do(func() {
		cs, err := purego.Dlopen(coreServicesPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			f.err = err
			return
		}
		cf, err := purego.Dlopen(coreFoundationPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			f.err = err
			return
		}
		sys, err := purego.Dlopen(libSystemPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			f.err = err
			return
		}
		if f.typeArrayCallBacks, err = purego.Dlsym(cf, "kCFTypeArrayCallBacks"); err != nil {
			f.err = err
			return
		}
		purego.RegisterLibFunc(&f.CFStringCreateWithCString, cf, "CFStringCreateWithCString")
		purego.RegisterLibFunc(&f.CFArrayCreate, cf, "CFArrayCreate")
		purego.RegisterLibFunc(&f.CFRelease, cf, "CFRelease")
		purego.RegisterLibFunc(&f.FSEventStreamCreate, cs, "FSEventStreamCreate")
		purego.RegisterLibFunc(&f.FSEventStreamSetDispatchQueue, cs, "FSEventStreamSetDispatchQueue")
		purego.RegisterLibFunc(&f.FSEventStreamStart, cs, "FSEventStreamStart")
		purego.RegisterLibFunc(&f.FSEventStreamStop, cs, "FSEventStreamStop")
		purego.RegisterLibFunc(&f.FSEventStreamInvalidate, cs, "FSEventStreamInvalidate")
		purego.RegisterLibFunc(&f.FSEventStreamRelease, cs, "FSEventStreamRelease")
		purego.RegisterLibFunc(&f.dispatch_queue_create, sys, "dispatch_queue_create")
		purego.RegisterLibFunc(&f.dispatch_release, sys, "dispatch_release")
		f.callback = purego.NewCallback(streamCallback)
	})
	return f.err
}

// streamCallback is FSEventStreamCallback, with char** paths (no
// kFSEventStreamCreateFlagUseCFTypes). It runs on the stream's dispatch
// queue, and copies what it keeps before returning.
func streamCallback(stream, info, n uintptr, paths, flags, ids unsafe.Pointer) {
	v, ok := streams.Load(info)
	if !ok || n == 0 {
		return
	}
	w := v.(*Watcher)
	ps := unsafe.Slice((**byte)(paths), n)
	fl := unsafe.Slice((*uint32)(flags), n)
	for i := range ps {
		switch f := fl[i]; {
		case f&(eventUserDropped|eventKernelDropped|eventRootChanged) != 0:
			w.lost()
		case f&eventHistoryDone != 0:
		default:
			// eventMustScanSubDirs names a directory whose events
			// were coalesced: reporting it says to look again under it.
			w.add(goString(ps[i]))
		}
	}
}

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func start(w *Watcher) (func(), error) {
	if err := load(); err != nil {
		return nil, err
	}
	f := &fsevents
	var strs []uintptr
	defer func() {
		for _, s := range strs {
			f.CFRelease(s)
		}
	}()
	for _, r := range w.roots {
		s := f.CFStringCreateWithCString(0, r.phys, kCFStringEncodingUTF8)
		if s == 0 {
			return nil, errors.New("fswatch: cannot make a CFString of " + r.phys)
		}
		strs = append(strs, s)
	}
	arr := f.CFArrayCreate(0, unsafe.Pointer(&strs[0]), len(strs), f.typeArrayCallBacks)
	runtime.KeepAlive(strs)
	if arr == 0 {
		return nil, errors.New("fswatch: cannot make the CFArray of paths")
	}
	defer f.CFRelease(arr)

	id := streamID.Add(1)
	streams.Store(id, w)
	ctx := &streamContext{info: id}
	stream := f.FSEventStreamCreate(0, f.callback, unsafe.Pointer(ctx), arr, eventIDSinceNow, latency,
		flagNoDefer|flagWatchRoot|flagFileEvents)
	runtime.KeepAlive(ctx)
	if stream == 0 {
		streams.Delete(id)
		return nil, errors.New("fswatch: FSEventStreamCreate failed")
	}
	queue := f.dispatch_queue_create("caboose.fswatch", 0)
	f.FSEventStreamSetDispatchQueue(stream, queue)
	if !f.FSEventStreamStart(stream) {
		f.FSEventStreamInvalidate(stream)
		f.FSEventStreamRelease(stream)
		f.dispatch_release(queue)
		streams.Delete(id)
		return nil, errors.New("fswatch: FSEventStreamStart failed")
	}
	return func() {
		streams.Delete(id)
		f.FSEventStreamStop(stream)
		f.FSEventStreamInvalidate(stream)
		f.FSEventStreamRelease(stream)
		f.dispatch_release(queue)
	}, nil
}
