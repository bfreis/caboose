package fake

import (
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/conformance"
)

// The model of a sandbox made with an empty [roots]: the host's canaries
// are where they always are, and only the managed directory is mounted.

// The mutants of that sandbox. Each breaks one thing a check can see, and
// nothing else, so that each of N1's looks is shown to be the one that tells.
const (
	// NoRootsRootStaysMounted mounts the read-only root anyway, which the
	// engine, the mount table, /work and a read all say.
	NoRootsRootStaysMounted Mutant = "no roots: a root stays mounted"
	// NoRootsDefaultRoot mounts the default ~/dev, as a config.toml with no
	// [roots] table at all does, under labels that say none.
	NoRootsDefaultRoot Mutant = "no roots: the default root is mounted"
	// NoRootsLabelLists says a root in the roots label and mounts nothing.
	NoRootsLabelLists Mutant = "no roots: the roots label lists one"
	// NoRootsLabelNull has the roots label say null, which is no list.
	NoRootsLabelNull Mutant = "no roots: the roots label is null"
	// NoRootsWritableLabel says a root in the writable label.
	NoRootsWritableLabel Mutant = "no roots: the writable label lists one"
	// NoRootsHiddenMount has a mount the engine does not report: only the
	// sandbox's own mount table shows it.
	NoRootsHiddenMount Mutant = "no roots: a mount the engine does not report"
	// NoRootsRootMountedElsewhere mounts the read-only root's directory
	// outside /work, where the labels, /work and the mount table say
	// nothing of it: only its source in the engine's list tells.
	NoRootsRootMountedElsewhere Mutant = "no roots: a root is mounted outside /work"
	// NoRootsCanaryReadable lets a canary's path read with no mount or entry
	// of /work to show for it: only the read tells.
	NoRootsCanaryReadable Mutant = "no roots: a canary reads"
	// NoRootsWorkHolds leaves an entry in /work: only the listing shows it.
	NoRootsWorkHolds Mutant = "no roots: /work holds something"
)

// NoRootsMutants are every mutant of that sandbox.
var NoRootsMutants = []Mutant{
	NoRootsRootStaysMounted, NoRootsDefaultRoot, NoRootsLabelLists, NoRootsLabelNull,
	NoRootsWritableLabel, NoRootsHiddenMount, NoRootsRootMountedElsewhere, NoRootsCanaryReadable, NoRootsWorkHolds,
}

// SetNoRoots makes the sandbox one made with no roots.
func (f *Fake) SetNoRoots() { f.mu.Lock(); f.noRoots = true; f.mu.Unlock() }

// The sandbox's cat gone odd: a setup's fault, not a control's.
const (
	catDenied  = "denied"
	catMissing = "missing"
)

// SetCatDenied makes every cat refused, the managed canary's too.
func (f *Fake) SetCatDenied() { f.mu.Lock(); f.catFault = catDenied; f.mu.Unlock() }

// SetCatMissing makes cat a command the sandbox does not have by the time
// it reads under /work: after the read of the managed canary went well.
func (f *Fake) SetCatMissing() { f.mu.Lock(); f.catFault = catMissing; f.mu.Unlock() }

func (f *Fake) noRootsLabels() map[string]string {
	roots, writable := `[]`, `{}`
	ro := "/work/conf-" + f.n8 + "-ro"
	switch {
	case f.mutants[NoRootsLabelLists]:
		roots = `["` + ro + `"]`
	case f.mutants[NoRootsLabelNull]:
		roots = `null`
	}
	if f.mutants[NoRootsWritableLabel] {
		writable = `{"` + ro + `":[]}`
	}
	labels := map[string]string{"caboose.isolation": f.isolate, assets.LabelRoots: roots, assets.LabelWritable: writable}
	f.labelUser(labels)
	return labels
}

func (f *Fake) noRootsMounts() []conformance.Mount {
	ms := []conformance.Mount{{Source: "<B>/" + managedDir, Target: "/etc/claude-code", ReadOnly: true}}
	if f.mutants[NoRootsRootStaysMounted] {
		ms = append(ms, conformance.Mount{Source: "<B>/" + f.RO(), Target: "/work/conf-" + f.n8 + "-ro", ReadOnly: true})
	}
	if f.mutants[NoRootsRootMountedElsewhere] {
		ms = append(ms, conformance.Mount{Source: "<B>/" + f.RO(), Target: "/home/agent/project", ReadOnly: true})
	}
	if f.mutants[NoRootsDefaultRoot] {
		ms = append(ms, conformance.Mount{Source: "/home/person/dev", Target: "/work/dev"})
	}
	return ms
}

// noRootsReaches reports whether a path under /work reaches the host's
// roots in that sandbox.
func (f *Fake) noRootsReaches(rest string) bool {
	if f.mutants[NoRootsCanaryReadable] {
		return true
	}
	ro := "conf-" + f.n8 + "-ro"
	return f.mutants[NoRootsRootStaysMounted] && (rest == ro || strings.HasPrefix(rest, ro+"/"))
}

// workEntries is what /work lists.
func (f *Fake) workEntries() []string {
	if !f.noRoots {
		return []string{"conf-" + f.n8 + "-dev", "conf-" + f.n8 + "-ro"}
	}
	var out []string
	if f.mutants[NoRootsDefaultRoot] {
		out = append(out, "dev")
	}
	if f.mutants[NoRootsRootStaysMounted] {
		out = append(out, "conf-"+f.n8+"-ro")
	}
	if f.mutants[NoRootsWorkHolds] {
		out = append(out, "something")
	}
	return out
}
