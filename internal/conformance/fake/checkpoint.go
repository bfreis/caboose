package fake

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/conformance"
)

// The mutants of the checkpoint store and its commands, which Checkpoints
// acts on (beside CheckpointStoreMounted and RestoreInPlace, the model's
// own names for two of them).
const (
	// StoreParentMounted mounts the environment's directory, which holds
	// the store.
	StoreParentMounted Mutant = "store's parent mounted"
	// StoreChildMounted mounts a directory inside the store.
	StoreChildMounted Mutant = "a directory in the store mounted"
	// StoreSeen makes the store's files visible in the sandbox through no
	// mount the sandbox lists.
	StoreSeen Mutant = "store visible without a mount"
	// CheckpointAliasesLive makes a checkpoint share the live files.
	CheckpointAliasesLive Mutant = "checkpoint shares the live files"
	// RestoreWrong leaves files out of a restore.
	RestoreWrong Mutant = "restore gives another tree"
	// RestoreRefused refuses every restore.
	RestoreRefused Mutant = "restore refuses everything"
	// RestoreFollowsLink restores through a dangling link at the target.
	RestoreFollowsLink Mutant = "restore follows a symbolic link"
	// RestoreUnsafeSilent restores where a sandbox can write, unwarned.
	RestoreUnsafeSilent Mutant = "restore into a sandbox-writable place unwarned"
	// RestoreUnsafeWarned restores where a sandbox can write, with a
	// warning in place of the refusal.
	RestoreUnsafeWarned Mutant = "restore into a sandbox-writable place, only warned"
	// ShrinkUnflagged never flags a sharp shrink.
	ShrinkUnflagged Mutant = "shrink not flagged"
	// PruneOutsideStore removes the file beside the store too.
	PruneOutsideStore Mutant = "prune removes beside the store"
	// PruneIgnoresPins drops pinned checkpoints.
	PruneIgnoresPins Mutant = "prune drops pinned checkpoints"
	// PruneDropsNewest drops the newest checkpoint once it is old.
	PruneDropsNewest Mutant = "prune drops the newest checkpoint"
	// ShrinkUnpinned flags a sharp shrink but pins nothing.
	ShrinkUnpinned Mutant = "shrink flagged, the earlier checkpoint not pinned"
)

// CheckpointMutants are the mutants Checkpoints acts on.
var CheckpointMutants = []Mutant{
	CheckpointStoreMounted, StoreParentMounted, StoreChildMounted, StoreSeen, CheckpointAliasesLive, RestoreInPlace, RestoreWrong,
	RestoreRefused, RestoreFollowsLink, RestoreUnsafeSilent, RestoreUnsafeWarned, ShrinkUnflagged, PruneOutsideStore,
	PruneIgnoresPins, PruneDropsNewest, ShrinkUnpinned,
}

// CkptFault is the setup, or the product's answer, gone odd: a check
// must say ERROR, never PASS.
type CkptFault string

const (
	StoreUnknown     CkptFault = "store path unknown"
	MountsUnlisted   CkptFault = "the mounts list nothing"
	ScanBlind        CkptFault = "the scan finds nothing"
	CreateFails      CkptFault = "create fails"
	TreeUnreadable   CkptFault = "checkpoint unreadable"
	CopyIncomplete   CkptFault = "checkpoint leaves files out"
	DamageIgnored    CkptFault = "the sandbox lies about removing"
	LinkNotMade      CkptFault = "the sandbox's link is not made"
	RestoreErrors    CkptFault = "restore errors"
	ControlWarned    CkptFault = "every restore is warned"
	PopulateFails    CkptFault = "populate fails"
	ShrinkTooSmall   CkptFault = "populate makes too few files to shrink"
	PruneNothing     CkptFault = "prune drops nothing"
	PinnedVanishes   CkptFault = "the pinned checkpoint vanishes"
	ListFails        CkptFault = "list fails"
	StubsGone        CkptFault = "the stubs are not in place"
	RestoreAsExists  CkptFault = "every restore refused as existing"
	UnsafeAsExisting CkptFault = "unsafe targets refused as existing"
	DiscardFails     CkptFault = "a restore's copy cannot be removed"
)

