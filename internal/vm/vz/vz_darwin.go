// Package vz runs a caboose VM on macOS's Virtualization.framework, from
// Go through purego's objc package, with no cgo: caboose-vmm's Runner on
// a Mac. The framework needs the com.apple.security.virtualization
// entitlement on the binary, which is why this lives in caboose-vmm and
// never in the launcher.
//
// A VZVirtualMachine belongs to the dispatch queue it was made with: every
// call to it goes through onQueue, and its handlers and delegate run on
// that queue's threads, never the main thread (spike 1), so no run loop is
// needed.
package vz

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/bfreis/caboose/internal/vm"
)

// How long the framework has to answer a start, a vsock connect and a
// stop.
const (
	startTimeout   = 60 * time.Second
	connectTimeout = 10 * time.Second
	stopTimeout    = 10 * time.Second
)

var (
	dispatchQueueCreate func(label string, attr uintptr) uintptr
	dispatchSyncF       func(queue, ctx, fn uintptr)

	loadOnce sync.Once
	loadErr  error

	delegateClass objc.Class
	// active is the one VM of this process, for the delegate to reach.
	active atomic.Pointer[Runner]
)

// required are the classes a VM of caboose's needs; each is in macOS 12.
var required = []string{
	"VZVirtualMachine", "VZVirtualMachineConfiguration", "VZLinuxBootLoader",
	"VZFileSerialPortAttachment", "VZVirtioConsoleDeviceSerialPortConfiguration",
	"VZDiskImageStorageDeviceAttachment", "VZVirtioBlockDeviceConfiguration",
	"VZVirtioNetworkDeviceConfiguration", "VZNATNetworkDeviceAttachment", "VZMACAddress",
	"VZSharedDirectory", "VZMultipleDirectoryShare", "VZVirtioFileSystemDeviceConfiguration",
	"VZVirtioSocketDeviceConfiguration", "VZVirtioEntropyDeviceConfiguration",
	"VZVirtioTraditionalMemoryBalloonDeviceConfiguration",
}

func load() error {
	loadOnce.Do(func() { loadErr = doLoad() })
	return loadErr
}

func doLoad() error {
	for _, lib := range []string{
		"/usr/lib/libSystem.B.dylib",
		"/System/Library/Frameworks/Foundation.framework/Foundation",
		"/System/Library/Frameworks/Virtualization.framework/Virtualization",
	} {
		if _, err := purego.Dlopen(lib, purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			return fmt.Errorf("loading %s: %v", lib, err)
		}
	}
	sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&dispatchQueueCreate, sys, "dispatch_queue_create")
	purego.RegisterLibFunc(&dispatchSyncF, sys, "dispatch_sync_f")
	syncCall = purego.NewCallback(runSync)
	for _, c := range required {
		if objc.GetClass(c) == 0 {
			return fmt.Errorf("this macOS has no %s: vm needs macOS 13 or later", c)
		}
	}
	// The VZVirtualMachineDelegate protocol is not found at runtime
	// (spike 1), and the delegate works without it.
	var protos []*objc.Protocol
	if p := objc.GetProtocol("VZVirtualMachineDelegate"); p != nil {
		protos = append(protos, p)
	}
	delegateClass, err = objc.RegisterClass("CabooseVMMDelegate", objc.GetClass("NSObject"), protos, nil,
		[]objc.MethodDef{
			{Cmd: sel("guestDidStopVirtualMachine:"), Fn: func(_ objc.ID, _ objc.SEL, _ objc.ID) {
				if r := active.Load(); r != nil {
					r.end(nil)
				}
			}},
			{Cmd: sel("virtualMachine:didStopWithError:"), Fn: func(_ objc.ID, _ objc.SEL, _, err objc.ID) {
				if r := active.Load(); r != nil {
					r.end(fmt.Errorf("the VM stopped: %s", describe(err)))
				}
			}},
		})
	if err != nil {
		return fmt.Errorf("registering the VM's delegate: %v", err)
	}
	return nil
}

// onQueue runs fn on queue and waits for it, through dispatch_sync_f and
// one purego callback that finds fn by the context it is given.
var (
	syncFns  sync.Map
	syncID   atomic.Uintptr
	syncCall uintptr
)

