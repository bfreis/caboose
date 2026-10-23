package conformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreTargets(t *testing.T) {
	dir := Canary{Role: "restore", Kind: Dir, Rel: "canary/conf-0a1b2c3d-restore"}
	file := Canary{Role: "rst-file", Kind: File, Rel: "canary/conf-0a1b2c3d-rst-file"}
	in, err := TargetIn(dir, "c3-new")
	if err != nil || in.Rel() != dir.Rel+"/c3-new" || in.Canary() != dir.Rel {
		t.Fatalf("TargetIn: %+v %v", in, err)
	}
	at, err := TargetAt(file)
	if err != nil || at.Rel() != file.Rel {
		t.Fatalf("TargetAt: %+v %v", at, err)
	}
	if (RestoreTarget{}).Rel() != "" {
		t.Error("a zero target has a path")
	}
	for _, c := range []Canary{
		{Kind: Dir, Rel: ""}, {Kind: Dir, Rel: "/tmp/conf-0a1b2c3d-x"}, {Kind: Dir, Rel: "../conf-0a1b2c3d-x"},
		{Kind: Dir, Rel: "canary/../conf-0a1b2c3d-x"}, {Kind: Dir, Rel: "home/envs"}, {Kind: Dir, Rel: "canary/x"},
		{Kind: Dir, Rel: "canary/conf-0a1b2c3d-X"},
	} {
		if _, err := TargetAt(c); err == nil {
			t.Errorf("TargetAt(%q) accepted", c.Rel)
		}
		if _, err := TargetIn(c, "x"); err == nil {
			t.Errorf("TargetIn(%q) accepted", c.Rel)
		}
	}
	if _, err := TargetIn(file, "x"); err == nil {
		t.Error("a name inside a file canary")
	}
	for _, n := range []string{"", ".", "..", "a/b", "A", "x y", "-x", strings.Repeat("a", 33)} {
		if _, err := TargetIn(dir, n); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
}

func TestGuardRestorePath(t *testing.T) {
	r := newRig(t, "container")
	h := &GuardHost{G: r.g}
	dir, err := h.MakeCanary("canary", "restore", Dir)
	if err != nil {
		t.Fatal(err)
	}
	file, err := h.MakeCanary("canary", "rst-file", File)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := TargetIn(dir, "c3-new")
	p, err := r.g.RestorePath(tg)
	if err != nil || p != filepath.Join(r.g.Base(), "canary", r.g.CanaryName("restore"), "c3-new") {
		t.Errorf("RestorePath: %s %v", p, err)
	}
	at, _ := TargetAt(file)
	if p, err := r.g.RestorePath(at); err != nil || p != r.path(file.Rel) {
		t.Errorf("RestorePath of a file canary: %s %v", p, err)
	}
	// A canary this guard never made, by any spelling, is refused.
	forged := Canary{Role: "restore", Kind: Dir, Rel: "home/" + r.g.CanaryName("restore")}
	other := Canary{Role: "x", Kind: Dir, Rel: "canary/conf-ffffffff-restore"}
	asDir := Canary{Role: "rst-file", Kind: Dir, Rel: file.Rel}
	for _, c := range []Canary{forged, other, asDir} {
		tg, err := TargetIn(c, "x")
		if err != nil {
			t.Fatal(err)
		}
		if p, err := r.g.RestorePath(tg); err == nil {
			t.Errorf("%s accepted as %s", c.Rel, p)
		}
	}
	// A canary's spelling it was never made at, as the target itself.
	for _, c := range []Canary{
		{Kind: File, Rel: "home/" + r.g.CanaryName("rst-file")},
		{Kind: Dir, Rel: "canary/" + r.g.CanaryName("nosuch")},
		{Kind: File, Rel: "canary/conf-ffffffff-rst-file"},
	} {
		tg, err := TargetAt(c)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := r.g.RestorePath(tg); err == nil {
			t.Errorf("TargetAt(%s) accepted as %s", c.Rel, p)
		}
	}
	if _, err := r.g.RestorePath(RestoreTarget{}); err == nil {
		t.Error("a zero target accepted")
	}
	if _, err := (&Guard{}).RestorePath(tg); err == nil {
		t.Error("a guard Setup did not make")
	}
	var nilGuard *Guard
	if _, err := nilGuard.RestorePath(tg); err == nil {
		t.Error("a nil guard")
	}
}

func TestFindNames(t *testing.T) {
	cmd, err := FindNames("conf-0a1b2c3d-ckpt", "conf-0a1b2c3d-ckpt-seen")
	if err != nil {
		t.Fatal(err)
	}
	want := ScanPrefix + "-path /proc -prune -o -path /sys -prune -o -path /dev -prune -o -name 'conf-0a1b2c3d-ckpt' -print -o -name 'conf-0a1b2c3d-ckpt-seen' -print 1>&2"
	if cmd.Body() != want {
		t.Errorf("body %q", cmd.Body())
	}
	if cmd.Destructive() {
		t.Error("the scan is destructive")
	}
	if strings.ContainsAny(cmd.Body(), "$`~*?[") {
		t.Errorf("expansion or glob in %q", cmd.Body())
	}
	if out, err := exec.Command("sh", "-n", "-c", cmd.Argv()[2]).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v %s", err, out)
	}
	for _, bad := range [][]string{nil, {"x"}, {"conf-0a1b2c3d-*"}, {"conf-0a1b2c3d-a'b"}, {"conf-0a1b2c3d-a b"},
		{"conf-0a1b2c3d-$x"}, {"/etc/passwd"}, make([]string, 9)} {
		if _, err := FindNames(bad...); err == nil {
			t.Errorf("FindNames(%q) accepted", bad)
		}
	}
}

