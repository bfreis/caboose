package conformance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

type fakeRemover struct {
	calls  []string
	during func() // runs on the first removal
}

func (f *fakeRemover) RemoveContainer(_ context.Context, name string) error {
	f.calls = append(f.calls, "container "+name)
	if f.during != nil {
		f.during()
	}
	return nil
}

func (f *fakeRemover) RemoveImage(_ context.Context, name string) error {
	f.calls = append(f.calls, "image "+name)
	return nil
}

// sentinels puts a file beside B and one in the fake real home, which no
// teardown may touch.
func sentinels(t *testing.T, r *rig) []string {
	s := []string{filepath.Join(r.tmp, "beside-B"), filepath.Join(r.real, "keep")}
	for _, p := range s {
		must(t, os.WriteFile(p, []byte("keep"), 0o644))
	}
	return s
}

func TestTeardownRemovesExactlyItsOwn(t *testing.T) {
	r := newRig(t, config.KindContainer)
	underTemp(t, r.g.Base())
	keep := sentinels(t, r)
	// A canary tree a check left read-only.
	c, err := r.g.MakeCanary("roots/"+r.g.RootNames()[0], "ro-tree", Tree)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.Chmod(r.path(c.Paths[3]), 0o555))
	rm := &fakeRemover{}
	if err := r.g.Teardown(context.Background(), rm); err != nil {
		t.Fatal(err)
	}
	env := r.g.Env()
	want := []string{"container caboose-" + env, "image caboose:" + env, "image caboose-base:" + env}
	if !slices.Equal(rm.calls, want) {
		t.Fatalf("removed %v", rm.calls)
	}
	if _, err := os.Lstat(r.g.Base()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("B is still there: %v", err)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was touched: %v", p, err)
		}
	}
	if es, _ := os.ReadDir(r.tmp); len(es) != 1 {
		t.Fatalf("the temp dir holds %v", es)
	}
}

func TestTeardownDeletesNothingWhenVerifyFails(t *testing.T) {
	for name, c := range map[string]struct {
		before func(r *rig)        // before Teardown
		during func(r *rig) func() // while the container is removed
	}{
		"wrong nonce at the start": {before: func(r *rig) {
			rewriteMarker(r, func(m *Marker) { m.Nonce = "00000000000000000000000000000000" })
		}},
		"in a sandbox at the start": {before: func(r *rig) {
			r.g.facts.Detectors = []Detector{{"fake", func() (bool, error) { return true, nil }}}
		}},
		"root marker gone at the start": {before: func(r *rig) {
			must(r.t, os.Remove(filepath.Join(r.g.RootPath(r.g.RootNames()[1]), RootMarker)))
		}},
		"marker changed while removing": {during: func(r *rig) func() {
			return func() { rewriteMarker(r, func(m *Marker) { m.Env = "default" }) }
		}},
		"B overlaps the home when it ends": {during: func(r *rig) func() {
			return func() { r.g.facts.UserHome = r.tmp }
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, config.KindContainer)
			underTemp(t, r.g.Base())
			keep := sentinels(t, r)
			rm := &fakeRemover{}
			if c.before != nil {
				c.before(r)
			}
			if c.during != nil {
				rm.during = c.during(r)
			}
			err := r.g.Teardown(context.Background(), rm)
			var le *LeftError
			if !errors.As(err, &le) || le.Base != r.g.Base() {
				t.Fatalf("Teardown: %v", err)
			}
			if c.before != nil && len(rm.calls) != 0 {
				t.Fatalf("removed %v", rm.calls)
			}
			for _, p := range append(keep, r.path(r.g.DataMarker()), r.path("roots/"+r.g.RootNames()[0]+"/"+RootMarker)) {
				if _, err := os.Lstat(p); err != nil {
					t.Fatalf("%s is gone: %v", p, err)
				}
			}
		})
	}
	// A guard it did not make, and no remover.
	if err := (&Guard{}).Teardown(context.Background(), &fakeRemover{}); err == nil {
		t.Fatal("a zero guard tore down")
	}
	r := newRig(t, config.KindContainer)
	if err := r.g.Teardown(context.Background(), nil); err == nil {
		t.Fatal("tore down with no remover")
	}
	if _, err := os.Lstat(r.path(MarkerFile)); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveCanary(t *testing.T) {
	r := newRig(t, config.KindContainer)
	underTemp(t, r.g.Base())
	part := "roots/" + r.g.RootNames()[1] + "/" + WritablePart
	a, err := r.g.MakeCanary(part, "a", Tree)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.g.MakeCanary(part, "b", File)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.Chmod(r.path(a.Paths[3]), 0o555))
	if err := r.g.RemoveCanary(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(r.path(a.Rel)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("not removed: %v", err)
	}
	if _, err := os.Lstat(r.path(b.Rel)); err != nil {
		t.Fatalf("the other canary: %v", err)
	}
	for _, c := range []Canary{
		{Rel: "roots"},
		{Rel: part},
		{Rel: "../x"},
		{Rel: a.Paths[2]},
		{Rel: MarkerFile},
	} {
		if err := r.g.RemoveCanary(c); err == nil {
			t.Fatalf("%s removed", c.Rel)
		}
	}
	if _, err := os.Lstat(r.path(MarkerFile)); err != nil {
		t.Fatal(err)
	}
}