func onQueue(queue uintptr, fn func()) {
	id := syncID.Add(1)
	syncFns.Store(id, fn)
	defer syncFns.Delete(id)
	dispatchSyncF(queue, id, syncCall)
}

func runSync(ctx uintptr) {
	if fn, ok := syncFns.Load(ctx); ok {
		fn.(func())()
	}
}

func sel(name string) objc.SEL { return objc.RegisterName(name) }

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func nsstring(s string) objc.ID {
	return class("NSString").Send(sel("stringWithUTF8String:"), s)
}

func gostring(id objc.ID) string {
	if id == 0 {
		return ""
	}
	return objc.Send[string](id, sel("UTF8String"))
}

// describe is an NSError in words: its description, domain and code.
func describe(err objc.ID) string {
	if err == 0 {
		return "no error given"
	}
	return fmt.Sprintf("%s (%s %d)", gostring(err.Send(sel("localizedDescription"))),
		gostring(err.Send(sel("domain"))), objc.Send[int](err, sel("code")))
}

func fileURL(path string) objc.ID {
	return class("NSURL").Send(sel("fileURLWithPath:"), nsstring(path))
}

func array(ids ...objc.ID) objc.ID {
	a := class("NSMutableArray").Send(sel("array"))
	for _, id := range ids {
		a.Send(sel("addObject:"), id)
	}
	return a
}

// ErrEntitlement is a caboose-vmm without the virtualization entitlement:
// an unsigned build, or one whose signature was lost.
var ErrEntitlement = errors.New("caboose-vmm is not signed with the virtualization entitlement (com.apple.security.virtualization): reinstall caboose, or in a checkout run 'make vmm' on this Mac, which signs it")

// Runner is one VM on Virtualization.framework.
type Runner struct {
	queue uintptr
	vm    objc.ID

	mu      sync.Mutex
	done    chan struct{}
	stopped bool
	err     error
}

// New is a Runner, once the framework is loaded and says this Mac can run
// a VM.
func New() (*Runner, error) {
	if err := load(); err != nil {
		return nil, err
	}
	if !objc.Send[bool](class("VZVirtualMachine"), sel("isSupported")) {
		return nil, errors.New("this Mac cannot run VMs: Virtualization.framework says it is not supported here")
	}
	return &Runner{done: make(chan struct{})}, nil
}

func (r *Runner) end(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stopped {
		r.stopped, r.err = true, err
		close(r.done)
	}
}

// Done is closed once the VM has stopped.
func (r *Runner) Done() <-chan struct{} { return r.done }

// Err is why the VM stopped: nil when the guest powered off.
func (r *Runner) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Start boots m.
func (r *Runner) Start(m vm.Machine) error {
	if !active.CompareAndSwap(nil, r) {
		return errors.New("this process already runs a VM")
	}
	pool := class("NSAutoreleasePool").Send(sel("new"))
	defer pool.Send(sel("drain"))

	cfg, err := configure(m)
	if err != nil {
		return err
	}
	r.queue = dispatchQueueCreate("caboose.vmm", 0)
	delegate := objc.ID(delegateClass).Send(sel("new"))
	onQueue(r.queue, func() {
		r.vm = class("VZVirtualMachine").Send(sel("alloc")).Send(sel("initWithConfiguration:queue:"), cfg, r.queue)
		if r.vm != 0 {
			r.vm.Send(sel("setDelegate:"), delegate)
		}
	})
	if r.vm == 0 {
		return errors.New("Virtualization.framework made no VM of a valid configuration")
	}

	started := make(chan error, 1)
	block := objc.NewBlock(func(_ objc.Block, err objc.ID) {
		if err != 0 {
			started <- fmt.Errorf("starting the VM: %s", describe(err))
			return
		}
		started <- nil
	})
	onQueue(r.queue, func() { r.vm.Send(sel("startWithCompletionHandler:"), block) })
	// A block is released only once it was called: on a timeout the
	// framework may still call it.
	select {
	case err := <-started:
		block.Release()
		return err
	case <-time.After(startTimeout):
		return fmt.Errorf("the VM did not start in %v", startTimeout)
	}
}

