package conformance

import (
	"errors"
	"io/fs"
	"path"
	"testing"
)

func TestGuardHost(t *testing.T) {
	r := newRig(t, "container")
	h := &GuardHost{G: r.g}
	ro := r.g.RootNames()[0]
	parent := "roots/" + ro
	f, err := h.MakeCanary(parent, "ro-file", File)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := h.MakeCanary(parent, "ro-tree", Tree)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := h.MakeCanary(parent, "ro-dir", Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.Canary("ro-tree"); err != nil || got.Rel != tree.Rel {
		t.Errorf("Canary: %v %v", got, err)
	}
	if _, err := h.Canary("nope"); err == nil {
		t.Error("a canary that was never made")
	}
	if n := len(h.Mutations()); n != 1+len(tree.Paths)+1 {
		t.Errorf("%d mutations", n)
	}
	before, err := h.Snapshot(tree)
	if err != nil || len(before) != 9 { // 3 dirs and 6 files
		t.Fatalf("%d paths, %v", len(before), err)
	}
	fb, _ := h.Snapshot(f)
	db, _ := h.Snapshot(dir)
	if len(diffTrees(before, before)) != 0 {
		t.Error("a snapshot differs from itself")
	}
	// A change on the host shows; the sandbox's word is not asked.
	r.write(f.Rel, "changed\n")
	r.write(dir.Rel+"/new-file", "x")
	r.write(tree.Paths[len(tree.Paths)-1], "changed\n")
	fa, _ := h.Snapshot(f)
	da, _ := h.Snapshot(dir)
	ta, _ := h.Snapshot(tree)
	if len(diffTrees(fb, fa)) != 1 || len(diffTrees(db, da)) != 1 || len(diffTrees(before, ta)) != 1 {
		t.Errorf("diffs: %v %v %v", diffTrees(fb, fa), diffTrees(db, da), diffTrees(before, ta))
	}
	// A gone canary has an empty snapshot; Identity says not exist.
	if err := r.g.RemoveCanary(f); err != nil {
		t.Fatal(err)
	}
	underTemp(t, r.g.Base())
	if s, err := h.Snapshot(f); err != nil || len(s) != 0 {
		t.Errorf("gone: %v %v", s, err)
	}
	if _, err := h.Identity(f.Rel); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Identity of a gone canary: %v", err)
	}
	// Only canaries are read.
	if _, err := h.Identity("roots/" + ro); err == nil {
		t.Error("a root's identity was read")
	}
	if _, err := h.Snapshot(Canary{Rel: "roots/" + ro}); err == nil {
		t.Error("a non-canary was walked")
	}
	if _, err := (&GuardHost{}).Snapshot(f); err == nil {
		t.Error("a host with no guard")
	}
}

// The probe, which the suite places in the writable part beside the
// canaries and the daemon checks require in place, is read as a canary
// is; nothing else beside it is.
func TestGuardHostProbe(t *testing.T) {
	r := newRig(t, "container")
	h := &GuardHost{G: r.g}
	rel, err := ProbeRel(r.g.Nonce8())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Identity(rel); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Identity of a probe not placed: %v", err)
	}
	r.write(rel, "probe")
	if id, err := h.Identity(rel); err != nil || !id.Mode.IsRegular() {
		t.Errorf("Identity of the probe: %v %v", id, err)
	}
	other := path.Dir(rel) + "/other"
	r.write(other, "x")
	if _, err := h.Identity(other); err == nil {
		t.Error("a file beside the probe was read")
	}
	if _, err := h.Identity(rel + "x"); err == nil {
		t.Error("a name the probe's is a prefix of was read")
	}
	if _, err := h.ReadCanary(rel); err == nil {
		t.Error("the probe was read as a canary's contents")
	}
}

// The host's record of a canary is what it was made as, then what each
// post-check of a check it was handed to found; and it lists each canary
// once.
func TestGuardHostRecords(t *testing.T) {
	r := newRig(t, "container")
	h := &GuardHost{G: r.g}
	parent := "roots/" + r.g.RootNames()[0]
	f, err := h.MakeCanary(parent, "ro-file", File)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := h.MakeCanary(parent, "ro-tree", Tree)
	if err != nil {
		t.Fatal(err)
	}
	now, _ := h.Identity(f.Rel)
	id, gone, err := h.Recorded(f.Rel)
	if err != nil || gone || !SameIdentity(id, now) {
		t.Fatalf("%v %v %v", id, gone, err)
	}
	if _, _, err := h.Recorded(parent); err == nil {
		t.Error("a record of a path that is no canary")
	}
	if got := h.Canaries(); len(got) != 2 || got[0].Rel != f.Rel || got[1].Rel != tree.Rel {
		t.Errorf("Canaries: %v", got)
	}
}
