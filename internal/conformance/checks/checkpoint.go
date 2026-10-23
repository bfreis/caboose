package checks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	c "github.com/bfreis/caboose/internal/conformance"
)

// The checkpoint checks, C1-C6: the checkpoint store is out of the
// sandbox's reach, a checkpoint survives anything the sandbox does to the
// live tree, a restore brings it back and never writes over anything, a
// restore into a place a sandbox can write is refused or at least warned
// about, a mass deletion is noticed, and a prune removes nothing but
// checkpoints. Every checkpoint is of the suite's writable part, the
// configured path of the throwaway environment; every restore is into a
// target made from a canary of the run; every verdict is read on the host.

const (
	groupCheckpoints = "checkpoints"
	stepCheckpoints  = "checkpoints on the host"

	// The canaries of the checkpoint checks (CheckpointLayout).
	RoleStore   = "ckpt"      // a file in the store
	RoleSeen    = "ckpt-seen" // a file in the writable part: the scan's positive control
	RoleC2      = "c2-src"    // a tree in the part, damaged by C2
	RoleC3      = "c3-src"    // a tree in the part, damaged by C3 and restored
	RoleRestore = "restore"   // a directory outside every mount, restores land in it
	RoleRstDir  = "rst-dir"   // an existing directory, outside every mount
	RoleRstFile = "rst-file"  // an existing file, outside every mount
	RoleRstLink = "rst-link"  // a directory in the part where the sandbox makes a dangling link
	RoleRstPart = "rst-part"  // a directory in the part
	RoleRstHome = "rst-home"  // a directory in CABOOSE_HOME
	RoleRstData = "rst-data"  // a directory in a kept, mounted part of the data dir's home
	RoleBulk    = "bulk"      // a directory in the part, filled by the host
	RoleBeside  = "beside"    // a file in the environment's directory, beside the store

	// BulkFiles is how many files the bulk canary holds: well past the
	// product's shrink floor.
	BulkFiles = 2048
	// shrinkFloor is the product's: a checkpoint is flagged, and the one
	// before it pinned, when it holds half or fewer of the previous one's
	// entries, and that one held more than this many.
	shrinkFloor = 1000
	// pruneAhead is how far ahead C6's prune measures ages from: past the
	// product's longest retention band (7 days), so every checkpoint but
	// the newest and the pinned ones is due.
	pruneAhead = 8 * 24 * time.Hour

	// The product's restore warnings, as it words them.
	warnHome     = "CABOOSE_HOME"
	warnWritable = "can write"
)

// CheckpointDirs are where the checkpoint canaries go, relative to B.
type CheckpointDirs struct {
	// Part is the writable part (roots/conf-<n8>-dev/part), Canary the
	// host-only canary directory (canary), Store the environment's
	// checkpoint store (home/envs/<name>/checkpoints), EnvDir its
	// directory (home/envs/<name>), Home CABOOSE_HOME (home), and DataKeep
	// a directory of the data dir's home that the sandbox mounts
	// (data/home/.config/caboose).
	Part, Canary, Store, EnvDir, Home, DataKeep string
}

// CheckpointLayout lists the canaries of CheckpointChecks. Every directory
// must exist first; the store, and the configured path being the writable
// part, are the throwaway environment's setup.
func CheckpointLayout(d CheckpointDirs) ([]HostCanary, error) {
	for _, p := range []string{d.Part, d.Canary, d.Store, d.EnvDir, d.Home, d.DataKeep} {
		if p == "" || path.Clean(p) != p || path.IsAbs(p) || strings.HasPrefix(p, "..") {
			return nil, errors.New("checks: the checkpoint checks need every directory, relative to the base")
		}
	}
	if path.Dir(d.Store) != d.EnvDir || !strings.HasPrefix(d.EnvDir, d.Home+"/") {
		return nil, errors.New("checks: the store must be in the environment's directory, in CABOOSE_HOME")
	}
	return []HostCanary{
		{d.Store, RoleStore, c.File},
		{d.Part, RoleSeen, c.File},
		{d.Part, RoleC2, c.Tree},
		{d.Part, RoleC3, c.Tree},
		{d.Canary, RoleRestore, c.Dir},
		{d.Canary, RoleRstDir, c.Dir},
		{d.Canary, RoleRstFile, c.File},
		{d.Part, RoleRstLink, c.Dir},
		{d.Part, RoleRstPart, c.Dir},
		{d.Home, RoleRstHome, c.Dir},
		{d.DataKeep, RoleRstData, c.Dir},
		{d.Part, RoleBulk, c.Dir},
		{d.EnvDir, RoleBeside, c.File},
	}, nil
}