// configure is m as a validated VZVirtualMachineConfiguration.
func configure(m vm.Machine) (objc.ID, error) {
	for _, p := range []string{m.Kernel, m.Initramfs} {
		if _, err := os.Stat(p); err != nil {
			return 0, err
		}
	}
	cmdline := m.Cmdline
	if cmdline == "" {
		cmdline = vm.DefaultCmdline(m.Network)
	}
	boot := class("VZLinuxBootLoader").Send(sel("alloc")).Send(sel("initWithKernelURL:"), fileURL(m.Kernel))
	boot.Send(sel("setInitialRamdiskURL:"), fileURL(m.Initramfs))
	boot.Send(sel("setCommandLine:"), nsstring(cmdline))

	cfg := class("VZVirtualMachineConfiguration").Send(sel("new"))
	cfg.Send(sel("setBootLoader:"), boot)
	vzc := class("VZVirtualMachineConfiguration")
	cpus := clamp(uint64(m.CPUs), uint64(objc.Send[uint](vzc, sel("minimumAllowedCPUCount"))), uint64(objc.Send[uint](vzc, sel("maximumAllowedCPUCount"))))
	mem := clamp(uint64(m.MemoryMiB)<<20, objc.Send[uint64](vzc, sel("minimumAllowedMemorySize")), objc.Send[uint64](vzc, sel("maximumAllowedMemorySize")))
	cfg.Send(sel("setCPUCount:"), uint(cpus))
	cfg.Send(sel("setMemorySize:"), mem)

	// The console, to a file the host reads as the VM's log.
	if err := os.WriteFile(m.Console, nil, 0o600); err != nil {
		return 0, err
	}
	var nserr objc.ID
	attach := class("VZFileSerialPortAttachment").Send(sel("alloc")).Send(sel("initWithURL:append:error:"),
		fileURL(m.Console), true, unsafe.Pointer(&nserr))
	if attach == 0 {
		return 0, fmt.Errorf("the console %s: %s", m.Console, describe(nserr))
	}
	serial := class("VZVirtioConsoleDeviceSerialPortConfiguration").Send(sel("new"))
	serial.Send(sel("setAttachment:"), attach)
	cfg.Send(sel("setSerialPorts:"), array(serial))

	var disks []objc.ID
	for _, d := range m.Disks {
		nserr = 0
		att := class("VZDiskImageStorageDeviceAttachment").Send(sel("alloc")).Send(sel("initWithURL:readOnly:error:"),
			fileURL(d.Path), d.ReadOnly, unsafe.Pointer(&nserr))
		if att == 0 {
			return 0, fmt.Errorf("the disk %s: %s", d.Path, describe(nserr))
		}
		disks = append(disks, class("VZVirtioBlockDeviceConfiguration").Send(sel("alloc")).Send(sel("initWithAttachment:"), att))
	}
	cfg.Send(sel("setStorageDevices:"), array(disks...))

	switch m.Network {
	case "nat":
		nic := class("VZVirtioNetworkDeviceConfiguration").Send(sel("new"))
		nic.Send(sel("setAttachment:"), class("VZNATNetworkDeviceAttachment").Send(sel("new")))
		// The launcher's MAC, the same at every boot, so bootpd hands the
		// VM its lease again (Dir.Boot always writes one).
		if err := vm.CheckMAC(m.MAC); err != nil {
			return 0, err
		}
		mac := class("VZMACAddress").Send(sel("alloc")).Send(sel("initWithString:"), nsstring(m.MAC))
		if mac == 0 {
			return 0, fmt.Errorf("Virtualization.framework refuses the MAC %s", m.MAC)
		}
		nic.Send(sel("setMACAddress:"), mac)
		cfg.Send(sel("setNetworkDevices:"), array(nic))
	case "none":
	default:
		return 0, fmt.Errorf("unknown network %q (nat or none)", m.Network)
	}

	if len(m.Shares) > 0 {
		dirs := class("NSMutableDictionary").Send(sel("dictionary"))
		for _, s := range m.Shares {
			if _, err := os.Stat(s.Path); err != nil {
				return 0, err
			}
			d := class("VZSharedDirectory").Send(sel("alloc")).Send(sel("initWithURL:readOnly:"), fileURL(s.Path), false)
			dirs.Send(sel("setObject:forKey:"), d, nsstring(s.Name))
		}
		share := class("VZMultipleDirectoryShare").Send(sel("alloc")).Send(sel("initWithDirectories:"), dirs)
		fs := class("VZVirtioFileSystemDeviceConfiguration").Send(sel("alloc")).Send(sel("initWithTag:"), nsstring(vm.ShareTag))
		fs.Send(sel("setShare:"), share)
		cfg.Send(sel("setDirectorySharingDevices:"), array(fs))
	}

	cfg.Send(sel("setSocketDevices:"), array(class("VZVirtioSocketDeviceConfiguration").Send(sel("new"))))
	cfg.Send(sel("setEntropyDevices:"), array(class("VZVirtioEntropyDeviceConfiguration").Send(sel("new"))))
	cfg.Send(sel("setMemoryBalloonDevices:"), array(class("VZVirtioTraditionalMemoryBalloonDeviceConfiguration").Send(sel("new"))))

	nserr = 0
	if !objc.Send[bool](cfg, sel("validateWithError:"), unsafe.Pointer(&nserr)) {
		why := describe(nserr)
		if strings.Contains(why, "com.apple.security.virtualization") {
			return 0, fmt.Errorf("%w (%s)", ErrEntitlement, why)
		}
		return 0, fmt.Errorf("the VM's configuration is not valid: %s", why)
	}
	return cfg, nil
}

