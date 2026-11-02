package diskusage

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// From <sys/attr.h>, for the request.
const (
	// attrCmnReturnedAttrs asks for the attribute_set_t of what was
	// returned, so a filesystem that has no private size says so.
	attrCmnReturnedAttrs = 0x80000000
	fsoptNoFollow        = 0x1
	fsoptAttrCmnExtended = 0x20
)

// attrList is struct attrlist.
type attrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

// libc holds getattrlistat's address, called through purego.SyscallN:
// on darwin its trampoline reads errno right after the call, on the same
// thread, before anything else can change it.
var libc struct {
	once sync.Once
	err  error

	getattrlistat uintptr
}

func loadLibc() error {
	l := &libc
	l.once.Do(func() {
		sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			l.err = err
			return
		}
		l.getattrlistat, l.err = purego.Dlsym(sys, "getattrlistat")
	})
	return l.err
}

// apfsUnshared is getattrlistat's ATTR_CMNEXT_PRIVATESIZE, which APFS
// reports and most other filesystems do not.
type apfsUnshared struct{}

// DefaultUnshared is this system's UnsharedSizer: APFS's private size on
// macOS, nil elsewhere.
func DefaultUnshared() UnsharedSizer {
	if loadLibc() != nil {
		return nil
	}
	return apfsUnshared{}
}

// UnsharedMethod says how DefaultUnshared measures, for a report.
const UnsharedMethod = "APFS's private size (getattrlist ATTR_CMNEXT_PRIVATESIZE)"

func (apfsUnshared) UnsharedSize(dirfd int, name string) (int64, error) {
	p, err := unix.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	al := &attrList{
		bitmapCount: attrBitMapCount,
		commonAttr:  attrCmnReturnedAttrs,
		forkAttr:    attrCmnExtPrivateSize,
	}
	buf := new([replySize + 8]byte)
	// SyscallN takes the pointers as integers, which the garbage
	// collector does not follow: pinned, the three stay where they are
	// (on the heap, which pinning puts them on) until the call is done.
	var pin runtime.Pinner
	pin.Pin(p)
	pin.Pin(al)
	pin.Pin(buf)
	r, _, errno := purego.SyscallN(libc.getattrlistat, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(al)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), fsoptNoFollow|fsoptAttrCmnExtended)
	pin.Unpin()
	// getattrlistat returns an int, -1 on failure, and errno is an int
	// too: the trampoline reads each as 64 bits, of which only the low 32
	// are the value.
	if int32(r) != 0 {
		return 0, callError("getattrlistat", name, syscall.Errno(uint32(errno)))
	}
	return parseReply(buf[:])
}