// CheckpointChecks are C1-C6 in the order they run. None applies to a run
// with no sandbox (hostile without vm).
func CheckpointChecks() []c.Check {
	return []c.Check{c1(), c2(), c3(), c4(), c5(), c6()}
}

func ckptCheck(id, title, control string, class c.Class, roles []string, run func(context.Context, *ckptRun) c.CheckResult) c.Check {
	return c.Check{
		ID: id, Title: title, Control: control, Step: stepCheckpoints, Group: groupCheckpoints,
		Class: class, Roles: roles,
		Run: func(ctx context.Context, env c.Env) c.CheckResult {
			r, bad := prepareCkpt(env, id, roles)
			if bad != nil {
				return *bad
			}
			return run(ctx, r)
		},
	}
}

// ckptRun is one checkpoint check's run.
type ckptRun struct {
	env c.Env
	ops c.CheckpointOps
	id  string
	// path is the configured path: the writable part, in the sandbox.
	path string
	// seen is the canary in the writable part.
	seen c.Canary
	can  map[string]c.Canary
}

// prepareCkpt is what every checkpoint check needs before it asks for
// anything: a running sandbox, the stubs in place (a checkpoint's
// warnings are notifications the daemon may send), the checkpoint calls,
// and its canaries on the host.
func prepareCkpt(env c.Env, id string, roles []string) (*ckptRun, *c.CheckResult) {
	bad := func(r c.CheckResult) (*ckptRun, *c.CheckResult) { return nil, &r }
	if st := env.State(); st != c.StateRunning {
		return bad(errorf("the sandbox is not running (%s)", st))
	}
	host, err := env.HostOps()
	if err != nil {
		return bad(errorf("%v", err))
	}
	if err := host.StubsInPlace(); err != nil {
		return bad(errorf("the suite's stub notifier is not in place, so nothing was asked: %v", err))
	}
	ops, err := env.CheckpointOps()
	if err != nil {
		return bad(errorf("%v", err))
	}
	r := &ckptRun{env: env, ops: ops, id: id, can: map[string]c.Canary{}}
	for _, role := range append([]string{RoleSeen}, roles...) {
		cn, err := env.Canary(role)
		if err != nil {
			return bad(errorf("canary %s is missing: %v", role, err))
		}
		if s, err := env.Snapshot(cn); err != nil || len(s) == 0 {
			return bad(errorf("canary %s is not on the host", role))
		}
		r.can[role] = cn
	}
	r.seen = r.can[RoleSeen]
	sp, err := c.SandboxPath(r.seen)
	if err != nil {
		return bad(errorf("%v", err))
	}
	mp, err := sp.MountPoint()
	if err != nil {
		return bad(errorf("canary %s is not in the writable part: %v", RoleSeen, err))
	}
	r.path = mp.String()
	return r, nil
}

// spath is where the sandbox sees the canary of role.
func (r *ckptRun) spath(role string) (c.SPath, *c.CheckResult) {
	p, err := c.SandboxPath(r.can[role])
	if err != nil {
		e := errorf("%v", err)
		return c.SPath{}, &e
	}
	return p, nil
}

// create takes a checkpoint of the configured path.
func (r *ckptRun) create(ctx context.Context) (c.CheckpointTaken, *c.CheckResult) {
	t, err := r.ops.TakeCheckpoint(ctx, r.path)
	if err == nil && t.ID == "" {
		err = errors.New("no ID")
	}
	if err != nil {
		e := errorf("a checkpoint of %s could not be taken: %v", r.path, err)
		return t, &e
	}
	return t, nil
}

