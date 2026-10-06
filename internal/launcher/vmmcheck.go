package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/version"
	"github.com/bfreis/caboose/internal/vm"
)

// vmmCheckTimeout bounds caboose-vmm --check, which bounds itself at 5s:
// past this it is hung, or held by something that is not it.
var vmmCheckTimeout = 15 * time.Second

// minMacOS is the first macOS vm runs on (vz's required classes).
const minMacOS = 13

// vmmVerdict is what caboose-vmm --check said, in the launcher's words:
// Problem is empty when vm can run, else what is wrong, and Fix what to do.
type vmmVerdict struct {
	Check   vm.Check
	Problem string
	Fix     string
}

// vmmFix is how to get a caboose-vmm that is this caboose's own, signed.
func (a *App) vmmFix() string {
	if a.Checkout != "" {
		return fmt.Sprintf("run 'make vmm' in %s on this Mac, which builds and signs it", a.Checkout)
	}
	return "reinstall caboose, which brings a signed caboose-vmm: curl -fsSL " + installScriptURL + " | sh"
}

// otherIsolation is the way out when this Mac cannot run vm at all.
func (a *App) otherIsolation() string {
	return otherProfile(a.Cfg)
}

// checkVMM runs path --check and judges what it says.
func (a *App) checkVMM(path string) vmmVerdict {
	ctx, cancel := context.WithTimeout(context.Background(), vmmCheckTimeout)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "--check")
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	return a.judgeVMM(path, out.Bytes(), errb.String(), runErr, ctx.Err())
}

// judgeVMM is checkVMM's judgement of a run: its output, and how it ended.
func (a *App) judgeVMM(path string, out []byte, stderr string, runErr, ctxErr error) vmmVerdict {
	fail := func(fix, format string, args ...any) vmmVerdict {
		return vmmVerdict{Problem: fmt.Sprintf(format, args...), Fix: fix}
	}
	if ctxErr != nil {
		return fail(a.vmmFix()+"; if it hangs again, restart the Mac, or "+a.otherIsolation(),
			"%s --check did not finish in %v", path, vmmCheckTimeout)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fail(a.vmmFix()+"; if a security tool blocks it, allow caboose-vmm there",
				"macOS stopped %s (%v): its signature is broken, or a security tool refused it", path, ws.Signal())
		}
	} else if runErr != nil {
		return fail(a.vmmFix(), "%s cannot run: %v", path, runErr)
	}
	c, err := vm.ParseCheck(out)
	if errors.Is(err, vm.ErrNoCheck) {
		return fail(a.vmmFix(), "%s did not answer --check as this caboose's caboose-vmm does%s", path, said(stderr))
	}
	if err != nil {
		return fail(a.vmmFix(), "%v", err)
	}
	v := vmmVerdict{Check: c}
	mine := version.Get().Version
	switch {
	case c.Error != "":
		v.Problem, v.Fix = "caboose-vmm could not check this Mac: "+c.Error, "run 'caboose doctor' again; if it persists, restart the Mac, or "+a.otherIsolation()
	case c.Version != mine:
		v.Problem, v.Fix = fmt.Sprintf("%s is version %s, and this caboose %s: they are released together", path, or(c.Version, "unknown"), mine), a.vmmFix()
	case c.OS != "darwin":
		v.Problem, v.Fix = fmt.Sprintf("%s is built for %s, not macOS", path, or(c.OS, "an unknown system")), a.vmmFix()
	case c.Translated == vm.Yes:
		v.Problem, v.Fix = fmt.Sprintf("%s is a %s build, running under Rosetta: vm needs the arm64 one", path, c.Arch), a.vmmFix()
	case c.Arch != "arm64":
		v.Problem, v.Fix = fmt.Sprintf("vm runs on Apple silicon only, for now, and this Mac is %s", or(c.Arch, "unknown")), a.otherIsolation()
	case macOSMajor(c.MacOS) > 0 && macOSMajor(c.MacOS) < minMacOS:
		v.Problem, v.Fix = fmt.Sprintf("this Mac runs macOS %s, and vm needs macOS %d or later", c.MacOS, minMacOS), "update macOS, or "+a.otherIsolation()
	case c.Framework != "ok":
		v.Problem, v.Fix = "Virtualization.framework cannot be used: "+or(c.Framework, "caboose-vmm did not say why"), "update macOS, or "+a.otherIsolation()
	case c.Supported != vm.Yes:
		v.Problem, v.Fix = "Virtualization.framework says this Mac cannot run VMs (as inside a VM without nested virtualization)", a.otherIsolation()
	case c.Entitled == vm.No:
		v.Problem, v.Fix = fmt.Sprintf("%s is not signed with the virtualization entitlement (com.apple.security.virtualization), so macOS refuses it VMs%s",
			path, validSaid(c.Valid)), a.vmmFix()
	case c.Entitled != vm.Yes:
		v.Problem, v.Fix = fmt.Sprintf("caboose-vmm could not tell whether it has the virtualization entitlement%s", validSaid(c.Valid)), a.vmmFix()
	}
	return v
}

// macOSMajor is the major version of a product version, "14" of "14.5";
// 0 when it is not one.
func macOSMajor(v string) int {
	n, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if err != nil {
		return 0
	}
	return n
}

func said(stderr string) string {
	if s := firstLine(strings.TrimSpace(stderr)); s != "" {
		return fmt.Sprintf(" (it said: %s)", s)
	}
	return ""
}

func validSaid(valid string) string {
	if valid == "" || valid == "ok" || valid == vm.Unknown {
		return ""
	}
	return " (Virtualization.framework: " + valid + ")"
}
