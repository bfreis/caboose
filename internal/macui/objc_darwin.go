//go:build darwin

package macui

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The Objective-C runtime and the frameworks, loaded on first use (load)
// and never at link time: the launcher links this package into every
// command. Written against libobjc's C functions directly, not purego's
// objc package, whose init loads the runtime in every process that links
// it.

const (
	libObjCPath    = "/usr/lib/libobjc.A.dylib"
	libSystemPath  = "/usr/lib/libSystem.B.dylib"
	foundationPath = "/System/Library/Frameworks/Foundation.framework/Foundation"
	appKitPath     = "/System/Library/Frameworks/AppKit.framework/AppKit"

	userNotificationsPath = "/System/Library/Frameworks/UserNotifications.framework/UserNotifications"
	serviceManagementPath = "/System/Library/Frameworks/ServiceManagement.framework/ServiceManagement"
)

// frameworks are dlopened, in order, by load.
var frameworks = []string{foundationPath, appKitPath, userNotificationsPath, serviceManagementPath}

// obj is an Objective-C object pointer (id), 0 for nil.
type obj uintptr

// selector is a selector (SEL).
type selector uintptr

// nsPoint is NSPoint (CGPoint), passed by value.
type nsPoint struct{ X, Y float64 }

// nsSize is NSSize (CGSize), passed by value.
type nsSize struct{ W, H float64 }

var rt struct {
	once sync.Once
	err  error

	msgSend uintptr
	// The typed forms of objc_msgSend: the same function, called with the
	// arguments each method takes.
	send       func(o obj, s selector, args ...any) obj
	sendBool   func(o obj, s selector, args ...any) bool
	sendInt    func(o obj, s selector, args ...any) int
	sendString func(o obj, s selector, args ...any) string
	sendEvent  func(o obj, s selector, typ uint, loc nsPoint, flags uint, ts float64, window int, ctx obj, subtype int16, d1, d2 int) obj
	sendSize   func(o obj, s selector, size nsSize)

	getClass          func(name string) uintptr
	selRegisterName   func(name string) selector
	allocClassPair    func(super uintptr, name string, extra uintptr) uintptr
	registerClassPair func(cls uintptr)
	addMethod         func(cls uintptr, s selector, imp uintptr, types string) bool
	addProtocol       func(cls, proto uintptr) bool
	getProtocol       func(name string) uintptr
	respondsToSel     func(cls uintptr, s selector) bool
	objectGetClass    func(o uintptr) uintptr
	instanceMethod    func(cls uintptr, s selector) uintptr
	classMethod       func(cls uintptr, s selector) uintptr
	methodTypes       func(m uintptr) string
	protoMethods      func(p uintptr, required, instance bool, count *uint32) *methodDescription
	poolPush          func() uintptr
	poolPop           func(pool uintptr)

	free           func(p unsafe.Pointer)
	pthreadMainNP  func() int32
	dispatchAsyncF func(queue, ctx, fn uintptr)
	dispatchAfterF func(when uint64, queue, ctx, fn uintptr)
	dispatchTime   func(when uint64, delta int64) uint64
	mainQueue      uintptr

	// The C callbacks: the main queue's, and each registered method's.
	dispatchCallback uintptr
}

// methodDescription is objc_method_description.
type methodDescription struct {
	Name  selector
	Types *byte
}

// load loads the runtime and the frameworks and registers the glue's
// classes, once.
func load() error {
	rt.once.Do(func() { rt.err = doLoad() })
	return rt.err
}

