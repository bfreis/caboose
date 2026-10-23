package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stampsOf(t *testing.T, s *Store, key string) []string {
	t.Helper()
	es, err := s.List(key)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, FormatStamp(e.Stamp))
	}
	return out
}

func trashIn(t *testing.T, s *Store) []string {
	t.Helper()
	des, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if strings.HasPrefix(de.Name(), trashPrefix) {
			out = append(out, de.Name())
		}
	}
	return out
}

func TestPruneSeries(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/work/dev/x"
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var stamps []time.Time
	for d := time.Duration(0); d < 9*24*time.Hour; d += 37 * time.Minute {
		stamps = append(stamps, now.Add(-d).Truncate(time.Second))
	}
	for _, st := range stamps {
		take(t, s, l, p, st)
	}
	keep, drop := Retain(stamps, DefaultRules(now), nil)
	listed := 0
	testHookAfterTrash = func() { listed++ }
	t.Cleanup(func() { testHookAfterTrash = nil })
	dropped, err := s.Prune(l, Key(p), DefaultRules(now), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != len(drop) || listed != len(drop) {
		t.Fatalf("dropped %d, want %d", len(dropped), len(drop))
	}
	var want []string
	for _, k := range keep {
		want = append(want, FormatStamp(k))
	}
	if got := stampsOf(t, s, Key(p)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("left %v, want %v", got, want)
	}
	if tr := trashIn(t, s); len(tr) != 0 {
		t.Fatalf("trash left: %v", tr)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir(), logName))
	if strings.Count(string(b), "pruned ") != len(drop) {
		t.Errorf("log: %d prunes logged", strings.Count(string(b), "pruned "))
	}
	// Pruning again drops nothing.
	if d, err := s.Prune(l, Key(p), DefaultRules(now), nil); err != nil || len(d) != 0 {
		t.Fatalf("second prune %v %v", d, err)
	}
}

func TestPrunePinnedAndNewest(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "~/.claude"
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	src := take(t, s, l, p, now.Add(-14*24*time.Hour))
	take(t, s, l, p, now.Add(-10*24*time.Hour))
	fresh := take(t, s, l, p, now.Add(-time.Minute)) // before-restore
	other := take(t, s, l, "/other", now.Add(-20*24*time.Hour))
	r := Rules{From: now, KeepNewest: true}
	if _, err := s.Prune(l, Key(p), r, []ID{{Key: Key(p), Latest: true}}); err == nil {
		t.Fatal("pinned latest accepted")
	}
	dropped, err := s.Prune(l, Key(p), r, []ID{src, other})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 {
		t.Fatalf("dropped %v", dropped)
	}
	if got := stampsOf(t, s, Key(p)); fmt.Sprint(got) != fmt.Sprint([]string{FormatStamp(fresh.Stamp), FormatStamp(src.Stamp)}) {
		t.Fatalf("left %v", got)
	}
	// Only old ones: the newest stays under KeepNewest, and by default.
	if d, err := s.Prune(l, other.Key, r, nil); err != nil || len(d) != 0 {
		t.Fatalf("KeepNewest dropped %v %v", d, err)
	}
	if d, err := s.Prune(l, other.Key, DefaultRules(now), nil); err != nil || len(d) != 0 {
		t.Fatalf("default dropped %v %v", d, err)
	}
	if d, err := s.Prune(l, other.Key, Rules{From: now}, nil); err != nil || len(d) != 1 {
		t.Fatalf("from now, the newest not kept: kept %v %v", d, err)
	}
}

// TestPruneAfterLongStop: by default, ages count from the newest
// checkpoint, so a month with none taken drops nothing more.
func TestPruneAfterLongStop(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/work/dev/x"
	stop := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	newest := take(t, s, l, p, stop)
	day := take(t, s, l, p, stop.Add(-3*24*time.Hour))
	gone := take(t, s, l, p, stop.Add(-7*24*time.Hour))
	d, err := s.Prune(l, Key(p), DefaultRules(stop.Add(40*24*time.Hour)), nil)
	if err != nil || len(d) != 1 || d[0] != gone {
		t.Fatalf("dropped %v %v", d, err)
	}
	if got := stampsOf(t, s, Key(p)); fmt.Sprint(got) != fmt.Sprint([]string{FormatStamp(newest.Stamp), FormatStamp(day.Stamp)}) {
		t.Fatalf("left %v", got)
	}
}