func clamp(v, lo, hi uint64) uint64 { return max(lo, min(v, hi)) }

// Connect opens a connection to port in the guest.
func (r *Runner) Connect(port uint32) (io.ReadWriteCloser, error) {
	if r.vm == 0 {
		return nil, errors.New("the VM has not started")
	}
	type dialed struct {
		fd  int
		err error
	}
	res := make(chan dialed, 1)
	block := objc.NewBlock(func(_ objc.Block, c, err objc.ID) {
		if err != 0 {
			res <- dialed{-1, errors.New(describe(err))}
			return
		}
		// The connection closes its descriptor when it is freed.
		fd, derr := syscall.Dup(int(objc.Send[int32](c, sel("fileDescriptor"))))
		res <- dialed{fd, derr}
	})
	ok := false
	onQueue(r.queue, func() {
		devs := r.vm.Send(sel("socketDevices"))
		if objc.Send[uint](devs, sel("count")) == 0 {
			return
		}
		devs.Send(sel("firstObject")).Send(sel("connectToPort:completionHandler:"), port, block)
		ok = true
	})
	if !ok {
		block.Release()
		return nil, errors.New("the VM has no socket device")
	}
	select {
	case d := <-res:
		block.Release()
		if d.err != nil {
			return nil, d.err
		}
		syscall.CloseOnExec(d.fd)
		// Non-blocking, so the file is on Go's poller: deadlines work,
		// and a Close ends a read in progress.
		if err := syscall.SetNonblock(d.fd, true); err != nil {
			syscall.Close(d.fd)
			return nil, err
		}
		return os.NewFile(uintptr(d.fd), fmt.Sprintf("vsock:%d", port)), nil
	case <-time.After(connectTimeout):
		return nil, fmt.Errorf("vsock port %d: no answer in %v", port, connectTimeout)
	}
}

// Stop stops the VM at once.
func (r *Runner) Stop() error {
	if r.vm == 0 {
		return errors.New("the VM has not started")
	}
	done := make(chan error, 1)
	block := objc.NewBlock(func(_ objc.Block, err objc.ID) {
		if err != 0 {
			done <- errors.New(describe(err))
			return
		}
		done <- nil
	})
	onQueue(r.queue, func() { r.vm.Send(sel("stopWithCompletionHandler:"), block) })
	select {
	case err := <-done:
		block.Release()
		if err != nil {
			return fmt.Errorf("stopping the VM: %w", err)
		}
		r.end(errors.New("stopped by the host"))
		return nil
	case <-time.After(stopTimeout):
		return fmt.Errorf("the VM did not stop in %v", stopTimeout)
	}
}