func doLoad() error {
	objc, err := purego.Dlopen(libObjCPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load the Objective-C runtime: %w", err)
	}
	sys, err := purego.Dlopen(libSystemPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load libSystem: %w", err)
	}
	for _, f := range frameworks {
		if _, err := purego.Dlopen(f, purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			return fmt.Errorf("load %s: %w", f, err)
		}
	}
	if rt.msgSend, err = purego.Dlsym(objc, "objc_msgSend"); err != nil {
		return err
	}
	if rt.mainQueue, err = purego.Dlsym(sys, "_dispatch_main_q"); err != nil {
		return err
	}
	purego.RegisterFunc(&rt.send, rt.msgSend)
	purego.RegisterFunc(&rt.sendBool, rt.msgSend)
	purego.RegisterFunc(&rt.sendInt, rt.msgSend)
	purego.RegisterFunc(&rt.sendString, rt.msgSend)
	purego.RegisterFunc(&rt.sendEvent, rt.msgSend)
	purego.RegisterFunc(&rt.sendSize, rt.msgSend)
	purego.RegisterLibFunc(&rt.getClass, objc, "objc_getClass")
	purego.RegisterLibFunc(&rt.selRegisterName, objc, "sel_registerName")
	purego.RegisterLibFunc(&rt.allocClassPair, objc, "objc_allocateClassPair")
	purego.RegisterLibFunc(&rt.registerClassPair, objc, "objc_registerClassPair")
	purego.RegisterLibFunc(&rt.addMethod, objc, "class_addMethod")
	purego.RegisterLibFunc(&rt.addProtocol, objc, "class_addProtocol")
	purego.RegisterLibFunc(&rt.getProtocol, objc, "objc_getProtocol")
	purego.RegisterLibFunc(&rt.respondsToSel, objc, "class_respondsToSelector")
	purego.RegisterLibFunc(&rt.objectGetClass, objc, "object_getClass")
	purego.RegisterLibFunc(&rt.instanceMethod, objc, "class_getInstanceMethod")
	purego.RegisterLibFunc(&rt.classMethod, objc, "class_getClassMethod")
	purego.RegisterLibFunc(&rt.methodTypes, objc, "method_getTypeEncoding")
	purego.RegisterLibFunc(&rt.protoMethods, objc, "protocol_copyMethodDescriptionList")
	purego.RegisterLibFunc(&rt.poolPush, objc, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&rt.poolPop, objc, "objc_autoreleasePoolPop")
	purego.RegisterLibFunc(&rt.free, sys, "free")
	purego.RegisterLibFunc(&rt.pthreadMainNP, sys, "pthread_main_np")
	purego.RegisterLibFunc(&rt.dispatchAsyncF, sys, "dispatch_async_f")
	purego.RegisterLibFunc(&rt.dispatchAfterF, sys, "dispatch_after_f")
	purego.RegisterLibFunc(&rt.dispatchTime, sys, "dispatch_time")
	rt.dispatchCallback = purego.NewCallback(runDispatched)
	if class("NSApplication") == 0 || class("NSStatusBar") == 0 || class("NSAlert") == 0 {
		return fmt.Errorf("AppKit did not load")
	}
	return registerClasses()
}

// sel is the selector named name. Its argument is always a literal, so
// that the self-check's table holds every selector (selftest.go).
func sel(name string) selector { return rt.selRegisterName(name) }

// class is the class named name, 0 when there is none; a literal too.
func class(name string) obj { return obj(rt.getClass(name)) }

// method is one method a registered class implements.
type method struct {
	name  selector
	types string
	imp   uintptr
}

// registerClass registers a subclass of NSObject with these methods,
// optionally conforming to a protocol.
func registerClass(name, protocol string, methods []method) (obj, error) {
	if c := rt.getClass(name); c != 0 {
		return obj(c), nil // registered already
	}
	c := rt.allocClassPair(uintptr(class("NSObject")), name, 0)
	if c == 0 {
		return 0, fmt.Errorf("register class %s", name)
	}
	for _, m := range methods {
		if !rt.addMethod(c, m.name, m.imp, m.types) {
			return 0, fmt.Errorf("add a method to %s", name)
		}
	}
	if protocol != "" {
		if p := rt.getProtocol(protocol); p != 0 {
			rt.addProtocol(c, p)
		}
	}
	rt.registerClassPair(c)
	return obj(c), nil
}

// send and its typed forms call a method.
func (o obj) send(s selector, args ...any) obj          { return rt.send(o, s, args...) }
func (o obj) sendBool(s selector, args ...any) bool     { return rt.sendBool(o, s, args...) }
func (o obj) sendInt(s selector, args ...any) int       { return rt.sendInt(o, s, args...) }
func (o obj) sendString(s selector, args ...any) string { return rt.sendString(o, s, args...) }

// nsstring is an autoreleased NSString holding s; an autorelease pool
// must be in place.
func nsstring(s string) obj {
	return class("NSString").send(sel("stringWithUTF8String:"), cText(s))
}

// nsdata is an autoreleased NSData holding a copy of b; an autorelease
// pool must be in place.
func nsdata(b []byte) obj {
	d := class("NSData").send(sel("dataWithBytes:length:"), unsafe.Pointer(unsafe.SliceData(b)), uint(len(b)))
	runtime.KeepAlive(b)
	return d
}

// gostring is an NSString's contents, "" for nil.
func gostring(o obj) string {
	if o == 0 {
		return ""
	}
	return o.sendString(sel("UTF8String"))
}

// withPool runs fn inside an autorelease pool.
func withPool(fn func()) {
	p := rt.poolPush()
	defer rt.poolPop(p)
	fn()
}

// cString is the NUL-terminated string at p, "" for nil.
func cString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}
