// Package linkdebug reads CABOOSE_DEBUG_LINK, a switch for bisecting the
// host link's throughput on one machine in one run: each knob turns one
// part of the link back to what it was before its windows were widened.
// It is for debugging only, and changes nothing when unset.
//
//	CABOOSE_DEBUG_LINK="sockbuf=0,guestbuf=0,window=262144,relaybuf=0,onewrite=0"
//
// The knobs:
//   - sockbuf=0: caboose-vmm and hvsock.Dial ask for no socket buffers on
//     vm.sock's connections or on the hypervisor's vsock (the system's own)
//   - guestbuf=0: the guest's vsock listeners keep Linux's buffer
//   - window=N: what this side's streams receive in flight, and announce
//     in their hello (within agentproto's DefaultWindow and MaxWindow)
//   - relaybuf=0: caboose-vmm relays through io.Copy's buffers
//   - onewrite=0: a frame's header and payload are two writes
//
// The launcher's environment reaches caboose-vmm and the link it starts;
// a vm guest, which has none of it, reads it from the kernel's command
// line (caboose.debuglink=..., which vm.DefaultCmdline adds).
package linkdebug

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// Var is the environment variable.
const Var = "CABOOSE_DEBUG_LINK"

// cmdlineKey is its name on a guest kernel's command line.
const cmdlineKey = "caboose.debuglink="

// Knobs is CABOOSE_DEBUG_LINK parsed. The zero value is the link as built.
type Knobs struct {
	Raw         string // what was set, "" when nothing was
	NoSockBuf   bool
	NoGuestBuf  bool
	NoRelayBuf  bool
	SplitFrames bool
	Window      int // 0: the side's own
}

// Parse reads a value of CABOOSE_DEBUG_LINK. One with a character outside
// [a-z0-9=,] is ignored whole: it goes on a kernel's command line.
// Unknown knobs are ignored.
func Parse(v string) Knobs {
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '=' || r == ',') {
			return Knobs{}
		}
	}
	k := Knobs{Raw: v}
	for _, kv := range strings.Split(v, ",") {
		name, val, _ := strings.Cut(kv, "=")
		off := val == "0"
		switch name {
		case "sockbuf":
			k.NoSockBuf = off
		case "guestbuf":
			k.NoGuestBuf = off
		case "relaybuf":
			k.NoRelayBuf = off
		case "onewrite":
			k.SplitFrames = off
		case "window":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				k.Window = n
			}
		}
	}
	return k
}

// Get is this process's knobs: the environment's, else a guest kernel's
// command line's.
var Get = sync.OnceValue(func() Knobs { return Parse(value()) })

func value() string {
	if v := os.Getenv(Var); v != "" {
		return v
	}
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return fromCmdline(string(b))
}

func fromCmdline(cmdline string) string {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, cmdlineKey); ok {
			return v
		}
	}
	return ""
}

// Cmdline is what a guest kernel's command line gets for this process's
// knobs: " caboose.debuglink=..." from the environment, or "".
func Cmdline() string {
	k := Parse(os.Getenv(Var))
	if k.Raw == "" {
		return ""
	}
	return " " + cmdlineKey + k.Raw
}
