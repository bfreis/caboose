package conformance

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CheckpointOps are the host-side calls of the checkpoint checks: the
// product's own checkpoint commands, run against the suite's environment
// under B, and host-side reads of what they made. The real Host implements
// them in the launcher's conformance command; a Host that does not makes
// every checkpoint check an ERROR, never a PASS.
//
// None of them may reach anything outside B: a checkpoint is only ever of
// the suite's writable part, a restore only ever into a RestoreTarget
// (which is made only from a canary of the run), and a prune only ever of
// the environment's own store.
type CheckpointOps interface {
	// StorePath is the host path of the environment's checkpoint store
	// (B/home/envs/<name>/checkpoints), spelled as the sandbox's mount
	// sources are.
	StorePath() (string, error)
	// Populate fills the directory canary c, from the host and never
	// through a symlink, with n files of canary content.
	Populate(c Canary, n int) error
	// TakeCheckpoint is `caboose checkpoint create PATH`: a checkpoint now of the
	// configured path, a container path.
	TakeCheckpoint(ctx context.Context, path string) (CheckpointTaken, error)
	// List is `caboose checkpoint list --json`: every configured path's
	// checkpoints, newest first.
	List(ctx context.Context) ([]CheckpointPath, error)
	// Tree is the identity of every path below the top of checkpoint id's
	// copy, relative to that top and slash-separated (the top itself is
	// left out), read from the host without following a symlink.
	Tree(id string) (map[string]Identity, error)
	// Restore is `caboose checkpoint restore ID --to DIR` with DIR the
	// target's host path (Guard.RestorePath). A refusal by the product is
	// an outcome; an error is anything else.
	Restore(ctx context.Context, id string, to RestoreTarget) (RestoreOutcome, error)
	// Prune runs the product's prune of every key of the environment's
	// store, under the store's lock, as the daemon's prune round does:
	// the product's retention rules, but with ages measured from at
	// rather than from each key's newest checkpoint (which is still always
	// kept), the store's own pins and the checkpoints in pinned never
	// dropped. It returns the IDs it dropped.
	Prune(ctx context.Context, at time.Time, pinned []string) ([]string, error)
	// Discard removes what a restore wrote at to, a name inside a
	// directory canary (TargetIn), through saferm with B as the root
	// (Guard.DiscardRestore). A check that restores where the sandbox
	// sees calls it once it has read its verdict, so that the copy, which
	// holds the run's names and nonce, is not left for the checks after
	// it to find. Nothing there is not an error.
	Discard(to RestoreTarget) error
}

// CheckpointTaken is what a create said.
type CheckpointTaken struct {
	ID string
	// Shrunk is the earlier checkpoint the create said this one shrank
	// sharply from; "" when it said nothing of the kind.
	Shrunk string
}

// CheckpointPath is one configured path as the listing shows it.
type CheckpointPath struct {
	Path, Key string
	// Shrunk is the earlier checkpoint to restore from while a sharp
	// shrink stands.
	Shrunk      string
	Checkpoints []CheckpointInfo
}

// CheckpointInfo is one checkpoint as listed.
type CheckpointInfo struct {
	ID, Reason string
	// Shrunk is the earlier checkpoint this one shrank sharply from.
	Shrunk         string
	Entries, Bytes int64
	// Pinned is set while the store keeps it outside retention, after a
	// later one shrank sharply from it.
	Pinned bool
}

// RestoreRefusal is why the product refused a restore.
type RestoreRefusal string

const (
	// RestoreDone is no refusal: the restore was made.
	RestoreDone RestoreRefusal = ""
	// RefusedExists is the product's "the target exists".
	RefusedExists RestoreRefusal = "exists"
	// RefusedUnsafe is the product's "restore may not write there".
	RefusedUnsafe RestoreRefusal = "unsafe"
)

// RestoreOutcome is what a restore came to.
type RestoreOutcome struct {
	Refused RestoreRefusal
	Message string
	// Warnings are the restore's warnings, as printed.
	Warnings []string
}

// CheckpointOps are the checkpoint calls, inside the envelope only.
func (e Env) CheckpointOps() (CheckpointOps, error) {
	if err := e.sealed(); err != nil {
		return nil, err
	}
	ops, ok := e.host.(CheckpointOps)
	if !ok || ops == nil {
		return nil, errors.New("conformance: this host offers no checkpoint calls")
	}
	return ops, nil
}

// RestoreTarget is where a restore may be asked to write: a canary of the
// run itself (an existing target, which the product must refuse), or a
// fixed name inside a directory canary. Nothing else makes one.
type RestoreTarget struct {
	canary, name string
}

var (
	targetNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	canaryBaseRe = regexp.MustCompile(`^conf-[0-9a-f]{8}-[a-z][a-z0-9-]{0,23}$`)
)

func canaryRelOK(rel string) bool {
	return rel != "" && path.Clean(rel) == rel && !path.IsAbs(rel) && rel != ".." &&
		!strings.HasPrefix(rel, "../") && canaryBaseRe.MatchString(path.Base(rel))
}

// TargetAt is the canary c itself as a restore's target.
func TargetAt(c Canary) (RestoreTarget, error) {
	if !canaryRelOK(c.Rel) {
		return RestoreTarget{}, fmt.Errorf("conformance: %q is not a canary of the suite", c.Rel)
	}
	return RestoreTarget{canary: c.Rel}, nil
}

// TargetIn is the entry name inside the directory canary c.
func TargetIn(c Canary, name string) (RestoreTarget, error) {
	if !canaryRelOK(c.Rel) || c.Kind != Dir {
		return RestoreTarget{}, fmt.Errorf("conformance: %q is not a directory canary of the suite", c.Rel)
	}
	if !targetNameRe.MatchString(name) {
		return RestoreTarget{}, fmt.Errorf("conformance: %q is not a name a restore may be given", name)
	}
	return RestoreTarget{canary: c.Rel, name: name}, nil
}

func (t RestoreTarget) valid() bool {
	return canaryRelOK(t.canary) && (t.name == "" || targetNameRe.MatchString(t.name))
}

// Canary is the path, relative to B, of the canary the target is or is in.
func (t RestoreTarget) Canary() string { return t.canary }

// Rel is the target's path relative to B; "" for a target no
// constructor made.
func (t RestoreTarget) Rel() string {
	if !t.valid() {
		return ""
	}
	if t.name == "" {
		return t.canary
	}
	return t.canary + "/" + t.name
}

// RestorePath is the host path of t, which the real Restore passes to the
// launcher and nothing else: t must be, or be a name inside, a canary this
// guard made (by its top, a directory for a name inside it).
func (g *Guard) RestorePath(t RestoreTarget) (string, error) {
	if g == nil || !g.made {
		return "", refuse("guard", "not an environment this suite created")
	}
	if !t.valid() {
		return "", errors.New("conformance: a restore target is made only from a canary")
	}
	g.mu.Lock()
	rec, ok := g.canaries[t.canary]
	g.mu.Unlock()
	switch {
	case !ok || rec.canary.Rel != t.canary:
		return "", fmt.Errorf("conformance: %s is not a canary of this run", t.canary)
	case t.name != "" && rec.canary.Kind != Dir:
		return "", fmt.Errorf("conformance: %s is not a directory canary", t.canary)
	}
	return filepath.Join(g.base, filepath.FromSlash(t.Rel())), nil
}
