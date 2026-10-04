package fswatch

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// waitFor reads w until every path in want has been reported, or fails.
func waitFor(t *testing.T, w *Watcher, want ...string) {
	t.Helper()
	got := map[string]bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			paths, ok := w.Next()
			if !ok {
				return
			}
			for _, p := range paths {
				got[p] = true
			}
			if !slices.ContainsFunc(want, func(p string) bool { return !got[p] }) {
				return
			}
		}
	}()
	select {
	case <-done:
		for _, p := range want {
			if !got[p] {
				t.Fatalf("watcher closed before %s was reported", p)
			}
		}
	case <-time.After(5 * time.Second):
		w.Close()
		<-done
		var missing []string
		for _, p := range want {
			if !got[p] {
				missing = append(missing, p)
			}
		}
		t.Fatalf("not reported within 5s: %v (reported: %v)", missing, got)
	}
}

// settle gives the watcher time to be listening: FSEvents starts
// reporting a little after its stream starts.
func settle() { time.Sleep(300 * time.Millisecond) }

func TestWatchReportsChanges(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(existing, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Watch([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	settle()

	if err := os.WriteFile(existing, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, existing)

	created := filepath.Join(root, "created.txt")
	if err := os.WriteFile(created, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, created)

	if err := os.Remove(created); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, created)

	renamed := filepath.Join(root, "renamed.txt")
	if err := os.Rename(existing, renamed); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, existing, renamed)
}

func TestWatchFollowsNewDirectories(t *testing.T) {
	root := t.TempDir()
	w, err := Watch([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	settle()

	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "new.txt")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, f)

	// A file in the new directory changed later, once it is surely
	// watched.
	settle()
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, f)
}

// TestWatchReportsUnderGivenRoot: a root given through a symlink (as a
// Mac's temp dir is, under /private) reports paths under the name it was
// given, not the one the system uses.
func TestWatchReportsUnderGivenRoot(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	w, err := Watch([]string{link})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	settle()
	if err := os.WriteFile(filepath.Join(real, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, filepath.Join(link, "f"))
}

func TestWatchCloseEndsNext(t *testing.T) {
	w, err := Watch([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Changes from before Close may still come first: FSEvents reports
	// the temp dir's own creation, just before the stream started.
	done := make(chan struct{})
	go func() {
		for {
			if _, ok := w.Next(); !ok {
				close(done)
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	w.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not end after Close")
	}
	if _, ok := w.Next(); ok {
		t.Fatal("Next said ok after Close")
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		p, dir string
		want   bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/x", "/", true},
	} {
		if got := within(c.p, c.dir); got != c.want {
			t.Errorf("within(%q, %q) = %v, want %v", c.p, c.dir, got, c.want)
		}
	}
}

func TestOverflowReportsRoots(t *testing.T) {
	w := &Watcher{roots: []root{{given: "/given", phys: "/phys"}}, seen: map[string]bool{}, wake: make(chan struct{}, 1)}
	for i := range maxPending + 1 {
		w.add(filepath.Join("/phys", "f", string(rune('a'+i%26)), time.Duration(i).String()))
	}
	w.add("/elsewhere/x")
	paths, ok := w.Next()
	if !ok || !slices.Equal(paths, []string{"/given"}) {
		t.Fatalf("Next = %v, %v; want the root after an overflow", paths, ok)
	}
}
