package vnodes

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// libproc's, from <sys/proc_info.h> and <libproc.h>.
const (
	procUIDOnly            = 4 // proc_listpids: the processes of a uid
	procPIDListFDs         = 1 // proc_pidinfo: struct proc_fdinfo[]
	procPIDFDVnodePathInfo = 2 // proc_pidfdinfo: struct vnode_fdinfowithpath
	proxFDTypeVnode        = 1

	// fdInfoSize is sizeof(struct proc_fdinfo): int32 fd, uint32 type.
	fdInfoSize = 8
	// vnodePathInfoSize is sizeof(struct vnode_fdinfowithpath): a
	// proc_fileinfo (24 bytes), a vnode_info (152) and the path
	// (MAXPATHLEN, 1024), which is why the path is at 176.
	vnodePathInfoSize = 1200
	vnodePathOffset   = 176
	// pidPathMax is PROC_PIDPATHINFO_MAXSIZE.
	pidPathMax = 4096
)

var libproc struct {
	once sync.Once
	err  error

	proc_listpids  func(typ, typeinfo uint32, buf unsafe.Pointer, size int32) int32
	proc_pidpath   func(pid int32, buf unsafe.Pointer, size uint32) int32
	proc_pidinfo   func(pid, flavor int32, arg uint64, buf unsafe.Pointer, size int32) int32
	proc_pidfdinfo func(pid, fd, flavor int32, buf unsafe.Pointer, size int32) int32
}

func load() error {
	l := &libproc
	l.once.Do(func() {
		sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			l.err = err
			return
		}
		purego.RegisterLibFunc(&l.proc_listpids, sys, "proc_listpids")
		purego.RegisterLibFunc(&l.proc_pidpath, sys, "proc_pidpath")
		purego.RegisterLibFunc(&l.proc_pidinfo, sys, "proc_pidinfo")
		purego.RegisterLibFunc(&l.proc_pidfdinfo, sys, "proc_pidfdinfo")
	})
	return l.err
}

// Host is this Mac.
func Host() (System, error) {
	if err := load(); err != nil {
		return nil, fmt.Errorf("cannot load libproc: %v", err)
	}
	return host{uid: uint32(os.Getuid())}, nil
}

type host struct{ uid uint32 }

func (h host) Processes() ([]int, error) {
	n := libproc.proc_listpids(procUIDOnly, h.uid, nil, 0)
	if n <= 0 {
		return nil, fmt.Errorf("proc_listpids returned %d", n)
	}
	// The list can grow between the two calls: room for more, and again
	// whenever it comes back full.
	for size := int(n) + 256*4; ; size *= 2 {
		buf := make([]int32, size/4)
		got := libproc.proc_listpids(procUIDOnly, h.uid, unsafe.Pointer(&buf[0]), int32(len(buf)*4))
		runtime.KeepAlive(buf)
		if got <= 0 {
			return nil, fmt.Errorf("proc_listpids returned %d", got)
		}
		if int(got) >= len(buf)*4 && size < 1<<24 {
			continue
		}
		var pids []int
		for _, pid := range buf[:int(got)/4] {
			if pid > 0 {
				pids = append(pids, int(pid))
			}
		}
		return pids, nil
	}
}

func (host) Path(pid int) (string, error) {
	buf := make([]byte, pidPathMax)
	n := libproc.proc_pidpath(int32(pid), unsafe.Pointer(&buf[0]), uint32(len(buf)))
	runtime.KeepAlive(buf)
	if n <= 0 {
		return "", fmt.Errorf("no process %d", pid)
	}
	return string(buf[:n]), nil
}

func (host) VnodeFDs(pid int) ([]int32, error) {
	// Without a buffer it says how much the descriptor table could need,
	// which is at least what is open.
	n := libproc.proc_pidinfo(int32(pid), procPIDListFDs, 0, nil, 0)
	if n <= 0 {
		return nil, fmt.Errorf("cannot list the open files of process %d", pid)
	}
	for size := int(n) + int(n)/8 + 64*fdInfoSize; ; size *= 2 {
		buf := make([]byte, size)
		got := libproc.proc_pidinfo(int32(pid), procPIDListFDs, 0, unsafe.Pointer(&buf[0]), int32(len(buf)))
		runtime.KeepAlive(buf)
		if got <= 0 {
			return nil, fmt.Errorf("cannot list the open files of process %d", pid)
		}
		if int(got) >= len(buf) && size < 1<<28 {
			continue
		}
		var fds []int32
		for i := 0; i+fdInfoSize <= int(got); i += fdInfoSize {
			if binary.LittleEndian.Uint32(buf[i+4:]) == proxFDTypeVnode {
				fds = append(fds, int32(binary.LittleEndian.Uint32(buf[i:])))
			}
		}
		return fds, nil
	}
}

func (host) FDPath(pid int, fd int32) (string, error) {
	buf := make([]byte, vnodePathInfoSize)
	n := libproc.proc_pidfdinfo(int32(pid), fd, procPIDFDVnodePathInfo, unsafe.Pointer(&buf[0]), int32(len(buf)))
	runtime.KeepAlive(buf)
	if n < vnodePathInfoSize {
		return "", fmt.Errorf("cannot read the path of descriptor %d of process %d", fd, pid)
	}
	p := buf[vnodePathOffset:]
	if i := bytes.IndexByte(p, 0); i >= 0 {
		p = p[:i]
	}
	return string(p), nil
}

func (host) MaxVnodes() (int, error) {
	n, err := unix.SysctlUint32("kern.maxvnodes")
	return int(n), err
}