func TestCheckpointOpsNeedTheEnvelope(t *testing.T) {
	r := newRig(t, "container")
	env := NewEnv(nil, &GuardHost{G: r.g}, nil, nil, "container", "fallible")
	if _, err := env.CheckpointOps(); !errors.Is(err, ErrNoEnvelope) {
		t.Errorf("outside the envelope: %v", err)
	}
	env.seal = &seal{id: "C1"}
	if _, err := env.CheckpointOps(); err == nil {
		t.Error("a host without the calls offered them")
	}
}

func TestGuardDiscardRestore(t *testing.T) {
	r := newRig(t, "container")
	h := &GuardHost{G: r.g}
	dir, err := h.MakeCanary("canary", "restore", Dir)
	if err != nil {
		t.Fatal(err)
	}
	file, err := h.MakeCanary("canary", "rst-file", File)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := h.MakeCanary("canary", "outside", File)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := TargetIn(dir, "c4-target")
	// What a restore wrote, read-only directories among it, is removed.
	must(t, os.MkdirAll(r.path(tg.Rel()+"/a/b"), 0o755))
	r.write(tg.Rel()+"/a/b/f", "x")
	must(t, os.Chmod(r.path(tg.Rel()+"/a"), 0o555))
	if err := r.g.DiscardRestore(tg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(r.path(tg.Rel())); !os.IsNotExist(err) {
		t.Errorf("the restore's copy is still there: %v", err)
	}
	if _, err := os.Lstat(r.path(dir.Rel)); err != nil {
		t.Errorf("the canary that held it went too: %v", err)
	}
	// Nothing there is not an error.
	if err := r.g.DiscardRestore(tg); err != nil {
		t.Errorf("a second discard: %v", err)
	}
	// A link at the target is unlinked, never followed.
	must(t, os.Symlink(r.path(outside.Rel), r.path(tg.Rel())))
	if err := r.g.DiscardRestore(tg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(r.path(outside.Rel)); err != nil {
		t.Errorf("the link's target was removed: %v", err)
	}
	// A canary itself, a file canary and one never made are refused.
	at, _ := TargetAt(dir)
	inFile, _ := TargetIn(Canary{Role: "rst-file", Kind: Dir, Rel: file.Rel}, "x")
	forged, _ := TargetIn(Canary{Role: "x", Kind: Dir, Rel: "canary/conf-ffffffff-restore"}, "x")
	for _, bad := range []RestoreTarget{at, inFile, forged, {}} {
		if err := r.g.DiscardRestore(bad); err == nil {
			t.Errorf("%q accepted", bad.Rel())
		}
	}
	// A canary the sandbox swapped for a link to another directory is
	// refused: the removal would land where the link points.
	must(t, os.Rename(r.path(dir.Rel), r.path("canary/moved")))
	must(t, os.Mkdir(r.path("canary/elsewhere"), 0o755))
	r.write("canary/elsewhere/c4-target", "keep")
	must(t, os.Symlink(r.path("canary/elsewhere"), r.path(dir.Rel)))
	if err := r.g.DiscardRestore(tg); err == nil {
		t.Error("a canary replaced by a link accepted")
	}
	if _, err := os.Lstat(r.path("canary/elsewhere/c4-target")); err != nil {
		t.Errorf("removed through the link: %v", err)
	}
	if err := (&Guard{}).DiscardRestore(tg); err == nil {
		t.Error("a guard Setup did not make")
	}
}

// The canary sits where a sandbox can write, so it can be swapped for a
// link between the guard's check and the removal: the removal is of the
// directory the guard checked and holds, never of where the link points.
func TestGuardDiscardRestoreRace(t *testing.T) {
	r := newRig(t, "container")
	h := &GuardHost{G: r.g}
	dir, err := h.MakeCanary("canary", "restore", Dir)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := TargetIn(dir, "c4-target")
	r.write(tg.Rel()+"/copy", "x")
	// Another directory of B's, a host-only one: what a link a sandbox
	// made could point to (saferm's root keeps a path-based removal in B,
	// not out of everything the suite must not touch).
	outside := r.path("canary/host-only")
	must(t, os.MkdirAll(filepath.Join(outside, "c4-target", "keep"), 0o755))
	swapped := false
	testHookDiscard = func() {
		swapped = true
		must(t, os.Rename(r.path(dir.Rel), r.path("canary/moved")))
		must(t, os.Symlink(outside, r.path(dir.Rel)))
	}
	t.Cleanup(func() { testHookDiscard = nil })
	if err := r.g.DiscardRestore(tg); err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Fatal("the hook did not run")
	}
	if _, err := os.Lstat(filepath.Join(outside, "c4-target", "keep")); err != nil {
		t.Errorf("removed through the link: %v", err)
	}
	if _, err := os.Lstat(r.path("canary/moved/c4-target")); !os.IsNotExist(err) {
		t.Errorf("the held directory's copy is still there: %v", err)
	}
	// A component on the way that is a link is refused before anything.
	testHookDiscard = nil
	if err := r.g.DiscardRestore(tg); err == nil {
		t.Error("a canary that is now a link accepted")
	}
}
