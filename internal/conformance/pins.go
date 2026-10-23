package conformance

import (
	"context"
	"fmt"
	"regexp"

	"github.com/bfreis/caboose/internal/config"
)

// Pins are the identity of the engine and of the sandbox, read when the
// sandbox is created and compared before every destructive exec. A
// destructive exec goes by ContainerID (container, gvisor), never by name,
// so that nothing between the check and the exec can swap the target.
type Pins struct {
	// EngineID is docker info's ID of the engine the run uses
	// (container, gvisor).
	EngineID string
	// ContainerID is the full 64-hex container ID (container, gvisor).
	ContainerID string
	// VMMStart is the vmm process's start time, MachineHash the sha256 of
	// machine.json in hex, and BootID the guest's
	// /proc/sys/kernel/random/boot_id (vm).
	VMMStart    string
	MachineHash string
	BootID      string
}

var bootIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (p Pins) check(isolation string) error {
	if isolation == config.KindVM {
		switch {
		case p.VMMStart == "":
			return fmt.Errorf("no vmm start time")
		case !hex64.MatchString(p.MachineHash):
			return fmt.Errorf("machine.json's hash %q is not a sha256", p.MachineHash)
		case !bootIDRe.MatchString(p.BootID):
			return fmt.Errorf("boot_id %q is not one", p.BootID)
		case p.EngineID != "" || p.ContainerID != "":
			return fmt.Errorf("a VM has container pins")
		}
		return nil
	}
	switch {
	case p.EngineID == "":
		return fmt.Errorf("no engine ID")
	case !hex64.MatchString(p.ContainerID):
		return fmt.Errorf("container ID %q is not a full ID", p.ContainerID)
	case p.VMMStart != "" || p.MachineHash != "" || p.BootID != "":
		return fmt.Errorf("a container has VM pins")
	}
	return nil
}

// Pin runs Verify, then reads and records the pins and the audit's
// result. It is called once, when the sandbox has been created (and
// again after the runner restarts it).
func (g *Guard) Pin(ctx context.Context) (Pins, error) {
	if err := g.Verify(ctx); err != nil {
		return Pins{}, err
	}
	pin := g.inputs().Pin
	if pin == nil {
		return Pins{}, refuse("pins", "no way to read the sandbox's identity was given")
	}
	p, err := pin(ctx)
	if err != nil {
		return Pins{}, refuse("pins", "reading the sandbox's identity: %v", err)
	}
	if err := p.check(g.isolation); err != nil {
		return Pins{}, &RefusalError{Item: "pins", Err: err}
	}
	g.mu.Lock()
	if g.isolation != config.KindVM && g.inspectedID != p.ContainerID {
		g.mu.Unlock()
		return Pins{}, refuse("pins", "the container inspected is not the one pinned")
	}
	a := *g.audited
	g.pinned, g.pinnedAudit = &p, &a
	g.mu.Unlock()
	return p, nil
}

// Pinned returns the pins and the audit recorded with them.
func (g *Guard) Pinned() (Pins, AuditResult, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pinned == nil {
		return Pins{}, AuditResult{}, false
	}
	return *g.pinned, *g.pinnedAudit, true
}

// VerifyDestructive is what runs immediately before each destructive
// exec: Verify, then the pins and the audited mounts read again and
// required equal to what was pinned. Anything different refuses, and
// nothing is executed.
func (g *Guard) VerifyDestructive(ctx context.Context) error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	g.mu.Lock()
	var pinned *Pins
	var pinnedAudit AuditResult
	if g.pinned != nil {
		pinned = g.pinned
		pinnedAudit = *g.pinnedAudit
	}
	g.mu.Unlock()
	if pinned == nil {
		return refuse("pins", "the sandbox was never pinned")
	}
	if err := g.Verify(ctx); err != nil {
		return err
	}
	pin := g.inputs().Pin
	if pin == nil {
		return refuse("pins", "no way to read the sandbox's identity was given")
	}
	now, err := pin(ctx)
	if err != nil {
		return refuse("pins", "reading the sandbox's identity: %v", err)
	}
	if now != *pinned {
		return refuse("pins", "the sandbox's or the engine's identity changed since it was pinned")
	}
	g.mu.Lock()
	audit, id := *g.audited, g.inspectedID
	g.mu.Unlock()
	if g.isolation != config.KindVM && id != pinned.ContainerID {
		return refuse("pins", "the container inspected is not the one pinned")
	}
	if !audit.Equal(pinnedAudit) {
		return refuse("pins", "the sandbox's mounts changed since it was pinned")
	}
	return nil
}
