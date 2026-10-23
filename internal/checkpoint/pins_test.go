package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/saferm"
)

// pinWorld is a path of n one-byte files, a store and a clock.
type pinWorld struct {
	s     *Store
	src   Source
	proj  string
	clock *manualClock
	notes []Notice
}

func newPinWorld(t *testing.T, n int) *pinWorld {
	t.Helper()
	skipRoot(t)
	top := tempTree(t)
	proj := filepath.Join(top, "proj")
	w := &pinWorld{s: takeStore(t), src: Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}, proj: proj,
		clock: &manualClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}}
	w.fill(t, 0, n, "x")
	return w
}

func (w *pinWorld) fill(t *testing.T, from, to int, content string) {
	t.Helper()
	for i := from; i < to; i++ {
		mustWrite(t, filepath.Join(w.proj, fmt.Sprintf("f%04d", i)), content, 0o644)
	}
}

func (w *pinWorld) remove(t *testing.T, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if err := os.Remove(filepath.Join(w.proj, fmt.Sprintf("f%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *pinWorld) take(t *testing.T) TakeResult {
	t.Helper()
	r, err := Take(context.Background(), w.s, w.src, ReasonInterval, TakeOptions{Now: w.clock.now, Statfs: plenty, Copy: walkerOnly,
		Notify: func(n Notice) { w.notes = append(w.notes, n) }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func pinIDs(t *testing.T, s *Store) []string {
	t.Helper()
	ps, err := s.Pins()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func listed(t *testing.T, s *Store, id ID) bool {
	t.Helper()
	es, err := s.List(id.Key)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(es, func(e Entry) bool { return e.ID == id })
}

// A sharp shrink pins the checkpoint before it, which then outlives
// retention, survives the store's reopening, and goes with a release.
func TestTakePinsOnShrink(t *testing.T) {
	w := newPinWorld(t, 1200)
	big := w.take(t)
	if big.Pinned != nil || len(pinIDs(t, w.s)) != 0 {
		t.Fatalf("pinned with no shrink: %v", big.Pinned)
	}
	w.remove(t, 0, 700)
	w.clock.add(10 * time.Minute)
	small := w.take(t)
	if small.Pinned == nil || *small.Pinned != big.ID || small.PinErr != nil {
		t.Fatalf("pinned %v (%v), want %s", small.Pinned, small.PinErr, big.ID)
	}
	ps, err := w.s.Pins()
	if err != nil || len(ps) != 1 {
		t.Fatalf("pins %+v %v", ps, err)
	}
	if p := ps[0]; p.ID != big.ID.String() || p.By != small.ID.String() || p.Entries != big.Meta.Entries || p.Bytes != big.Meta.Bytes || !p.Since.Equal(w.clock.now()) {
		t.Fatalf("pin %+v", p)
	}
	if len(w.notes) != 1 || w.notes[0].Kind != NoticeShrunk || !w.notes[0].Pinned || !strings.Contains(w.notes[0].Text, "kept until released") {
		t.Fatalf("notes %+v", w.notes)
	}
	b, err := os.ReadFile(filepath.Join(w.s.Dir(), pinsName))
	if err != nil || !strings.Contains(string(b), `"version": 1`) {
		t.Fatalf("pins.json %q %v", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(w.s.Dir(), pinsName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pins.json mode %v %v", fi, err)
	}

	// Eight days on, a smaller one still: the shrunk one ages out, the
	// pinned one stays.
	w.clock.add(8 * 24 * time.Hour)
	later := w.take(t)
	if !listed(t, w.s, big.ID) || listed(t, w.s, small.ID) || !slices.Contains(later.Pruned, small.ID) {
		t.Fatalf("after 8 days: big %v small %v, pruned %v", listed(t, w.s, big.ID), listed(t, w.s, small.ID), later.Pruned)
	}

	// Reopened, the pin is there.
	s2, err := Open(filepath.Dir(filepath.Dir(filepath.Dir(w.s.Dir()))), filepath.Dir(w.s.Dir()))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := pinIDs(t, s2); fmt.Sprint(got) != fmt.Sprint([]string{big.ID.String()}) {
		t.Fatalf("pins after reopening %v", got)
	}

	l, err := w.s.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if err := w.s.ReleasePin(l, ID{Key: big.ID.Key, Latest: true}); err == nil {
		t.Fatal("released latest")
	}
	if err := w.s.ReleasePin(l, later.ID); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("released one not pinned: %v", err)
	}
	if err := w.s.ReleasePin(l, big.ID); err != nil {
		t.Fatal(err)
	}
	if got := pinIDs(t, w.s); len(got) != 0 {
		t.Fatalf("pins after release %v", got)
	}
	if _, err := os.Lstat(filepath.Join(w.s.Dir(), pinsName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty pins file is left: %v", err)
	}
	if d, err := w.s.Prune(l, big.ID.Key, DefaultRules(w.clock.now()), nil); err != nil || len(d) != 1 || d[0] != big.ID {
		t.Fatalf("released, then pruned %v %v", d, err)
	}
}

// A later checkpoint that holds the pinned one's entries and bytes again
// unpins it; one that holds its entries alone does not.
func TestTakeUnpinsOnRecovery(t *testing.T) {
	w := newPinWorld(t, 1200)
	big := w.take(t)
	w.remove(t, 0, 700)
	w.clock.add(10 * time.Minute)
	if r := w.take(t); r.Pinned == nil {
		t.Fatal("not pinned")
	}
	// The entries back, empty: fewer bytes.
	w.fill(t, 0, 700, "")
	w.clock.add(10 * time.Minute)
	r := w.take(t)
	if r.Meta.Entries < big.Meta.Entries || r.Meta.Bytes >= big.Meta.Bytes || len(r.Unpinned) != 0 {
		t.Fatalf("entries %d bytes %d, unpinned %v", r.Meta.Entries, r.Meta.Bytes, r.Unpinned)
	}
	if got := pinIDs(t, w.s); len(got) != 1 {
		t.Fatalf("pins %v", got)
	}
	w.fill(t, 0, 700, "x")
	w.clock.add(10 * time.Minute)
	r = w.take(t)
	if fmt.Sprint(r.Unpinned) != fmt.Sprint([]string{big.ID.String()}) {
		t.Fatalf("unpinned %v", r.Unpinned)
	}
	if got := pinIDs(t, w.s); len(got) != 0 {
		t.Fatalf("pins %v", got)
	}
}

// A path under the thresholds (1,000 entries) is never pinned.
func TestTakeNoPinUnderThreshold(t *testing.T) {
	w := newPinWorld(t, 990)
	w.take(t)
	w.remove(t, 0, 900)
	w.clock.add(10 * time.Minute)
	if r := w.take(t); r.Pinned != nil || r.Meta.Shrunk != nil {
		t.Fatalf("pinned %v, shrunk %v", r.Pinned, r.Meta.Shrunk)
	}
	if got := pinIDs(t, w.s); len(got) != 0 {
		t.Fatalf("pins %v", got)
	}
}

// A pin that cannot be written: the checkpoint is taken, nothing is
// pruned.
func TestTakePinFailsNoPrune(t *testing.T) {
	w := newPinWorld(t, 1200)
	w.take(t)
	w.clock.add(8 * 24 * time.Hour)
	old := w.take(t) // the newest, then
	w.remove(t, 0, 700)
	w.clock.add(8 * 24 * time.Hour)
	if err := os.Mkdir(filepath.Join(w.s.Dir(), pinsName), 0o700); err != nil {
		t.Fatal(err)
	}
	r := w.take(t)
	if r.Pinned != nil || r.PinErr == nil || r.PruneErr == nil || len(r.Pruned) != 0 {
		t.Fatalf("pinned %v (%v), pruned %v (%v)", r.Pinned, r.PinErr, r.Pruned, r.PruneErr)
	}
	if !listed(t, w.s, old.ID) {
		t.Fatal("the checkpoint to restore from was pruned")
	}
	if len(w.notes) != 1 || w.notes[0].Pinned {
		t.Fatalf("notes %+v", w.notes)
	}
}

// The pins file: none when missing; one that cannot be used makes a prune
// drop nothing; a link is not followed; a pin whose checkpoint is gone is
// forgotten; each key keeps at most maxPinsPerKey.
func TestPinsFile(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/work/dev/x"
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if ps, err := s.Pins(); err != nil || ps != nil {
		t.Fatalf("no file: %v %v", ps, err)
	}
	old := take(t, s, l, p, now.Add(-9*24*time.Hour))
	take(t, s, l, p, now)
	pf := filepath.Join(s.Dir(), pinsName)
	for name, content := range map[string]string{
		"not JSON":       "{",
		"newer":          `{"version":2,"pins":[]}`,
		"bad id":         `{"version":1,"pins":[{"id":"nope"}]}`,
		"latest":         `{"version":1,"pins":[{"id":"x-0@latest"}]}`,
		"over the bound": `{"version":1,"pins":[` + strings.Repeat(`{"id":"`+old.String()+`"},`, maxMetaSize/20) + `{}]}`,
	} {
		if err := os.WriteFile(pf, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pins(); err == nil {
			t.Errorf("%s: read", name)
		}
		if d, err := s.Prune(l, Key(p), DefaultRules(now), nil); err == nil || len(d) != 0 {
			t.Errorf("%s: pruned %v %v", name, d, err)
		}
	}
	if err := os.Remove(pf); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(target, []byte(`{"version":1,"pins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, pf); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pins(); err == nil {
		t.Error("a link followed")
	}
	if err := os.Remove(pf); err != nil {
		t.Fatal(err)
	}

	// Bounded per key: the oldest go.
	for i := range maxPinsPerKey + 2 {
		id := ID{Key: Key(p), Stamp: now.Add(-time.Duration(100+i) * time.Hour)}
		if err := s.addPin(l, Key(p), Pin{ID: id.String(), Since: now.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := s.Pins()
	if err != nil || len(ps) != maxPinsPerKey || ps[0].Since != now.Add(2*time.Minute) {
		t.Fatalf("bounded pins %d %v", len(ps), err)
	}
	// None of those is listed: a prune forgets them all, and pins old.
	if err := s.addPin(l, Key(p), Pin{ID: old.String(), Since: now}); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Prune(l, Key(p), DefaultRules(now), nil); err != nil || len(d) != 0 {
		t.Fatalf("pruned %v %v", d, err)
	}
	if got := pinIDs(t, s); fmt.Sprint(got) != fmt.Sprint([]string{old.String()}) {
		t.Fatalf("pins after the prune %v", got)
	}
	// Removed by hand: forgotten at the next prune.
	if err := saferm.RemoveTree(s.Dir(), s.checkpointDir(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(l, Key(p), DefaultRules(now), nil); err != nil {
		t.Fatal(err)
	}
	if got := pinIDs(t, s); len(got) != 0 {
		t.Fatalf("pins of a checkpoint gone %v", got)
	}
}

// A pin whose write fails while the pins file stays readable: the
// checkpoint is taken, and nothing of the key is pruned.
func TestTakePinWriteFailsNoPrune(t *testing.T) {
	w := newPinWorld(t, 1200)
	w.take(t)
	w.clock.add(8 * 24 * time.Hour)
	old := w.take(t)
	w.remove(t, 0, 700)
	w.clock.add(8 * 24 * time.Hour)
	testHookWritePins = func() error { return errors.New("disk on fire") }
	t.Cleanup(func() { testHookWritePins = nil })
	r := w.take(t)
	if r.Pinned != nil || r.PinErr == nil || r.PruneErr == nil || len(r.Pruned) != 0 {
		t.Fatalf("pinned %v (%v), pruned %v (%v)", r.Pinned, r.PinErr, r.Pruned, r.PruneErr)
	}
	if !listed(t, w.s, old.ID) {
		t.Fatal("the checkpoint to restore from was pruned")
	}
	testHookWritePins = nil
	if ps, err := w.s.Pins(); err != nil || len(ps) != 0 {
		t.Fatalf("pins %v %v", ps, err)
	}
}

// A checkpoint of another path, however large, unpins nothing of this one.
func TestPinsRecoveryIsPerKey(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	a := take(t, s, l, "/work/dev/a", now)
	if err := s.addPin(l, a.Key, Pin{ID: a.String(), Entries: 10, Bytes: 10, Since: now}); err != nil {
		t.Fatal(err)
	}
	gone, err := s.recovered(l, Key("/work/dev/b"), Meta{Entries: 1 << 40, Bytes: 1 << 40})
	if err != nil || len(gone) != 0 {
		t.Fatalf("another path unpinned %v %v", gone, err)
	}
	if got := pinIDs(t, s); len(got) != 1 {
		t.Fatalf("pins %v", got)
	}
	if gone, err := s.recovered(l, a.Key, Meta{Entries: 10, Bytes: 10}); err != nil || len(gone) != 1 {
		t.Fatalf("its own path: unpinned %v %v", gone, err)
	}
}

// At most maxPins in all, the oldest going first; a file holding more is
// refused.
func TestPinsTotalBound(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	n := 0
	var first string
	for k := 0; n < maxPins+5; k++ {
		key := Key(fmt.Sprintf("/work/dev/p%d", k))
		for j := 0; j < maxPinsPerKey && n < maxPins+5; j++ {
			id := ID{Key: key, Stamp: now.Add(-time.Duration(j+1) * time.Hour)}
			if n == 0 {
				first = id.String()
			}
			if err := s.addPin(l, key, Pin{ID: id.String(), Since: now.Add(time.Duration(n) * time.Second)}); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	ps, err := s.Pins()
	if err != nil || len(ps) != maxPins {
		t.Fatalf("%d pins %v", len(ps), err)
	}
	if ps[0].Since != now.Add(5*time.Second) || slices.ContainsFunc(ps, func(p Pin) bool { return p.ID == first }) {
		t.Fatalf("the oldest kept: %+v", ps[0])
	}
	// One more than the bound, written by hand: refused.
	ps = append(ps, Pin{ID: ID{Key: Key("/work/dev/extra"), Stamp: now}.String(), Since: now})
	b, err := json.Marshal(pinsDoc{Version: MetaVersion, Pins: ps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), pinsName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pins(); err == nil || !strings.Contains(err.Error(), "over 256 pins") {
		t.Fatalf("%d pins read: %v", len(ps), err)
	}
}

// A write of pins.json or path.json that did not finish leaves a temporary
// file: cleared once it is old, through the store's own cleanup; a young
// one, or a directory of that name, is left.
func TestCleanLeftoverTempFiles(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	id := take(t, s, l, "/work/dev/x", time.Now())
	old := []string{
		filepath.Join(s.Dir(), pinsName+".tmp-0123456789abcdef"),
		filepath.Join(s.Dir(), id.Key, pathName+".tmp-0123456789abcdef"),
	}
	young := filepath.Join(s.Dir(), pinsName+".tmp-fedcba9876543210")
	dir := filepath.Join(s.Dir(), pinsName+".tmp-dir")
	for _, p := range append(slices.Clone(old), young) {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * LeftoverAge)
	for _, p := range append(slices.Clone(old), dir) {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.CleanLeftovers(l, time.Now())
	if err != nil || len(removed) != 2 {
		t.Fatalf("removed %v %v", removed, err)
	}
	for _, p := range old {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left: %v", p, err)
		}
	}
	for _, p := range []string{young, dir, filepath.Join(s.Dir(), id.Key, pathName)} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}
