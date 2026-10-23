package checks

import (
	"fmt"

	c "github.com/bfreis/caboose/internal/conformance"
)

// All are the checks in the order they run: the one that only reads, the
// refusals that write, then those that remove, replace or unmount (the
// writable part's own check first, since the others carry its control),
// with the one that unmounts last, since what a guest unmounts stays
// unmounted for the checks after it.
func All() []c.Check {
	byID := map[string]c.Check{}
	for _, fc := range fileChecks() {
		byID[fc.id] = fc.check()
	}
	out := []c.Check{r13(), byID["R1"], byID["R6"], byID["R7"], byID["R11"], r5()}
	for _, id := range []string{"R2", "R3", "R4", "R8", "R10", "R12"} {
		out = append(out, byID[id])
	}
	return append(out, r9(), r14())
}

// Paths are the host directories (relative to B) that the canaries of the
// checks are made in.
type Paths struct {
	// RO is the root with no writable part, Dev the root that holds the
	// writable part, Part the part, Managed the managed directory, Canary the directory of canaries that
	// no mount reaches.
	RO, Dev, Part, Managed, Canary string
}

// Spec is one canary to make before the checks run.
type Spec struct {
	Parent, Role string
	Kind         c.Kind
}

// Layout lists every canary the checks in All use, so the suite makes
// them before the run. Roles shared between checks are listed once.
func Layout(p Paths) ([]Spec, error) {
	var out []Spec
	seen := map[string]Spec{}
	add := func(parent, role string, k c.Kind) error {
		s := Spec{parent, role, k}
		if old, ok := seen[role]; ok {
			if old != s {
				return fmt.Errorf("checks: canary %q is wanted in two shapes", role)
			}
			return nil
		}
		seen[role] = s
		out = append(out, s)
		return nil
	}
	dir := map[zone]string{zRO: p.RO, zDev: p.Dev, zPart: p.Part, zManaged: p.Managed}
	for _, fc := range fileChecks() {
		for _, pt := range fc.parts {
			if err := add(dir[pt.tz], pt.target, pt.tkind); err != nil {
				return nil, err
			}
			for _, u := range allUsers {
				if err := add(p.Part, fc.allowedRole(pt, u), pt.allowed); err != nil {
					return nil, err
				}
				if pt.scratch {
					if err := add(p.Part, fc.scratchRole(pt, u), c.Dir); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if p.Canary == "" {
		return nil, fmt.Errorf("checks: no directory for the canaries outside the mounts")
	}
	for _, role := range watchRoles {
		if err := add(p.Canary, role, c.File); err != nil {
			return nil, err
		}
	}
	for _, s := range steps5() {
		for _, u := range allUsers {
			if err := add(p.Part, role5(s, u), s.kind); err != nil {
				return nil, err
			}
		}
	}
	for _, x := range []Spec{
		{p.RO, "um-ro", c.File}, {p.Dev, "um-parent", c.File}, {p.Part, "um-wr", c.File}, {p.Part, "um-scratch", c.Dir},
	} {
		if err := add(x.Parent, x.Role, x.Kind); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Every is every check of the suite, each once: the list verify-doc and
// the matrix are judged against.
func Every() []c.Check {
	var all []c.Check
	all = append(all, All()...)
	all = append(all, HostChecks()...)
	all = append(all, TrustChecks()...)
	all = append(all, CheckpointChecks()...)
	all = append(all, EgressChecks()...)
	all = append(all, DaemonChecks()...)
	var out []c.Check
	seen := map[string]bool{}
	for _, ck := range all {
		if !seen[ck.ID] {
			seen[ck.ID] = true
			out = append(out, ck)
		}
	}
	return out
}
