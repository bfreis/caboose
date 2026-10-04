package vz

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/bfreis/caboose/internal/vm"
)

// entitlement is what Virtualization.framework refuses a process without.
const entitlement = "com.apple.security.virtualization"

// Check is caboose-vmm --check on a Mac: the framework, whether this Mac
// supports it, and whether this process is entitled to it. The caller
// fills in the version, OS and architecture.
func Check() vm.Check {
	c := vm.Check{Translated: vm.No, Supported: vm.Unknown, Entitled: vm.Unknown, Valid: vm.Unknown}
	c.MacOS, _ = syscall.Sysctl("kern.osproductversion")
	// Absent on an Intel Mac, which translates nothing.
	if t, err := syscall.SysctlUint32("sysctl.proc_translated"); err == nil && t == 1 {
		c.Translated = vm.Yes
	}
	if err := load(); err != nil {
		c.Framework = err.Error()
		return c
	}
	c.Framework = "ok"
	pool := class("NSAutoreleasePool").Send(sel("new"))
	defer pool.Send(sel("drain"))

	c.Supported = vm.No
	if objc.Send[bool](class("VZVirtualMachine"), sel("isSupported")) {
		c.Supported = vm.Yes
	}
	c.Entitled = entitled()
	c.Valid = validateMinimal()
	// The framework's own refusal is the last word; where the signature
	// could not be read, a validation that passes says it is there.
	switch {
	case strings.Contains(c.Valid, entitlement):
		c.Entitled = vm.No
	case c.Entitled == vm.Unknown && c.Valid == "ok":
		c.Entitled = vm.Yes
	}
	return c
}

// entitled reads the entitlement off this process's own signature, through
// the Security framework's SecTask: Yes, No, or Unknown when it could not
// ask.
func entitled() string {
	sec, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return vm.Unknown
	}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return vm.Unknown
	}
	var (
		createFromSelf func(allocator uintptr) uintptr
		copyValue      func(task, name, errp uintptr) uintptr
		release        func(uintptr)
	)
	purego.RegisterLibFunc(&createFromSelf, sec, "SecTaskCreateFromSelf")
	purego.RegisterLibFunc(&copyValue, sec, "SecTaskCopyValueForEntitlement")
	purego.RegisterLibFunc(&release, cf, "CFRelease")
	task := createFromSelf(0)
	if task == 0 {
		return vm.Unknown
	}
	defer release(task)
	// An NSString is a CFString, toll-free.
	v := copyValue(task, uintptr(nsstring(entitlement)), 0)
	if v == 0 {
		return vm.No
	}
	defer release(v)
	// A CFBoolean is an NSNumber.
	if objc.Send[bool](objc.ID(v), sel("isKindOfClass:"), class("NSNumber")) && objc.Send[bool](objc.ID(v), sel("boolValue")) {
		return vm.Yes
	}
	return vm.No
}

// validateMinimal validates the smallest configuration a Linux VM has --
// a boot loader, the fewest CPUs, the least memory -- with no device and
// nothing booted: "ok", or the framework's error, which names the
// entitlement when that is what is missing.
func validateMinimal() string {
	exe, err := os.Executable()
	if err != nil {
		return vm.Unknown
	}
	// Validation reads no kernel: any file will do.
	boot := class("VZLinuxBootLoader").Send(sel("alloc")).Send(sel("initWithKernelURL:"), fileURL(exe))
	vzc := class("VZVirtualMachineConfiguration")
	cfg := vzc.Send(sel("new"))
	cfg.Send(sel("setBootLoader:"), boot)
	cfg.Send(sel("setCPUCount:"), objc.Send[uint](vzc, sel("minimumAllowedCPUCount")))
	cfg.Send(sel("setMemorySize:"), objc.Send[uint64](vzc, sel("minimumAllowedMemorySize")))
	var nserr objc.ID
	if objc.Send[bool](cfg, sel("validateWithError:"), unsafe.Pointer(&nserr)) {
		return "ok"
	}
	return describe(nserr)
}