func TestPruneReadOnlyTreesAndSymlinks(t *testing.T) {
	skipRoot(t)
	s := newStore(t)
	l := lockT(t, s)
	p := "/work/dev/x"
	now := time.Now()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chmod(outside, 0o555)
	t.Cleanup(func() { os.Chmod(outside, 0o700) })
	id := take(t, s, l, p, now.Add(-8*24*time.Hour))
	take(t, s, l, p, now.Add(-time.Minute))
	tree := filepath.Join(s.checkpointDir(id), TreeName)
	if err := os.Symlink(outside, filepath.Join(tree, "sub", "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "keep"), filepath.Join(tree, "outfile")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tree, "zero", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(tree, "zero", "deeper"), 0)
	os.Chmod(filepath.Join(tree, "zero"), 0)
	os.Chmod(filepath.Join(tree, "sub"), 0o555)
	os.Chmod(tree, 0o555)
	if d, err := s.Prune(l, Key(p), DefaultRules(now), nil); err != nil || len(d) != 1 {
		t.Fatalf("prune %v %v", d, err)
	}
	if _, err := os.Lstat(s.checkpointDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checkpoint left")
	}
	if fi, err := os.Stat(outside); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatal("outside changed")
	}
	if fi, err := os.Stat(filepath.Join(outside, "keep")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatal("outside file changed")
	}
	if tr := trashIn(t, s); len(tr) != 0 {
		t.Fatalf("trash left: %v", tr)
	}
}

func TestPruneLeavesUnreadable(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/w/x"
	now := time.Now()
	id := take(t, s, l, p, now.Add(-30*24*time.Hour))
	if err := os.WriteFile(filepath.Join(s.checkpointDir(id), metaName), []byte(`{"version":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Prune(l, Key(p), DefaultRules(now), nil); err != nil || len(d) != 0 {
		t.Fatalf("pruned an unreadable one: %v %v", d, err)
	}
	if _, err := os.Lstat(s.checkpointDir(id)); err != nil {
		t.Fatal("unreadable checkpoint removed")
	}
}

// TestPruneTrashFirst checks a dropped checkpoint is out of the listing
// before its removal starts.
func TestPruneTrashFirst(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/w/x"
	now := time.Now()
	id := take(t, s, l, p, now.Add(-8*24*time.Hour))
	take(t, s, l, p, now.Add(-time.Minute))
	called := false
	testHookAfterTrash = func() {
		called = true
		es, err := s.List(Key(p))
		if err != nil || len(es) != 1 || es[0].ID == id {
			t.Errorf("listed during removal: %v %v", es, err)
		}
		if tr := trashIn(t, s); len(tr) != 1 || !strings.HasPrefix(tr[0], trashPrefix+id.Key+"-"+FormatStamp(id.Stamp)+"-") {
			t.Errorf("trash %v", tr)
		}
	}
	t.Cleanup(func() { testHookAfterTrash = nil })
	if _, err := s.Prune(l, Key(p), DefaultRules(now), nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("hook not called")
	}
}

func TestPruneFailedRemoval(t *testing.T) {
	skipRoot(t)
	s := newStore(t)
	l := lockT(t, s)
	p := "/w/x"
	now := time.Now()
	take(t, s, l, p, now.Add(-8*24*time.Hour))
	keep := take(t, s, l, p, now.Add(-time.Minute))
	s.Logf("test start") // the log is open before the store turns read-only
	// The store not writable while the removal runs: it fails.
	testHookAfterTrash = func() { os.Chmod(s.Dir(), 0o500) }
	t.Cleanup(func() { testHookAfterTrash = nil; os.Chmod(s.Dir(), 0o700) })
	d, err := s.Prune(l, Key(p), DefaultRules(now), nil)
	os.Chmod(s.Dir(), 0o700)
	testHookAfterTrash = nil
	if err == nil || len(d) != 1 {
		t.Fatalf("want a failed removal: %v %v", d, err)
	}
	tr := trashIn(t, s)
	if len(tr) != 1 {
		t.Fatalf("trash %v", tr)
	}
	if got := stampsOf(t, s, Key(p)); len(got) != 1 || got[0] != FormatStamp(keep.Stamp) {
		t.Fatalf("left %v", got)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir(), logName))
	if !strings.Contains(string(b), "prune "+d[0].String()) {
		t.Errorf("failure not logged: %q", b)
	}
	// Not yet old: kept. An hour later: removed.
	if r, err := s.CleanLeftovers(l, time.Now()); err != nil || len(r) != 0 {
		t.Fatalf("fresh trash removed: %v %v", r, err)
	}
	if r, err := s.CleanLeftovers(l, time.Now().Add(LeftoverAge+time.Minute)); err != nil || len(r) != 1 || r[0] != tr[0] {
		t.Fatalf("aged trash: %v %v", r, err)
	}
	if tr := trashIn(t, s); len(tr) != 0 {
		t.Fatalf("trash left: %v", tr)
	}
}

func TestPruneExposed(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/w/x"
	now := time.Now()
	id := take(t, s, l, p, now.Add(-8*24*time.Hour))
	take(t, s, l, p, now.Add(-time.Minute))
	s.Exposed = func() error { return errors.New("the store is inside root R") }
	if _, err := s.Prune(l, Key(p), DefaultRules(now), nil); err == nil || !strings.Contains(err.Error(), "inside root R") {
		t.Fatalf("exposed store pruned: %v", err)
	}
	if _, err := os.Lstat(s.checkpointDir(id)); err != nil {
		t.Fatal("removed while exposed")
	}
	s.Exposed = func() error { return nil }
	if d, err := s.Prune(l, Key(p), DefaultRules(now), nil); err != nil || len(d) != 1 {
		t.Fatalf("%v %v", d, err)
	}
}