// CkptFaults are every fault Checkpoints acts on.
var CkptFaults = []CkptFault{
	StoreUnknown, MountsUnlisted, ScanBlind, CreateFails, TreeUnreadable, CopyIncomplete,
	DamageIgnored, LinkNotMade, RestoreErrors, ControlWarned, PopulateFails, ShrinkTooSmall,
	PruneNothing, PinnedVanishes, ListFails, StubsGone, RestoreAsExists, UnsafeAsExisting,
	DiscardFails,
}

// CkptEnv is the throwaway environment's name in the model.
const CkptEnv = "cf-5e6f7a8b"

// The model's directories, relative to B.
const (
	CkptHome     = "home"
	CkptEnvDir   = "home/envs/" + CkptEnv
	CkptStore    = CkptEnvDir + "/checkpoints"
	CkptDataKeep = "data/home/.config/caboose"
)

type checkpoint struct {
	id       string
	stamp    time.Time
	reason   string
	tree     map[string]*node // below the part, by relative path
	entries  int64
	bytes    int64
	shrunk   string
	pruned   bool
	vanished bool
	pinned   bool // by the store, after a sharp shrink
}

// Checkpoints is a Link with the host's checkpoint store and the product's
// checkpoint commands: what the checkpoint checks talk to. The store is a
// model of its own, never nodes of the filesystem, so nothing in the
// sandbox's world can reach it but a mutant.
type Checkpoints struct {
	*Link

	ckpts  []*checkpoint
	seq    int
	cfault map[CkptFault]bool
	// Targets are every restore target asked for, by path relative to B.
	Targets []string
	// Foreign are the targets asked for that are no canary of the run.
	Foreign []string
	// Discarded are the restore targets whose copies were removed.
	Discarded []string
}

// NewCheckpoints is a Checkpoints over a new Link under isolation and
// trust, with the directories the checkpoint canaries go in.
func NewCheckpoints(n8, isolation, trust string) (*Checkpoints, error) {
	l, err := NewLink(n8, isolation, trust)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"home", "home/envs", CkptEnvDir, CkptStore, "data", "data/home", "data/home/.config", CkptDataKeep} {
		l.nodes[d] = l.newNode(kDir, "")
	}
	return &Checkpoints{Link: l, cfault: map[CkptFault]bool{}}, nil
}

// SetCkptFault turns a fault on.
func (k *Checkpoints) SetCkptFault(f CkptFault) { k.mu.Lock(); k.cfault[f] = true; k.mu.Unlock() }

// Env is a conformance.Env over the model.
func (k *Checkpoints) Env(isolation, trust string) conformance.Env {
	return conformance.NewEnv(k, k, k, k, isolation, trust)
}

func (k *Checkpoints) StubsInPlace() error {
	k.mu.Lock()
	gone := k.cfault[StubsGone]
	k.mu.Unlock()
	if gone {
		return errors.New("the stub notifier is not first on the daemon's PATH")
	}
	return k.Link.StubsInPlace()
}

func (k *Checkpoints) partPath() string { return "/work/conf-" + k.n8 + "-dev/part" }

const storeTarget = "/mnt/conf-store"

func (k *Checkpoints) Mounts() []conformance.Mount {
	// Fake.Mounts takes the same lock: ask it first.
	ms := k.Fake.Mounts()
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfault[MountsUnlisted] {
		return nil
	}
	if k.mutants[CheckpointStoreMounted] {
		ms = append(ms, conformance.Mount{Source: "<B>/" + CkptStore, Target: storeTarget})
	}
	if k.mutants[StoreChildMounted] {
		ms = append(ms, conformance.Mount{Source: "<B>/" + CkptStore + "/work-conf-" + k.n8 + "-dev-part", Target: "/mnt/conf-key"})
	}
	if k.mutants[StoreParentMounted] {
		ms = append(ms, conformance.Mount{Source: "<B>/" + CkptEnvDir + "/", Target: "/mnt/conf-env"})
	}
	return ms
}

