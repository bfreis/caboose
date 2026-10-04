package vm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// CheckFormat is the first line of caboose-vmm --check's output, and its
// format's version: a caboose-vmm without it is one older than the check.
const CheckFormat = "caboose-vmm-check=1"

// Tri-state answers of a Check: what the check found, or that it could
// not ask.
const (
	Yes     = "yes"
	No      = "no"
	Unknown = "unknown"
)

// Check is what caboose-vmm --check reports: whether this process, on
// this Mac, can run a VM. It is written as key=value lines, one value to a
// line, so the launcher reads it with no JSON of a binary it does not
// trust to be its own.
type Check struct {
	Version string // caboose-vmm's version (internal/version)
	OS      string // runtime.GOOS
	Arch    string // runtime.GOARCH
	MacOS   string // the product version, "14.5"; empty off a Mac
	// Translated is Yes when the process runs under Rosetta: an x86_64
	// caboose-vmm on Apple silicon.
	Translated string
	// Framework is "ok" once Virtualization.framework is loaded with every
	// class a VM of caboose's needs, else why not.
	Framework string
	// Supported is VZVirtualMachine.isSupported: the hardware and the OS
	// can run a VM (not inside a VM without nested virtualization).
	Supported string
	// Entitled is whether the process's signature carries
	// com.apple.security.virtualization, as the Security framework reads
	// it, or, where it could not, as a validation says.
	Entitled string
	// Valid is "ok" when a minimal configuration validates, else the
	// framework's error: what an entitlement it lacks shows up in.
	Valid string
	// Error is set when the check itself failed (it timed out).
	Error string
}

// Write writes c as caboose-vmm --check prints it.
func (c Check) Write(w io.Writer) error {
	var b bytes.Buffer
	b.WriteString(CheckFormat + "\n")
	for _, kv := range [][2]string{
		{"version", c.Version}, {"os", c.OS}, {"arch", c.Arch}, {"macos", c.MacOS},
		{"translated", c.Translated}, {"framework", c.Framework}, {"supported", c.Supported},
		{"entitled", c.Entitled}, {"valid", c.Valid}, {"error", c.Error},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s=%s\n", kv[0], oneLine(kv[1]))
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

// Usable reports whether c says a VM can run: what the launcher offers vm
// on.
func (c Check) Usable() bool {
	return c.Error == "" && c.OS == "darwin" && c.Translated != Yes && c.Framework == "ok" &&
		c.Supported == Yes && c.Entitled == Yes
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// maxCheck bounds what ParseCheck reads: a check is a few hundred bytes.
const maxCheck = 16 << 10

// ErrNoCheck is output that is not a check's: a caboose-vmm from before
// --check, which prints its usage instead.
var ErrNoCheck = errors.New("caboose-vmm has no --check")

// ParseCheck reads caboose-vmm --check's output. Keys it does not know
// are skipped, for a newer caboose-vmm's sake.
func ParseCheck(out []byte) (Check, error) {
	var c Check
	if len(out) > maxCheck {
		return c, fmt.Errorf("caboose-vmm --check wrote %d bytes, more than a check's", len(out))
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			if line != CheckFormat {
				return c, ErrNoCheck
			}
			first = false
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "version":
			c.Version = v
		case "os":
			c.OS = v
		case "arch":
			c.Arch = v
		case "macos":
			c.MacOS = v
		case "translated":
			c.Translated = v
		case "framework":
			c.Framework = v
		case "supported":
			c.Supported = v
		case "entitled":
			c.Entitled = v
		case "valid":
			c.Valid = v
		case "error":
			c.Error = v
		}
	}
	if first {
		return c, ErrNoCheck
	}
	return c, nil
}
