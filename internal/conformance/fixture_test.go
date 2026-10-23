package conformance_test

import (
	"context"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

// The example checks: a few refusal checks over canaries, the wipe and a
// gap, defined here as the later pieces' checks will be.

const (
	isoContainer = "container"
	isoVM        = "vm"
)

var allRoles = []string{
	"ro-file", "wr-file", "parent-file", "wr-parent", "ro-tree", "wr-tree", "ro-dir", "wr-dir",
	"ro-link", "wr-link", "wr-link-s", "parent-um", "wr-um", "wr-um-s", "ro-wipe", "parent-wipe", "wr-wipe",
}

func refusals() []c.Check {
	appendOp := c.OpAppend
	return []c.Check{
		c.Refusal{ID: "F1", Title: "append to a file in a read-only root", Control: "roots are read-only", Step: "fixture", Group: "files",
			Class: c.Write, User: "", Target: "ro-file", Allowed: "wr-file", Attempt: c.OpAppend, Expect: []c.ErrClass{c.ClassErofs}}.Check(),
		c.Refusal{ID: "F2", Title: "append to a file beside the writable part", Control: "the part's parent is read-only", Step: "fixture", Group: "files",
			Class: c.Write, User: "", Target: "parent-file", Allowed: "wr-parent", Attempt: c.OpAppend, Expect: []c.ErrClass{c.ClassErofs}}.Check(),
		c.Refusal{ID: "F3", Title: "make a directory in a read-only root", Control: "roots are read-only", Step: "fixture", Group: "files",
			Class: c.Write, User: "root", Target: "ro-dir", Allowed: "wr-dir", Attempt: c.OpMkdirIn, Expect: []c.ErrClass{c.ClassErofs}}.Check(),
		c.Refusal{ID: "F4", Title: "write through a symbolic link", Control: "a link does not unprotect its target", Step: "fixture", Group: "files",
			Class: c.Write, User: "", Target: "ro-link", Allowed: "wr-link", Scratch: "wr-link-s", Attempt: c.OpAppendThrough, Expect: []c.ErrClass{c.ClassErofs}}.Check(),
		c.Refusal{ID: "F5", Title: "unmount the part, then write beside it", Control: "the part cannot be unmounted", Step: "fixture", Group: "files",
			Class: c.Destructive, User: "", Target: "parent-um", Allowed: "wr-um", Scratch: "wr-um-s", Attempt: c.OpUmountThenAppend, ControlOp: &appendOp,
			Expect: []c.ErrClass{c.ClassEperm, c.ClassEbusy}}.Check(),
		c.Refusal{ID: "F6", Title: "remove a tree in a read-only root", Control: "roots are read-only", Step: "fixture", Group: "files",
			Class: c.Destructive, User: "", Target: "ro-tree", Allowed: "wr-tree", Attempt: c.OpRemoveTree, Expect: []c.ErrClass{c.ClassErofs}}.Check(),
	}
}

func wipe() c.Check {
	return c.Check{ID: "F7", Title: "wipe as root", Control: "only the part is writable", Step: "fixture", Group: "files",
		Class: c.Destructive, Wipe: true, Roles: allRoles,
		Run: func(ctx context.Context, env c.Env) c.CheckResult {
			ro, err1 := env.Canary("ro-wipe")
			par, err2 := env.Canary("parent-wipe")
			wr, err3 := env.Canary("wr-wipe")
			if err1 != nil || err2 != nil || err3 != nil {
				return c.CheckResult{Status: c.Error, Reason: "a canary is missing"}
			}
			s1, _ := env.Snapshot(ro)
			s2, _ := env.Snapshot(par)
			w1, _ := env.Snapshot(wr)
			if _, err := env.Wipe(ctx); err != nil {
				return c.CheckResult{Status: c.Error, Reason: err.Error()}
			}
			a1, _ := env.Snapshot(ro)
			a2, _ := env.Snapshot(par)
			w2, _ := env.Snapshot(wr)
			if len(w2) != 0 || len(w1) == 0 {
				return c.CheckResult{Status: c.Error, Reason: "the wipe removed nothing: no positive control"}
			}
			if !same(s1, a1) || !same(s2, a2) {
				return c.CheckResult{Status: c.Fail, Reason: "a protected canary changed"}
			}
			return c.CheckResult{Status: c.Pass}
		}}
}

func same(a, b map[string]c.Identity) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if v != b[k] {
			return false
		}
	}
	return true
}

func gapCheck() c.Check {
	return c.Check{ID: "G1", Title: "outbound proxy", Control: "egress", Step: "fixture", Group: "net", Class: c.Observe,
		Isolations: []string{isoContainer, "gvisor"},
		Run: func(context.Context, c.Env) c.CheckResult {
			return c.CheckResult{Status: c.Gap, Reason: "not covered under this isolation"}
		}}
}

func vmOnly() c.Check {
	return c.Check{ID: "V1", Title: "vm only", Control: "vm", Step: "fixture", Group: "net", Class: c.Observe,
		Isolations: []string{isoVM},
		Run:        func(context.Context, c.Env) c.CheckResult { return c.CheckResult{Status: c.Pass} }}
}

func allChecks() []c.Check {
	return append(refusals(), wipe(), gapCheck(), vmOnly())
}

// setup makes the canaries of the example checks in a model.
func setup(f *fake.Fake) error {
	type m struct {
		parent, role string
		kind         c.Kind
	}
	for _, x := range []m{
		{f.RO(), "ro-file", c.File}, {f.Part(), "wr-file", c.File},
		{f.Dev(), "parent-file", c.File}, {f.Part(), "wr-parent", c.File},
		{f.RO(), "ro-dir", c.Dir}, {f.Part(), "wr-dir", c.Dir},
		{f.RO(), "ro-link", c.File}, {f.Part(), "wr-link", c.File}, {f.Part(), "wr-link-s", c.Dir},
		{f.Dev(), "parent-um", c.File}, {f.Part(), "wr-um", c.File}, {f.Part(), "wr-um-s", c.Dir},
		{f.RO(), "ro-tree", c.Tree}, {f.Part(), "wr-tree", c.Tree},
		{f.RO(), "ro-wipe", c.Tree}, {f.Dev(), "parent-wipe", c.File}, {f.Part(), "wr-wipe", c.Tree},
	} {
		if _, err := f.MakeCanary(x.parent, x.role, x.kind); err != nil {
			return err
		}
	}
	return nil
}

func newWorld(t testing.TB, iso string) (*fake.Fake, *fake.Envelope) {
	t.Helper()
	f := fake.New("0a1b2c3d")
	f.SetIsolation(iso)
	if err := setup(f); err != nil {
		t.Fatal(err)
	}
	return f, &fake.Envelope{F: f}
}

func runAll(t testing.TB, f *fake.Fake, env *fake.Envelope, checks []c.Check, iso string, wipeOn bool) ([]c.Result, *c.Collector, error) {
	t.Helper()
	r := &c.Runner{Env: env, Wipe: wipeOn}
	return c.RunChecks(context.Background(), r, checks, f.Env(iso, "fallible"))
}

func statusOf(rs []c.Result, id string) (c.Status, string) {
	for _, r := range rs {
		if r.ID == id {
			return r.Status, r.Reason
		}
	}
	return "", "absent"
}

var table = fake.Table{
	fake.RootsWritable:        {"F1", "F3", "F4", "F6", "F7"},
	fake.ParentWritable:       {"F2", "F7"},
	fake.SymlinkFollowed:      {"F4"},
	fake.UnmountWritesThrough: {"F5"},
}
