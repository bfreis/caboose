package conformance

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

type walkRig struct {
	*rig
	wr, ro, daemon Canary
	before         Snapshot
}

func newWalkRig(t *testing.T) *walkRig { return newWalkRigFor(t, config.KindContainer) }

func newWalkRigFor(t *testing.T, isolation string) *walkRig {
	r := newRig(t, isolation)
	g := r.g
	w := &walkRig{rig: r}
	var err error
	if w.wr, err = g.MakeCanary("roots/"+g.RootNames()[1]+"/"+WritablePart, "wr-file", File); err != nil {
		t.Fatal(err)
	}
	if w.ro, err = g.MakeCanary("roots/"+g.RootNames()[0], "ro-tree", Tree); err != nil {
		t.Fatal(err)
	}
	must(t, mkdirNoFollow(g.Base(), "home/daemon"))
	r.write("home/envs/"+g.Env()+"/config.toml", "# the suite's\n")
	if w.daemon, err = g.MakeCanary("home/daemon", "daemon", File); err != nil {
		t.Fatal(err)
	}
	if w.before, err = g.Snapshot(); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPostCheckNothingChanged(t *testing.T) {
	w := newWalkRig(t)
	if _, err := w.g.PostCheck(w.before, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.before[w.daemon.Rel]; !ok {
		t.Fatal("a canary in a product subtree was not recorded")
	}
}

func TestPostCheckAllowsTheChecksOwn(t *testing.T) {
	w := newWalkRig(t)
	w.write(w.wr.Rel, "changed by a broken control")
	after, err := w.g.PostCheck(w.before, []string{w.wr.Rel})
	if err != nil {
		t.Fatalf("the check's own canary: %v", err)
	}
	// Its new state is the next check's before: nothing more to report.
	if _, err := w.g.PostCheck(after, nil); err != nil {
		t.Fatalf("the next check: %v", err)
	}
	must(t, os.Remove(w.path(w.wr.Rel)))
	after, err = w.g.PostCheck(after, []string{w.wr.Rel})
	if err != nil {
		t.Fatalf("removed by its check: %v", err)
	}
	if _, err := w.g.PostCheck(after, nil); err != nil {
		t.Fatalf("after removal: %v", err)
	}
}

func TestPostCheckProductPaths(t *testing.T) {
	w := newWalkRig(t)
	env := w.g.Env()
	for _, p := range []string{
		"home/daemon/daemon.log",
		"home/daemon-dev/daemon.log",
		"home/daemon-dev/daemon.json",
		"home/envs/" + env + "/requests.json",
		"home/envs/" + env + "/link.log",
		"home/envs/" + env + "/grants.json",
		"home/envs/" + env + "/decisions.log",
		"home/envs/" + env + "/checkpoints/k/s/tree/f",
		w.g.DataRel() + "/local/linux-arm64/bin/claude",
		w.g.DataRel() + "/home/.claude.json",
		w.g.DataRel() + "/home/.claude/settings.json",
		w.g.DataRel() + "/link.log",
		StubLog,
	} {
		w.write(p, "product")
	}
	// The sandbox's own files the host and daemon checks write, relative
	// to the data dir (whose home/ the sandbox has as its home, and whose
	// proposals/ and sync/ it has at ~/.caboose-proposals and
	// ~/.caboose-sync): no canaries, so the walk lets them change.
	n := "conf-" + w.g.Nonce8() + "-"
	for _, p := range []string{
		"home/.config/caboose/sandbox.toml",
		"proposals/" + n + "alias.toml", "proposals/" + n + "trust.toml",
		"proposals/" + n + "answer.toml", "proposals/" + n + "widen.toml",
		"sync/" + n + "alias.toml", "sync/" + n + "trust.toml",
	} {
		w.write(w.g.DataRel()+"/"+p, "the host checks'")
	}
	if _, err := w.g.PostCheck(w.before, nil); err != nil {
		t.Fatalf("product writes reported: %v", err)
	}
}

// What only the vm isolation writes is the product's under vm, and walked
// like anything else under container and gvisor.
func TestPostCheckVMPaths(t *testing.T) {
	for _, iso := range config.Kinds {
		t.Run(iso, func(t *testing.T) {
			w := newWalkRigFor(t, iso)
			w.write("home/vm/v1.0.0/arm64/kernel", "product")
			w.write(w.g.DataRel()+"/vm/x/state.json", "product")
			_, err := w.g.PostCheck(w.before, nil)
			if iso == config.KindVM {
				if err != nil {
					t.Fatalf("vm writes reported under vm: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "home/vm/v1.0.0") || !strings.Contains(err.Error(), "/vm/x/state.json") {
				t.Fatalf("vm paths not walked under %s: %v", iso, err)
			}
		})
	}
}

func TestPostCheckReportsStrayChanges(t *testing.T) {
	cases := map[string]func(w *walkRig){
		"a new file in a root":       func(w *walkRig) { w.write("roots/"+w.g.RootNames()[0]+"/new", "x") },
		"a new file in the part":     func(w *walkRig) { w.write("roots/"+w.g.RootNames()[1]+"/part/new", "x") },
		"a host-only canary dir":     func(w *walkRig) { w.write("canary/outside-root/x", "x") },
		"a canary not handed over":   func(w *walkRig) { w.write(w.ro.Paths[3]+"/y", "x") },
		"a canary's content":         func(w *walkRig) { w.write(w.ro.Paths[len(w.ro.Paths)-1], "x") },
		"a canary's mode":            func(w *walkRig) { must(w.t, os.Chmod(w.path(w.ro.Paths[2]), 0o600)) },
		"a canary removed":           func(w *walkRig) { must(w.t, os.Remove(w.path(w.ro.Paths[2]))) },
		"a canary in a product tree": func(w *walkRig) { w.write(w.daemon.Rel, "x") },
		"a file outside the list":    func(w *walkRig) { w.write(w.g.DataRel()+"/home/.bashrc", "x") },
		"a stray file in the env":    func(w *walkRig) { w.write("home/envs/"+w.g.Env()+"/stray", "x") },
		"a file in B/data":           func(w *walkRig) { w.write("data/home/.claude.json", "x") },
		"a stub changed":             func(w *walkRig) { w.write(StubDir+"/open", "x") },
		"the stub log as a dir":      func(w *walkRig) { w.write(StubLog+"/x", "x") },
		"a symlink appears": func(w *walkRig) {
			must(w.t, os.Symlink("/", w.path("roots/"+w.g.RootNames()[1]+"/part/l")))
		},
		"a symlink replaces a canary": func(w *walkRig) {
			must(w.t, os.Remove(w.path(w.ro.Paths[2])))
			must(w.t, os.Symlink("/", w.path(w.ro.Paths[2])))
		},
		"a hard link to a canary": func(w *walkRig) {
			must(w.t, os.Link(w.path(w.ro.Paths[2]), w.path(w.g.DataRel()+"/hl")))
		},
		"a marker": func(w *walkRig) { w.write(w.g.DataMarker(), w.g.Nonce()+"\n\n") },
		"B's top":  func(w *walkRig) { w.write("stray", "x") },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWalkRig(t)
			mod(w)
			_, err := w.g.PostCheck(w.before, []string{w.wr.Rel})
			var te *TouchedError
			if !errors.As(err, &te) {
				t.Fatalf("not reported: %v", err)
			}
		})
	}
}

func TestPostCheckRefusesANonCanary(t *testing.T) {
	w := newWalkRig(t)
	if _, err := w.g.PostCheck(w.before, []string{"roots"}); err == nil {
		t.Fatal("a non-canary was allowed")
	}
}

// config.toml may change only to what the suite last wrote there.
func TestPostCheckConfigOnlyTheSuites(t *testing.T) {
	touched := func(w *walkRig, what string, before Snapshot) {
		t.Helper()
		_, err := w.g.PostCheck(before, nil)
		var te *TouchedError
		if !errors.As(err, &te) || len(te.Paths) != 1 || te.Paths[0] != w.g.ConfigRel() {
			t.Fatalf("%s: %v, want config.toml touched", what, err)
		}
	}
	w := newWalkRig(t)
	w.write(w.g.ConfigRel(), "trust = \"hostile\"\n")
	touched(w, "written by anything but the suite", w.before)

	w = newWalkRig(t)
	w.write(w.g.ConfigRel(), "trust = \"hostile\"\n")
	must(t, w.g.NoteConfigWritten())
	after, err := w.g.PostCheck(w.before, nil)
	if err != nil {
		t.Fatalf("the suite's own write: %v", err)
	}
	w.write(w.g.ConfigRel(), "trust = \"fallible\"\n")
	touched(w, "other contents after the suite's write", after)
}