// Exec runs the scan in the model, and hands the rest on.
func (k *Checkpoints) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if rc, o, e, ok := k.agentShut(user); ok {
		return rc, o, e, nil
	}
	body, ok := conformance.Unwrap(argv)
	if !ok {
		return k.Link.Exec(ctx, user, argv, stdin)
	}
	k.mu.Lock()
	switch {
	case strings.HasPrefix(body, conformance.ScanPrefix):
		defer k.mu.Unlock()
		if k.down || k.absent || k.stopped {
			return -1, "", "", errors.New("the sandbox is not running")
		}
		if k.shellNo {
			return 127, "", "exec: the shell is not there: no such file or directory", nil
		}
		k.event("exec")
		return 0, "\nrc=0\n", strings.Join(k.scan(body), "\n"), nil
	case k.cfault[DamageIgnored] && isDestructive(body):
		defer k.mu.Unlock()
		k.event("exec-destructive")
		return 0, "\nrc=0\n", "", nil
	case k.cfault[LinkNotMade] && strings.HasPrefix(body, "ln -s "):
		defer k.mu.Unlock()
		k.event("exec")
		return 0, "\nrc=0\n", "", nil
	}
	k.mu.Unlock()
	return k.Link.Exec(ctx, user, argv, stdin)
}

// scan is find / ... -name A -print -o -name B -print: every node of the
// sandbox's world with one of the names, and, with StoreSeen, the store's
// files under storeTarget. A store mounted is in the mount list only, so
// that each half of C1 is held to a mutant of its own.
func (k *Checkpoints) scan(body string) []string {
	if k.cfault[ScanBlind] {
		return nil
	}
	toks := tokens(body)
	names := map[string]bool{}
	for i, t := range toks {
		if t == "-name" && i+1 < len(toks) {
			names[toks[i+1]] = true
		}
	}
	var out []string
	for _, p := range k.sortedKeys() {
		if !names[path.Base(p)] {
			continue
		}
		switch {
		case strings.HasPrefix(p, "roots/"):
			out = append(out, sandboxPath(p))
		case strings.HasPrefix(p, CkptStore+"/") && k.mutants[StoreSeen]:
			out = append(out, storeTarget+"/"+strings.TrimPrefix(p, CkptStore+"/"))
		}
	}
	return out
}

// CheckpointOps.

func (k *Checkpoints) StorePath() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfault[StoreUnknown] {
		return "", errors.New("no store")
	}
	return "<B>/" + CkptStore, nil
}

func (k *Checkpoints) canaryOf(rel string) (conformance.Canary, bool) {
	for _, cn := range k.canary {
		if cn.Rel == rel {
			return cn, true
		}
	}
	return conformance.Canary{}, false
}

func (k *Checkpoints) Populate(c conformance.Canary, n int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfault[PopulateFails] {
		return errors.New("populate: no space")
	}
	cn, ok := k.canaryOf(c.Rel)
	if !ok || cn.Kind != conformance.Dir || k.nodes[c.Rel] == nil {
		return fmt.Errorf("%s is not a directory canary", c.Rel)
	}
	if k.cfault[ShrinkTooSmall] {
		n /= 4
	}
	base := path.Base(c.Rel)
	for i := 1; i <= n; i++ {
		p := fmt.Sprintf("%s/%s-%d", c.Rel, base, i)
		k.nodes[p] = k.newNode(kFile, "caboose-conformance "+k.nonce+" bulk\n")
		k.logMut("populate", p)
	}
	return nil
}

func (k *Checkpoints) take(reason string) *checkpoint {
	k.seq++
	ck := &checkpoint{
		id:     fmt.Sprintf("work-conf-%s-dev-part@%04d", k.n8, k.seq),
		stamp:  k.Now().Add(time.Duration(k.seq) * time.Second),
		reason: reason, tree: map[string]*node{},
	}
	part := k.Part() + "/"
	for _, p := range k.sortedKeys() {
		rel, ok := strings.CutPrefix(p, part)
		if !ok {
			continue
		}
		n := k.nodes[p]
		if k.cfault[CopyIncomplete] && n.kind == kFile && strings.HasSuffix(p, "-1") {
			continue
		}
		if !k.mutants[CheckpointAliasesLive] {
			cp := *n
			k.ino++
			cp.ino = k.ino
			cp.nlink = 1
			n = &cp
		}
		ck.tree[rel] = n
		ck.entries++
		if n.kind == kFile {
			ck.bytes += int64(len(n.content))
		}
	}
	if prev := k.newest(); prev != nil && !k.mutants[ShrinkUnflagged] &&
		prev.entries > 1000 && ck.entries*2 <= prev.entries {
		ck.shrunk = prev.id
		prev.pinned = !k.mutants[ShrinkUnpinned]
	}
	k.ckpts = append(k.ckpts, ck)
	return ck
}