// file is the i-th file (1 to 6) of tree canary cn, in the sandbox.
func treeFile(top c.SPath, cn c.Canary, i int) (c.SPath, error) {
	name := path.Base(cn.Rel)
	if i <= 2 {
		return top.Child(fmt.Sprintf("%s-%d", name, i))
	}
	a, err := top.Child(name + "-a")
	if err != nil {
		return c.SPath{}, err
	}
	if i <= 4 {
		return a.Child(fmt.Sprintf("%s-%d", name, i))
	}
	b, err := a.Child(name + "-b")
	if err != nil {
		return c.SPath{}, err
	}
	return b.Child(fmt.Sprintf("%s-%d", name, i))
}

// damage does from the sandbox what a session gone wrong would to the tree
// canary of role: changes a file, empties one, deletes one and makes
// another in its place, renames one, then removes the whole tree. Each step
// must work (the canary is the sandbox's to damage), and the tree must be
// gone on the host after: else the check proves nothing.
func (r *ckptRun) damage(ctx context.Context, role string) *c.CheckResult {
	top, bad := r.spath(role)
	if bad != nil {
		return bad
	}
	cn := r.can[role]
	f := func(i int) c.SPath { p, _ := treeFile(top, cn, i); return p }
	steps := []struct {
		op c.Op
		p  c.SPath
	}{
		{c.OpAppend, f(1)}, {c.OpTruncate, f(2)}, {c.OpRemove, f(3)}, {c.OpTouch, f(3)},
		{c.OpMoveIn, top}, {c.OpRemoveTree, top},
	}
	for _, s := range steps {
		cmd, err := s.op.Build(s.p, c.SPath{})
		if err != nil {
			e := errorf("%v", err)
			return &e
		}
		res, err := r.env.RunCmd(ctx, r.env.SessionUser(), cmd)
		if err != nil {
			e := errorf("the damage (%s) could not be run: %v", s.op.Name, err)
			return &e
		}
		if res.Class != c.ClassOK {
			e := errorf("the damage (%s) failed in the writable part: %s%s", s.op.Name, res.Class, msgSuffix(res))
			return &e
		}
	}
	after, err := r.env.Snapshot(cn)
	if err != nil {
		e := errorf("canary %s could not be read back: %v", role, err)
		return &e
	}
	if len(after) != 0 {
		e := errorf("the sandbox said it removed canary %s, and it is still on the host", role)
		return &e
	}
	return nil
}

func msgSuffix(r c.ExecResult) string {
	if r.Msg == "" {
		return ""
	}
	return ": " + r.Msg
}

// content is what a restore must give back of a path: its type,
// permissions, size and contents' hash. Inode, device, link count and
// times are a copy's own.
type content struct {
	typ, perm fs.FileMode
	size      int64
	hash      string
}

func contentOf(id c.Identity) content {
	ct := content{typ: id.Mode.Type(), perm: id.Mode.Perm(), hash: id.Hash}
	if !id.Mode.IsDir() {
		ct.size = id.Size
	}
	return ct
}

// under is the content of top and everything below it in snap (keyed by
// a path relative to B, or to a checkpoint's top), by the path relative to
// top ("." for top itself).
func under(snap map[string]c.Identity, top string) map[string]content {
	out := map[string]content{}
	for p, id := range snap {
		switch {
		case p == top:
			out["."] = contentOf(id)
		case strings.HasPrefix(p, top+"/"):
			out[p[len(top)+1:]] = contentOf(id)
		}
	}
	return out
}

