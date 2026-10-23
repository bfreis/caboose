// Package fake is a model of a sandbox and of the host beside it, for
// testing the conformance suite's checks without anything real: an
// in-memory filesystem and a small interpreter for the dozen shell forms
// the checks use, under a policy of its own that can be switched, one
// control at a time, from enforcing to broken (a mutant).
package fake

import "sort"

// Mutant is one control broken. Enforcing (the empty mutant) breaks none.
type Mutant string

const Enforcing Mutant = ""

const (
	RootsWritable          Mutant = "roots writable"
	ParentWritable         Mutant = "parent writable"
	SymlinkFollowed        Mutant = "symlink followed"
	UnmountWritesThrough   Mutant = "unmount succeeds and then writes through"
	DaemonDirMounted       Mutant = "daemon dir mounted"
	SocketReachable        Mutant = "socket reachable"
	WebPortOpen            Mutant = "web port open without token"
	AnswerAccepted         Mutant = "answer accepted"
	AliasRunsWithoutGrant  Mutant = "alias runs without grant"
	HostExecOn             Mutant = "host exec on"
	EgressOpen             Mutant = "egress open"
	CheckpointStoreMounted Mutant = "checkpoint store mounted"
	RestoreInPlace         Mutant = "restore in place"
)

// Mutants that a later model interprets and registers (Register) for the
// runs that use them; they are in no table of the other checks.
const (
	// MountPointsReplaceable lets the sandbox appear to move or remove a
	// mount point (it reports success; no file changes).
	MountPointsReplaceable Mutant = "mount points replaceable"
	// ManagedWritable makes the managed directory writable.
	ManagedWritable Mutant = "managed directory writable"
	// OutsideReachable lets the sandbox read and write the host's canary
	// directories by a path that leaves the mounts.
	OutsideReachable Mutant = "outside reachable"
	// PartReadOnly makes the writable part refuse writes: a fault that
	// breaks every positive control.
	PartReadOnly Mutant = "part read-only"
	// LabelsDrift makes the container's labels say a writable part is read-only.
	LabelsDrift Mutant = "labels drift"
	// RootsNotMounts leaves the two roots plain directories of the
	// sandbox's own filesystem, not mount points: its mount table does not
	// list them, a rename of one succeeds (in the sandbox's view: no host
	// file changes) and an rmdir of one is refused only for not being
	// empty.
	RootsNotMounts Mutant = "roots not mount points"
	// WipeReachesOutside lets a recursive removal of the roots reach the
	// host's canaries that no mount reaches, as if a root's mount were the
	// directory above it: only the wipe acts on it.
	WipeReachesOutside Mutant = "wipe reaches outside"
	// UsernsRemountLands lets a user-namespace remount of a read-only root
	// succeed and a write through it land on the read-only canary.
	UsernsRemountLands Mutant = "userns remount lands"
	// UsernsUnmountReachesOut lets a user-namespace unmount of the part
	// reach the sandbox's own mounts while the probe reports it refused:
	// only the sandbox's mount table, read around the attempt, tells.
	UsernsUnmountReachesOut Mutant = "userns unmount reaches out"
)

// All are every mutant the model names, interpretable or not.
var All = []Mutant{
	RootsWritable, ParentWritable, SymlinkFollowed, UnmountWritesThrough,
	DaemonDirMounted, SocketReachable, WebPortOpen,
	AnswerAccepted, AliasRunsWithoutGrant, HostExecOn, EgressOpen,
	CheckpointStoreMounted, RestoreInPlace,
}

// interpretable are the mutants the filesystem model acts on; the others
// are names for controls whose checks arrive with their own models.
var interpretable = map[Mutant]bool{
	RootsWritable: true, ParentWritable: true, SymlinkFollowed: true, UnmountWritesThrough: true,
}

// Interpretable reports whether the model acts on m.
func Interpretable(m Mutant) bool { return interpretable[m] }

// Register makes a mutant of a later model known: from then on a Fake
// switched to it reports it through Active, for that model to read.
func Register(m Mutant) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registered[m] = true
}

// Registered are the extra mutants, sorted.
func Registered() []Mutant {
	registryMu.Lock()
	defer registryMu.Unlock()
	var out []Mutant
	for m := range registered {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