func (k *Checkpoints) newest() *checkpoint {
	for i := len(k.ckpts) - 1; i >= 0; i-- {
		if c := k.ckpts[i]; !c.pruned && !c.vanished {
			return c
		}
	}
	return nil
}

func (k *Checkpoints) find(id string) *checkpoint {
	for _, c := range k.ckpts {
		if c.id == id && !c.pruned && !c.vanished {
			return c
		}
	}
	return nil
}

func (k *Checkpoints) TakeCheckpoint(_ context.Context, p string) (conformance.CheckpointTaken, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfault[CreateFails] {
		return conformance.CheckpointTaken{}, errors.New("checkpoint: the store is locked")
	}
	if p != k.partPath() {
		return conformance.CheckpointTaken{}, fmt.Errorf("%s is not in [checkpoints] paths", p)
	}
	ck := k.take("manual")
	return conformance.CheckpointTaken{ID: ck.id, Shrunk: ck.shrunk}, nil
}

func (k *Checkpoints) List(context.Context) ([]conformance.CheckpointPath, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfault[ListFails] {
		return nil, errors.New("list: unreadable")
	}
	cp := conformance.CheckpointPath{Path: k.partPath(), Key: "work-conf-" + k.n8 + "-dev-part"}
	for i := len(k.ckpts) - 1; i >= 0; i-- {
		c := k.ckpts[i]
		if c.pruned || c.vanished {
			continue
		}
		cp.Checkpoints = append(cp.Checkpoints, conformance.CheckpointInfo{
			ID: c.id, Reason: c.reason, Shrunk: c.shrunk, Entries: c.entries, Bytes: c.bytes, Pinned: c.pinned})
	}
	if n := k.newest(); n != nil && n.shrunk != "" && k.find(n.shrunk) != nil {
		cp.Shrunk = n.shrunk
	}
	return []conformance.CheckpointPath{cp}, nil
}

func (k *Checkpoints) Tree(id string) (map[string]conformance.Identity, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfault[TreeUnreadable] {
		return nil, errors.New("tree: permission denied")
	}
	c := k.find(id)
	if c == nil {
		return nil, fmt.Errorf("no checkpoint %s", id)
	}
	out := map[string]conformance.Identity{}
	for p, n := range c.tree {
		out[p] = k.identity(n)
	}
	return out, nil
}

// unsafe says whether rel is where a sandbox can write, or in
// CABOOSE_HOME, and the warning the product gives.
func (k *Checkpoints) unsafe(rel string) (bool, string) {
	switch {
	case strings.HasPrefix(rel, CkptHome+"/"):
		return true, "<B>/" + rel + " is inside CABOOSE_HOME (<B>/home), where caboose keeps its own state"
	case k.zoneOf(rel) == zPart, strings.HasPrefix(rel, CkptDataKeep+"/"):
		return true, "the sandboxes of environment " + CkptEnv + " can write <B>/" + rel + ": such a sandbox could change the copy while it is written and made safe"
	}
	return false, ""
}

