//go:build darwin

package macui

import (
	"io"
	"unsafe"
)

// SelfTest loads the frameworks and the glue's classes, holds every class
// and selector of the table to the running system (each class exists,
// each method answers, and its type encoding is the one the glue calls
// it with), and writes what it found to w. It shows nothing.
func SelfTest(w io.Writer) error {
	if err := load(); err != nil {
		return err
	}
	var err error
	withPool(func() { err = check(runtimeLookup{}, w) })
	return err
}

// runtimeLookup answers the self-check from the Objective-C runtime. It
// alone (with objc_darwin.go) names classes and selectors that are not
// literals: the table's.
type runtimeLookup struct{}

func (runtimeLookup) HasClass(name string) bool { return rt.getClass(name) != 0 }

// target is the class to ask: the class itself, or its metaclass for a
// class method.
func target(name string, meta bool) uintptr {
	c := rt.getClass(name)
	if c != 0 && meta {
		c = rt.objectGetClass(c)
	}
	return c
}

func (runtimeLookup) Responds(name, s string, meta bool) bool {
	c := target(name, meta)
	return c != 0 && rt.respondsToSel(c, rt.selRegisterName(s))
}

func (runtimeLookup) Encoding(name, s string, meta bool) (string, bool) {
	c := rt.getClass(name)
	if c == 0 {
		return "", false
	}
	var m uintptr
	if meta {
		m = rt.classMethod(c, rt.selRegisterName(s))
	} else {
		m = rt.instanceMethod(c, rt.selRegisterName(s))
	}
	if m == 0 {
		return "", false
	}
	return rt.methodTypes(m), true
}

func (runtimeLookup) ProtocolEncoding(proto, s string) (string, bool) {
	p := rt.getProtocol(proto)
	if p == 0 {
		return "", false
	}
	want := rt.selRegisterName(s)
	for _, required := range []bool{true, false} {
		var n uint32
		list := rt.protoMethods(p, required, true, &n)
		if list == nil {
			continue
		}
		descs := unsafe.Slice(list, n)
		for _, d := range descs {
			if d.Name == want {
				types := cString(d.Types)
				rt.free(unsafe.Pointer(list))
				return types, true
			}
		}
		rt.free(unsafe.Pointer(list))
	}
	return "", false
}