// diffContent lists the paths whose content differs between a and b.
func diffContent(a, b map[string]content) []string {
	var out []string
	for p, x := range a {
		if y, ok := b[p]; !ok || x != y {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// sameFile compares two identities of one path of a checkpoint, which is
// never written again: everything but the link count (a later checkpoint
// may hard-link an unchanged file from it) and, for a directory, its size
// and times.
func sameFile(a, b c.Identity) bool {
	if a.Mode.IsDir() && b.Mode.IsDir() {
		return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode
	}
	a.Nlink, b.Nlink = 0, 0
	return a == b
}

func diffIdentity(a, b map[string]c.Identity) []string {
	var out []string
	for p, x := range a {
		if y, ok := b[p]; !ok || !sameFile(x, y) {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

func short(paths []string) string {
	if len(paths) > 5 {
		return strings.Join(paths[:5], ", ") + fmt.Sprintf(" and %d more", len(paths)-5)
	}
	return strings.Join(paths, ", ")
}

// holds checks that checkpoint t's copy holds the canary of role as it is
// on the host now: the checkpoint copies the canary at all.
func (r *ckptRun) holds(t c.CheckpointTaken, role string) (map[string]c.Identity, *c.CheckResult) {
	tree, err := r.ops.Tree(t.ID)
	if err != nil {
		e := errorf("checkpoint %s could not be read on the host: %v", t.ID, err)
		return nil, &e
	}
	live, err := r.env.Snapshot(r.can[role])
	if err != nil {
		e := errorf("canary %s could not be read: %v", role, err)
		return nil, &e
	}
	want := under(live, r.can[role].Rel)
	got := under(tree, path.Base(r.can[role].Rel))
	if d := diffContent(want, got); len(d) > 0 {
		e := errorf("checkpoint %s does not hold canary %s as it is (%s): the copy cannot be judged", t.ID, role, short(d))
		return nil, &e
	}
	return tree, nil
}

// C1: the store is not reachable from the sandbox.
func c1() c.Check {
	return ckptCheck("C1", "the checkpoint store is not reachable from the sandbox",
		"no sandbox mounts or sees the checkpoint store", c.Observe, []string{RoleStore},
		func(ctx context.Context, r *ckptRun) c.CheckResult {
			store, err := r.ops.StorePath()
			if err == nil && store == "" {
				err = errors.New("empty")
			}
			if err != nil {
				return errorf("the store's host path is not known: %v", err)
			}
			store = path.Clean(store)
			// The mounts, as the sandbox was made with them.
			listed := false
			var reach []string
			for _, m := range r.env.Mounts() {
				if m.Target == r.path {
					listed = true
				}
				if m.Source == "" {
					continue
				}
				src := path.Clean(m.Source)
				if within(store, src) || within(src, store) {
					reach = append(reach, m.Target)
				}
			}
			if len(reach) > 0 {
				return failf("the sandbox mounts the store, or a directory around or in it, at %s", short(reach))
			}
			if !listed {
				return errorf("the sandbox's mounts do not list the writable part at %s: the list cannot be judged", r.path)
			}
			// The scan: by name, everywhere the sandbox sees.
			storeName, seenName := path.Base(r.can[RoleStore].Rel), path.Base(r.seen.Rel)
			seenAt, bad := r.spath(RoleSeen)
			if bad != nil {
				return *bad
			}
			cmd, err := c.FindNames(storeName, seenName)
			if err != nil {
				return errorf("%v", err)
			}
			res, err := r.env.RunCmd(ctx, "root", cmd)
			if err != nil {
				return errorf("the scan could not be run: %v", err)
			}
			// find exits 1 when some directory could not be read.
			if res.RC != 0 && res.RC != 1 {
				return errorf("the scan ended with status %d%s", res.RC, msgSuffix(res))
			}
			seen := false
			var found []string
			for _, l := range strings.Split(res.Out, "\n") {
				l = strings.TrimSpace(l)
				if !strings.HasPrefix(l, "/") {
					continue
				}
				switch path.Base(l) {
				case storeName:
					found = append(found, l)
				case seenName:
					seen = seen || l == seenAt.String()
				}
			}
			if len(found) > 0 {
				return failf("the sandbox sees the store's canary at %s", short(found))
			}
			if !seen {
				return errorf("the scan did not find the canary in the writable part: it proves nothing")
			}
			return c.CheckResult{Status: c.Pass, Lines: []string{"scanned by name only; the scan by content is the daemon checks'"}}
		})
}

// C2: a checkpoint cannot be changed from the sandbox.
func c2() c.Check {
	return ckptCheck("C2", "a checkpoint cannot be changed from the sandbox",
		"checkpoints are copies the sandbox cannot reach", c.Destructive, []string{RoleC2},
		func(ctx context.Context, r *ckptRun) c.CheckResult {
			t, bad := r.create(ctx)
			if bad != nil {
				return *bad
			}
			before, bad := r.holds(t, RoleC2)
			if bad != nil {
				return *bad
			}
			if bad := r.damage(ctx, RoleC2); bad != nil {
				return *bad
			}
			after, err := r.ops.Tree(t.ID)
			if err != nil {
				return errorf("checkpoint %s could not be read again: %v", t.ID, err)
			}
			if d := diffIdentity(before, after); len(d) > 0 {
				return failf("checkpoint %s changed when the sandbox damaged the live tree: %s", t.ID, short(d))
			}
			return c.CheckResult{Status: c.Pass}
		})
}

// C3: recovery works, into a new directory only.
func c3() c.Check {
	return ckptCheck("C3", "a restore brings a checkpoint back, never over anything",
		"restore writes only into a new directory", c.Destructive,
		[]string{RoleC3, RoleRestore, RoleRstDir, RoleRstFile, RoleRstLink},
		func(ctx context.Context, r *ckptRun) c.CheckResult {
			src := r.can[RoleC3]
			pre, err := r.env.Snapshot(src)
			if err != nil {
				return errorf("canary %s could not be read: %v", RoleC3, err)
			}
			want := under(pre, src.Rel)
			t, bad := r.create(ctx)
			if bad != nil {
				return *bad
			}
			if _, bad := r.holds(t, RoleC3); bad != nil {
				return *bad
			}
			// A dangling link, made by the sandbox where it may.
			linkDir, bad := r.spath(RoleRstLink)
			if bad != nil {
				return *bad
			}
			cmd, err := c.OpSymlinkIn.Build(linkDir, c.SPath{})
			if err != nil {
				return errorf("%v", err)
			}
			if res, err := r.env.RunCmd(ctx, r.env.SessionUser(), cmd); err != nil || res.Class != c.ClassOK {
				return errorf("the sandbox could not make a link in the writable part: %v%s", err, msgSuffix(res))
			}
			link := r.can[RoleRstLink].Rel + "/new-link"
			if id, err := r.env.Identity(link); err != nil || id.Mode&fs.ModeSymlink == 0 {
				return errorf("the link the sandbox made is not on the host: %v", err)
			}
			// Every existing target is refused, and stays as it was.
			at := func(role string) (c.RestoreTarget, error) { return c.TargetAt(r.can[role]) }
			inLink := func(string) (c.RestoreTarget, error) { return c.TargetIn(r.can[RoleRstLink], "new-link") }
			for _, x := range []struct {
				what, role string
				target     func(string) (c.RestoreTarget, error)
			}{
				{"the live tree (in place)", RoleC3, at},
				{"an existing directory", RoleRstDir, at},
				{"an existing file", RoleRstFile, at},
				{"a dangling symbolic link", RoleRstLink, inLink},
			} {
				tg, err := x.target(x.role)
				if err != nil {
					return errorf("%v", err)
				}
				b, err := r.env.Snapshot(r.can[x.role])
				if err != nil {
					return errorf("canary %s could not be read: %v", x.role, err)
				}
				out, rerr := r.ops.Restore(ctx, t.ID, tg)
				a, err := r.env.Snapshot(r.can[x.role])
				if err != nil {
					return errorf("canary %s could not be read back: %v", x.role, err)
				}
				if d := diffIdentity(b, a); len(d) > 0 {
					return failf("a restore to %s wrote over it: %s", x.what, short(describeRel(d)))
				}
				switch {
				case rerr != nil:
					return errorf("the restore to %s could not be run: %v", x.what, rerr)
				case out.Refused == c.RestoreDone:
					return failf("a restore to %s was not refused", x.what)
				}
			}
			// Damage the live tree, then restore into a new directory.
			if bad := r.damage(ctx, RoleC3); bad != nil {
				return *bad
			}
			tg, err := c.TargetIn(r.can[RoleRestore], "c3-new")
			if err != nil {
				return errorf("%v", err)
			}
			b, err := r.env.Snapshot(r.can[RoleRestore])
			if err != nil {
				return errorf("canary %s could not be read: %v", RoleRestore, err)
			}
			out, err := r.ops.Restore(ctx, t.ID, tg)
			if err != nil {
				return errorf("the restore could not be run: %v", err)
			}
			if out.Refused != c.RestoreDone {
				return failf("a restore into a new directory was refused (%s): %s", out.Refused, out.Message)
			}
			a, err := r.env.Snapshot(r.can[RoleRestore])
			if err != nil {
				return errorf("canary %s could not be read back: %v", RoleRestore, err)
			}
			for p := range a {
				if _, old := b[p]; !old && p != tg.Rel() && !strings.HasPrefix(p, tg.Rel()+"/") {
					return failf("the restore wrote outside its target: <B>/%s", p)
				}
			}
			got := under(a, tg.Rel()+"/"+path.Base(src.Rel))
			if d := diffContent(want, got); len(d) > 0 {
				return failf("the restored tree differs from the canary before the damage: %s", short(d))
			}
			return c.CheckResult{Status: c.Pass, Lines: []string{"a target inside the writable part is C4's"}}
		})
}

// within reports whether a is b or below it, by their cleaned paths.
func within(a, b string) bool {
	return a == b || b == "/" || strings.HasPrefix(a, b+"/")
}

func describeRel(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "<B>/" + p
	}
	return out
}

// C4: a restore is not written where the sandbox can alter it.
func c4() c.Check {
	return ckptCheck("C4", "a restore is not written where a sandbox can change it",
		"restore targets a sandbox can write are refused", c.Write,
		[]string{RoleRestore, RoleRstPart, RoleRstHome, RoleRstData},
		func(ctx context.Context, r *ckptRun) c.CheckResult {
			// Every target asked for is discarded once the verdict is read,
			// whatever it is: a copy of the writable part left in the data
			// dir's kept home is the run's names and nonce where the sandbox
			// sees them, which the scans of D0 and D1 are right to report.
			var asked []c.RestoreTarget
			res := c4Run(ctx, r, func(tg c.RestoreTarget) { asked = append(asked, tg) })
			var left []string
			for _, tg := range asked {
				if err := r.ops.Discard(tg); err != nil {
					left = append(left, "<B>/"+tg.Rel()+": "+err.Error())
				}
			}
			switch {
			case len(left) > 0 && res.Status == c.Fail:
				// A failure stands; what is left is said beside it.
				res.Lines = append(res.Lines, "not removed, for the checks after this one to find: "+strings.Join(left, "; "))
			case len(left) > 0:
				return errorf("what the restores wrote could not be removed, and the checks after this one would find it: %s (the verdict was %s %s)",
					strings.Join(left, "; "), res.Status, res.Reason)
			}
			return res
		})
}

// c4Run is C4 itself; asked is told of every restore target before the
// restore into it is asked for.
func c4Run(ctx context.Context, r *ckptRun, asked func(c.RestoreTarget)) c.CheckResult {
	t, bad := r.create(ctx)
	if bad != nil {
		return *bad
	}
	restore := func(role, name string) (c.RestoreTarget, c.RestoreOutcome, map[string]c.Identity, *c.CheckResult) {
		tg, err := c.TargetIn(r.can[role], name)
		if err != nil {
			e := errorf("%v", err)
			return tg, c.RestoreOutcome{}, nil, &e
		}
		asked(tg)
		out, err := r.ops.Restore(ctx, t.ID, tg)
		if err != nil {
			e := errorf("the restore into canary %s could not be run: %v", role, err)
			return tg, out, nil, &e
		}
		a, err := r.env.Snapshot(r.can[role])
		if err != nil {
			e := errorf("canary %s could not be read back: %v", role, err)
			return tg, out, nil, &e
		}
		return tg, out, a, nil
	}
	warned := func(out c.RestoreOutcome, word string) bool {
		for _, w := range out.Warnings {
			if strings.Contains(w, word) {
				return true
			}
		}
		return false
	}
	// The positive control: a target no sandbox reaches is
	// restored, unwarned.
	tg, out, a, bad := restore(RoleRestore, "c4-control")
	switch {
	case bad != nil:
		return *bad
	case out.Refused != c.RestoreDone:
		return errorf("positive control failed: a restore outside every mount was refused (%s): %s", out.Refused, out.Message)
	case len(under(a, tg.Rel())) == 0:
		return errorf("positive control failed: the restore outside every mount is not on the host")
	case warned(out, warnHome) || warned(out, warnWritable):
		return errorf("positive control failed: a restore outside every mount was warned about too, so a warning tells nothing")
	}
	var fails []string
	for _, x := range []struct{ what, role, word string }{
		{"the writable part", RoleRstPart, warnWritable},
		{"CABOOSE_HOME", RoleRstHome, warnHome},
		{"a mounted part of the data dir", RoleRstData, warnWritable},
	} {
		tg, out, a, bad := restore(x.role, "c4-target")
		if bad != nil {
			return *bad
		}
		landed := len(under(a, tg.Rel())) > 0
		switch {
		case out.Refused == c.RefusedUnsafe && !landed:
		case out.Refused == c.RefusedUnsafe:
			fails = append(fails, x.what+": refused, and written anyway")
		case out.Refused != c.RestoreDone:
			return errorf("the restore into %s was refused as %s, not as a target a sandbox can write: %s", x.what, out.Refused, out.Message)
		case !landed:
			return errorf("the restore into %s was not refused and is not on the host", x.what)
		case warned(out, x.word):
			fails = append(fails, x.what+": written, only warned")
		default:
			fails = append(fails, x.what+": written with no warning")
		}
	}
	if len(fails) > 0 {
		return failf("a restore into a place a sandbox can write: %s", strings.Join(fails, "; "))
	}
	return c.CheckResult{Status: c.Pass}
}

// C5: a mass deletion is noticed.
func c5() c.Check {
	return ckptCheck("C5", "a mass deletion in a checkpointed path is noticed",
		"a sharp shrink is flagged, naming the checkpoint to restore", c.Destructive, []string{RoleBulk},
		func(ctx context.Context, r *ckptRun) c.CheckResult {
			bulk := r.can[RoleBulk]
			if err := r.ops.Populate(bulk, BulkFiles); err != nil {
				return errorf("canary %s could not be filled: %v", RoleBulk, err)
			}
			if s, err := r.env.Snapshot(bulk); err != nil || len(s) < BulkFiles {
				return errorf("canary %s holds %d entries, not %d", RoleBulk, len(s), BulkFiles)
			}
			prev, bad := r.create(ctx)
			if bad != nil {
				return *bad
			}
			top, bad := r.spath(RoleBulk)
			if bad != nil {
				return *bad
			}
			cmd, err := c.OpRemoveTree.Build(top, c.SPath{})
			if err != nil {
				return errorf("%v", err)
			}
			if res, err := r.env.RunCmd(ctx, r.env.SessionUser(), cmd); err != nil || res.Class != c.ClassOK {
				return errorf("the sandbox could not empty canary %s: %v%s", RoleBulk, err, msgSuffix(res))
			}
			if s, err := r.env.Snapshot(bulk); err != nil || len(s) != 0 {
				return errorf("the sandbox said it emptied canary %s, and it is still on the host", RoleBulk)
			}
			next, bad := r.create(ctx)
			if bad != nil {
				return *bad
			}
			paths, err := r.ops.List(ctx)
			if err != nil {
				return errorf("the checkpoints could not be listed: %v", err)
			}
			var listed *c.CheckpointPath
			for i := range paths {
				if paths[i].Path == r.path {
					listed = &paths[i]
				}
			}
			if listed == nil {
				return errorf("the listing has no %s", r.path)
			}
			var p, n *c.CheckpointInfo
			for i := range listed.Checkpoints {
				switch listed.Checkpoints[i].ID {
				case prev.ID:
					p = &listed.Checkpoints[i]
				case next.ID:
					n = &listed.Checkpoints[i]
				}
			}
			switch {
			case n == nil:
				return errorf("the listing does not show checkpoint %s", next.ID)
			case p == nil:
				return failf("checkpoint %s, the one to restore from, is gone", prev.ID)
			case p.Entries <= shrinkFloor || n.Entries*2 > p.Entries:
				return errorf("the path went from %d to %d entries, not a shrink the product flags: the check proves nothing", p.Entries, n.Entries)
			}
			var missing []string
			if n.Shrunk != prev.ID {
				missing = append(missing, "the checkpoint's record")
			}
			if listed.Shrunk != prev.ID {
				missing = append(missing, "the listing's flag")
			}
			if next.Shrunk != prev.ID {
				missing = append(missing, "the create's warning")
			}
			if len(missing) > 0 {
				return failf("the shrink from %d to %d entries was not flagged naming %s in %s", p.Entries, n.Entries, prev.ID, strings.Join(missing, ", "))
			}
			if !p.Pinned {
				return failf("the shrink from %d to %d entries was flagged, but %s, the one to restore from, is not pinned against a later prune", p.Entries, n.Entries, prev.ID)
			}
			return c.CheckResult{Status: c.Pass, Lines: []string{
				"the earlier checkpoint is pinned, as listed: C6's prune does not test that pin",
				"the daemon's notification is not waited for: its schedule takes it, not the create",
			}}
		})
}

// C6: retention and removal reach nothing but checkpoints.
func c6() c.Check {
	return ckptCheck("C6", "a prune removes nothing but unpinned checkpoints",
		"retention and removal go through the guarded remover", c.Destructive, []string{RoleBeside, RoleStore},
		func(ctx context.Context, r *ckptRun) c.CheckResult {
			ids := map[string]c.Identity{}
			for _, role := range []string{RoleBeside, RoleStore} {
				id, err := r.env.Identity(r.can[role].Rel)
				if err != nil {
					return errorf("canary %s could not be read: %v", role, err)
				}
				ids[role] = id
			}
			// Three checkpoints: the oldest is due, the middle one pinned, the
			// newest kept by the rule that always keeps the newest.
			due, bad := r.create(ctx)
			if bad != nil {
				return *bad
			}
			pinned, bad := r.create(ctx)
			if bad != nil {
				return *bad
			}
			newest, bad := r.create(ctx)
			if bad != nil {
				return *bad
			}
			if due.ID == pinned.ID || pinned.ID == newest.ID || due.ID == newest.ID {
				return errorf("two checkpoints were given one ID")
			}
			dropped, err := r.ops.Prune(ctx, r.env.Now().Add(pruneAhead), []string{pinned.ID})
			if err != nil {
				return errorf("the prune could not be run: %v", err)
			}
			var changed []string
			for _, role := range []string{RoleBeside, RoleStore} {
				id, err := r.env.Identity(r.can[role].Rel)
				if err != nil || id != ids[role] {
					changed = append(changed, role)
				}
			}
			if len(changed) > 0 {
				return failf("the prune changed or removed canary %s, beside the checkpoints", strings.Join(changed, ", "))
			}
			if slices.Contains(dropped, pinned.ID) {
				return failf("the prune dropped the pinned checkpoint %s", pinned.ID)
			}
			paths, err := r.ops.List(ctx)
			if err != nil {
				return errorf("the checkpoints could not be listed: %v", err)
			}
			have := map[string]bool{}
			for _, p := range paths {
				for _, ck := range p.Checkpoints {
					have[ck.ID] = true
				}
			}
			switch {
			case !slices.Contains(dropped, due.ID) || have[due.ID]:
				return errorf("positive control failed: the prune did not remove checkpoint %s, past every retention band", due.ID)
			case !have[pinned.ID]:
				return errorf("the pinned checkpoint %s is gone, but the prune says it did not drop it: something else removed it", pinned.ID)
			case slices.Contains(dropped, newest.ID) || !have[newest.ID]:
				return failf("the prune dropped the newest checkpoint %s, which is always kept", newest.ID)
			}
			return c.CheckResult{Status: c.Pass, Lines: []string{"the product has no command that removes a whole store: prune is what removes"}}
		})
}