func (k *Checkpoints) Restore(_ context.Context, id string, to conformance.RestoreTarget) (conformance.RestoreOutcome, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	rel := to.Rel()
	k.Targets = append(k.Targets, rel)
	if cn, ok := k.canaryOf(to.Canary()); rel == "" || !ok || (rel != cn.Rel && cn.Kind != conformance.Dir) {
		k.Foreign = append(k.Foreign, rel)
		return conformance.RestoreOutcome{}, fmt.Errorf("%q is not a target of the suite's", rel)
	}
	if k.cfault[RestoreErrors] {
		return conformance.RestoreOutcome{}, errors.New("restore: the store is locked")
	}
	c := k.find(id)
	if c == nil {
		return conformance.RestoreOutcome{}, fmt.Errorf("no checkpoint %s", id)
	}
	exists := func() conformance.RestoreOutcome {
		return conformance.RestoreOutcome{Refused: conformance.RefusedExists,
			Message: "checkpoint: the target exists; restore writes only into a new directory: <B>/" + rel}
	}
	if k.mutants[RestoreRefused] || k.cfault[RestoreAsExists] {
		return exists(), nil
	}
	if n := k.nodes[rel]; n != nil {
		switch {
		case n.kind == kLink && k.mutants[RestoreFollowsLink]:
			if t, ok := k.hostRel(n.content); ok && k.nodes[t] == nil {
				k.write(c, t)
				return conformance.RestoreOutcome{}, nil
			}
		case k.mutants[RestoreInPlace]:
			k.write(c, rel)
			return conformance.RestoreOutcome{}, nil
		}
		return exists(), nil
	}
	if n := k.nodes[path.Dir(rel)]; n == nil || n.kind != kDir {
		return conformance.RestoreOutcome{}, fmt.Errorf("no directory <B>/%s", path.Dir(rel))
	}
	var out conformance.RestoreOutcome
	if bad, warn := k.unsafe(rel); bad {
		if k.cfault[UnsafeAsExisting] {
			return exists(), nil
		}
		switch {
		case k.mutants[RestoreUnsafeSilent]:
		case k.mutants[RestoreUnsafeWarned]:
			out.Warnings = append(out.Warnings, warn)
		default:
			return conformance.RestoreOutcome{Refused: conformance.RefusedUnsafe, Message: "checkpoint: restore may not write there: " + warn}, nil
		}
	}
	if k.cfault[ControlWarned] {
		out.Warnings = append(out.Warnings, "the sandboxes of environment "+CkptEnv+" can write <B>/"+rel)
	}
	k.take("before-restore")
	k.write(c, rel)
	return out, nil
}

// Discard removes what a restore wrote at to, a name inside a directory
// canary, as the guard does; Discarded records each, and DiscardFails
// makes it fail.
func (k *Checkpoints) Discard(to conformance.RestoreTarget) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	rel := to.Rel()
	if cn, ok := k.canaryOf(to.Canary()); rel == "" || !ok || rel == cn.Rel || cn.Kind != conformance.Dir {
		k.Foreign = append(k.Foreign, rel)
		return fmt.Errorf("%q is not a restore target inside a canary", rel)
	}
	if k.cfault[DiscardFails] {
		return errors.New("discard: permission denied")
	}
	k.Discarded = append(k.Discarded, rel)
	k.removeTree(rel)
	return nil
}

// write copies checkpoint c's tree to rel: into what is there with
// RestoreInPlace, as a new directory otherwise.
func (k *Checkpoints) write(c *checkpoint, rel string) {
	if k.nodes[rel] == nil || k.nodes[rel].kind != kDir {
		k.nodes[rel] = k.newNode(kDir, "")
		k.logMut("restore", rel)
	}
	keys := make([]string, 0, len(c.tree))
	for p := range c.tree {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		n := c.tree[p]
		if k.mutants[RestoreWrong] && n.kind == kFile && strings.HasSuffix(p, "-2") {
			continue
		}
		cp := k.newNode(n.kind, n.content)
		cp.mode = n.mode
		k.nodes[rel+"/"+p] = cp
		k.logMut("restore", rel+"/"+p)
	}
}

func (k *Checkpoints) Prune(_ context.Context, at time.Time, pinned []string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfault[PinnedVanishes] {
		for _, c := range k.ckpts {
			if slices.Contains(pinned, c.id) {
				c.vanished = true
			}
		}
	}
	if k.cfault[PruneNothing] {
		return nil, nil
	}
	var dropped []string
	newest := k.newest()
	for _, c := range k.ckpts {
		if c.pruned || c.vanished || at.Sub(c.stamp) < 7*24*time.Hour {
			continue
		}
		if c == newest && !k.mutants[PruneDropsNewest] {
			continue
		}
		if (c.pinned || slices.Contains(pinned, c.id)) && !k.mutants[PruneIgnoresPins] {
			continue
		}
		c.pruned = true
		dropped = append(dropped, c.id)
	}
	if k.mutants[PruneOutsideStore] {
		for _, cn := range k.canary {
			if path.Dir(cn.Rel) == CkptEnvDir && k.nodes[cn.Rel] != nil {
				delete(k.nodes, cn.Rel)
				k.logMut("prune", cn.Rel)
			}
		}
	}
	return dropped, nil
}
