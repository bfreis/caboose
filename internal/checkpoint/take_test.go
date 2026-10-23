package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// clock is a fake clock that moves a minute on each reading.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// walkerOnly keeps a take on the walker: on a Mac the store and the
// source share a volume, and the whole-tree clone would run instead.
var walkerOnly = Options{NoWhole: true}

// plenty is a Statfs with lots of room.
func plenty(string) (uint64, uint64, error) { return 900 << 30, 1000 << 30, nil }

// takeStore is a store whose exposed-store check passes.
func takeStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	makeWritable(t, s.Dir())
	s.Exposed = func() error { return nil }
	return s
}

func leftovers(t *testing.T, s *Store) []string {
	t.Helper()
	des, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if strings.HasPrefix(de.Name(), tmpPrefix) || strings.HasPrefix(de.Name(), trashPrefix) {
			out = append(out, de.Name())
		}
	}
	return out
}

// takeT runs Take with a timeout.
func takeT(t *testing.T, f func() (TakeResult, error)) (TakeResult, error) {
	t.Helper()
	type out struct {
		r   TakeResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := f()
		ch <- out{r, err}
	}()
	select {
	case o := <-ch:
		return o.r, o.err
	case <-time.After(30 * time.Second):
		t.Fatal("Take did not return")
		return TakeResult{}, nil
	}
}

func TestTakeSequence(t *testing.T) {
	s := takeStore(t)
	top := tempTree(t)
	for i := range 5 {
		mustWrite(t, filepath.Join(top, "proj", fmt.Sprint(i)), "file", 0o644)
	}
	src := Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}
	// Files written now are older than a checkpoint started a minute
	// ahead, so the second take links them.
	c := &clock{t: time.Now().Add(time.Minute).UTC().Truncate(time.Second)}
	o := TakeOptions{Now: c.now, Statfs: plenty, Copy: Options{NoWhole: true, NoReflink: true}}
	r1, err := takeT(t, func() (TakeResult, error) { return Take(context.Background(), s, src, ReasonInterval, o) })
	if err != nil {
		t.Fatal(err)
	}
	m := r1.Meta
	if m.Kind != KindDir || m.Method != MethodCopy || m.Reason != ReasonInterval || m.Entries != 5 || m.Bytes != 20 ||
		m.Source != filepath.Join(top, "proj") || m.Caboose == "" || m.Path != src.Path || m.Shrunk != nil || m.Linked != 0 {
		t.Errorf("meta %+v", m)
	}
	if !m.Finished.After(m.Started) || FormatStamp(r1.ID.Stamp) != FormatStamp(m.Started) {
		t.Errorf("times %v %v, id %s", m.Started, m.Finished, r1.ID)
	}
	es, err := s.List(Key(src.Path))
	if err != nil || len(es) != 1 || es[0].Err != nil || es[0].Meta.Entries != 5 {
		t.Fatalf("listed %+v %v", es, err)
	}
	if b, err := os.ReadFile(filepath.Join(s.checkpointDir(r1.ID), TreeName, "3")); err != nil || string(b) != "file" {
		t.Fatalf("copy: %q %v", b, err)
	}
	r2, err := takeT(t, func() (TakeResult, error) { return Take(context.Background(), s, src, ReasonInterval, o) })
	if err != nil {
		t.Fatal(err)
	}
	if r2.Meta.Linked != 5 {
		t.Errorf("second take linked %d", r2.Meta.Linked)
	}
	if len(leftovers(t, s)) != 0 {
		t.Errorf("leftovers %v", leftovers(t, s))
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(), logName))
	if err != nil || !strings.Contains(string(b), "took "+r2.ID.String()+" method=copy") {
		t.Errorf("log %q %v", b, err)
	}
}

func TestTakePrune(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "proj", "f"), "f", 0o644)
	src := Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name    string
		noPrune bool
		pin     bool
		gone    bool
	}{{"prunes", false, false, true}, {"NoPrune", true, false, false}, {"pinned", false, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := takeStore(t)
			c := &clock{t: now.Add(-9 * 24 * time.Hour)}
			o := TakeOptions{Now: c.now, Statfs: plenty, Copy: walkerOnly}
			old, err := Take(context.Background(), s, src, ReasonInterval, o)
			if err != nil {
				t.Fatal(err)
			}
			c.set(now)
			o.NoPrune = tc.noPrune
			if tc.pin {
				o.Pinned = []ID{old.ID}
			}
			r, err := Take(context.Background(), s, src, ReasonInterval, o)
			if err != nil {
				t.Fatal(err)
			}
			es, _ := s.List(Key(src.Path))
			listed := slices.ContainsFunc(es, func(e Entry) bool { return e.ID == old.ID })
			if listed == tc.gone || slices.Contains(r.Pruned, old.ID) != tc.gone {
				t.Errorf("old listed %v, pruned %v", listed, r.Pruned)
			}
		})
	}
}

func TestTakeFailureLeavesNoTemp(t *testing.T) {
	s := takeStore(t)
	top := tempTree(t)
	for i := range 5 {
		mustWrite(t, filepath.Join(top, "proj", "d", fmt.Sprint(i)), "file", 0o644)
	}
	if err := os.Chmod(filepath.Join(top, "proj", "d"), 0o555); err != nil {
		t.Fatal(err)
	}
	src := Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}
	boom := errors.New("boom")
	o := TakeOptions{Statfs: plenty, Copy: Options{NoWhole: true, Hooks: &Hooks{BeforeWrite: func(rel string) error {
		if rel == "d/3" {
			return boom
		}
		return nil
	}}}}
	if _, err := Take(context.Background(), s, src, ReasonInterval, o); !errors.Is(err, boom) {
		t.Fatalf("%v, want boom", err)
	}
	if l := leftovers(t, s); len(l) != 0 {
		t.Fatalf("leftovers %v", l)
	}
	if es, _ := s.List(Key(src.Path)); len(es) != 0 {
		t.Fatalf("listed %v", es)
	}
	// A source that is not there fails too, and leaves nothing.
	if _, err := Take(context.Background(), s, Source{Path: "/work/dev/gone", Kind: SourceRoot, Top: top, Rel: "gone"}, ReasonInterval, TakeOptions{Statfs: plenty, Copy: walkerOnly}); !errors.Is(err, ErrSourceMissing) {
		t.Fatalf("%v, want ErrSourceMissing", err)
	}
	if l := leftovers(t, s); len(l) != 0 {
		t.Fatalf("leftovers %v", l)
	}
}

func TestTakeDropsLogin(t *testing.T) {
	skipRoot(t)
	for _, whole := range []bool{false, true} {
		t.Run(fmt.Sprint("whole=", whole), func(t *testing.T) {
			s := takeStore(t)
			data := tempTree(t)
			claude := filepath.Join(data, "home", ".claude")
			mustWrite(t, filepath.Join(claude, ".credentials.json"), `{"token":"x"}`, 0o600)
			mustWrite(t, filepath.Join(claude, "settings.json"), `{}`, 0o644)
			if err := os.Chmod(claude, 0o555); err != nil {
				t.Fatal(err)
			}
			if whole {
				// A clone keeps the 0555 directory as it is.
				fakeWhole(t, true, func(sp *os.File, sb string, dp *os.File, db string) error {
					d := filepath.Join(dp.Name(), db)
					mustWrite(t, filepath.Join(d, ".credentials.json"), `{"token":"x"}`, 0o600)
					mustWrite(t, filepath.Join(d, "settings.json"), `{}`, 0o644)
					return os.Chmod(d, 0o555)
				})
			}
			src := Source{Path: "~/.claude", Kind: SourceHome, Top: data, Rel: "home/.claude"}
			r, err := Take(context.Background(), s, src, ReasonInterval, TakeOptions{Statfs: plenty, Copy: Options{NoWhole: !whole}})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(r.Meta.Dropped, []string{"~/.claude/.credentials.json"}) {
				t.Errorf("dropped %v", r.Meta.Dropped)
			}
			tree := filepath.Join(s.checkpointDir(r.ID), TreeName)
			if _, err := os.Lstat(filepath.Join(tree, ".credentials.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the login is in the checkpoint: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(tree, "settings.json")); err != nil {
				t.Error(err)
			}
			if m := lstat(t, tree).Mode & 0o7777; m != 0o555 {
				t.Errorf("the copy's .claude is %o, want 0555 back", m)
			}
			if _, err := os.Lstat(filepath.Join(claude, ".credentials.json")); err != nil {
				t.Errorf("the source's login: %v", err)
			}
			// The meta.json read back says so too.
			es, _ := s.List(r.ID.Key)
			if len(es) != 1 || !slices.Equal(es[0].Meta.Dropped, r.Meta.Dropped) {
				t.Errorf("listed %+v", es)
			}
		})
	}
	t.Run("login made a directory", func(t *testing.T) {
		s := takeStore(t)
		data := tempTree(t)
		// A login name the sandbox made a non-empty directory is dropped
		// whole: it neither stays in the copy nor fails the take.
		mustWrite(t, filepath.Join(data, "home", ".claude", ".credentials.json", "sub", "x"), "x", 0o600)
		mustWrite(t, filepath.Join(data, "home", ".claude", "keep"), "k", 0o600)
		src := Source{Path: "~/.claude", Kind: SourceHome, Top: data, Rel: "home/.claude"}
		r, err := Take(context.Background(), s, src, ReasonInterval, TakeOptions{Statfs: plenty, Copy: walkerOnly})
		if err != nil {
			t.Fatal(err)
		}
		tree := filepath.Join(s.checkpointDir(r.ID), TreeName)
		if _, err := os.Lstat(filepath.Join(tree, ".credentials.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Error("the login directory is in the checkpoint")
		}
		if _, err := os.Lstat(filepath.Join(tree, "keep")); err != nil {
			t.Error("the rest of the copy is missing")
		}
		if !slices.Equal(r.Meta.Dropped, []string{"~/.claude/.credentials.json"}) {
			t.Errorf("dropped %v", r.Meta.Dropped)
		}
		if _, err := os.Lstat(filepath.Join(data, "home", ".claude", ".credentials.json", "sub", "x")); err != nil {
			t.Error("the source was changed")
		}
		if l := leftovers(t, s); len(l) != 0 {
			t.Fatalf("leftovers %v", l)
		}
	})
	t.Run("file source", func(t *testing.T) {
		s := takeStore(t)
		data := tempTree(t)
		mustWrite(t, filepath.Join(data, "home", ".claude", ".credentials.json"), "x", 0o600)
		src := Source{Path: "~/.claude/.credentials.json", Kind: SourceHome, Top: data, Rel: "home/.claude/.credentials.json", File: true}
		r, err := Take(context.Background(), s, src, ReasonInterval, TakeOptions{Statfs: plenty, Copy: walkerOnly})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(s.checkpointDir(r.ID), TreeName)); !errors.Is(err, fs.ErrNotExist) {
			t.Error("the login is in the checkpoint")
		}
	})
}

func TestTakeLocked(t *testing.T) {
	s := takeStore(t)
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "proj", "f"), "f", 0o644)
	src := Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}
	o := TakeOptions{Statfs: plenty, Copy: walkerOnly}
	if _, err := TakeLocked(context.Background(), nil, s, src, ReasonManual, o); err == nil {
		t.Fatal("taken without the lock")
	}
	other := takeStore(t)
	ol := lockT(t, other)
	if _, err := TakeLocked(context.Background(), ol, s, src, ReasonManual, o); err == nil {
		t.Fatal("taken with another store's lock")
	}
	l := lockT(t, s)
	r, err := takeT(t, func() (TakeResult, error) { return TakeLocked(context.Background(), l, s, src, ReasonBeforeRestore, o) })
	if err != nil {
		t.Fatal(err)
	}
	if r.Meta.Reason != ReasonBeforeRestore || !l.Held(s) {
		t.Fatalf("%+v held %v", r.Meta, l.Held(s))
	}
	l.Unlock()
	if _, err := TakeLocked(context.Background(), l, s, src, ReasonManual, o); err == nil {
		t.Fatal("taken with a released lock")
	}
}

func TestTakeExposed(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "proj", "f"), "f", 0o644)
	src := Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}

	s := newStore(t)
	if _, err := Take(context.Background(), s, src, ReasonManual, TakeOptions{Statfs: plenty, Copy: walkerOnly}); err == nil {
		t.Fatal("taken with no exposed-store check set")
	}

	// CABOOSE_HOME, named through a symlinked alias: the same directory by
	// identity, whatever its name.
	home := filepath.Dir(filepath.Dir(filepath.Dir(s.Dir()))) // home/envs/default/checkpoints
	alias := filepath.Join(t.TempDir(), "alias")
	mustSymlink(t, home, alias)
	unrelated := t.TempDir()
	cases := []struct {
		name    string
		reach   []Reach
		exposed bool
	}{
		{"unrelated", []Reach{{unrelated, "root u"}, {filepath.Join(unrelated, "missing"), "root m"}}, false},
		{"home by alias", []Reach{{unrelated, "root u"}, {alias, "root R of environment F"}}, true},
		{"the store", []Reach{{s.Dir(), "mount"}}, true},
		{"the env dir by alias", []Reach{{filepath.Join(alias, "envs", "default"), "root E"}}, true},
		{"inside the store", []Reach{{filepath.Join(alias, "envs", "default", "checkpoints", "k"), "root K"}}, true},
		{"a sibling env", []Reach{{filepath.Join(alias, "envs", "other"), "root O"}}, false},
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "k"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "envs", "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.Exposed = func() error { return s.CheckReach(tc.reach) }
			_, err := Take(context.Background(), s, src, ReasonManual, TakeOptions{Statfs: plenty, Copy: walkerOnly})
			if got := errors.Is(err, ErrExposed); got != tc.exposed {
				t.Fatalf("exposed %v (%v), want %v", got, err, tc.exposed)
			}
			l := lockT(t, s)
			_, perr := s.Prune(l, Key(src.Path), DefaultRules(time.Now()), nil)
			l.Unlock()
			if errors.Is(perr, ErrExposed) != tc.exposed {
				t.Fatalf("prune: %v", perr)
			}
			if tc.exposed && len(leftovers(t, s)) != 0 {
				t.Fatal("an exposed take left a temp dir")
			}
		})
	}
}

// fakeStatfs reports plenty of room for the first n readings, then avail.
func fakeStatfs(n int, avail uint64) (StatfsFunc, *int) {
	calls := new(int)
	var mu sync.Mutex
	return func(string) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		*calls++
		if *calls <= n {
			return 900 << 30, 1000 << 30, nil
		}
		return avail, 1000 << 30, nil
	}, calls
}

func TestTakeFreeSpace(t *testing.T) {
	top := tempTree(t)
	for i := range 20 {
		mustWrite(t, filepath.Join(top, "proj", fmt.Sprint(i)), strings.Repeat("z", 2048), 0o644)
	}
	src := Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}
	soft, hard := Floors(1000 << 30)
	if soft != 50<<30 || hard != 10<<30 {
		t.Fatalf("floors %d %d", soft, hard)
	}
	if s, h := Floors(20 << 30); s != 10<<30 || h != 1<<30 {
		t.Fatalf("small volume floors %d %d", s, h)
	}

	t.Run("soft floor warns and takes", func(t *testing.T) {
		s := takeStore(t)
		var notes []Notice
		r, err := Take(context.Background(), s, src, ReasonInterval, TakeOptions{
			Statfs: func(string) (uint64, uint64, error) { return 20 << 30, 1000 << 30, nil },
			Notify: func(n Notice) { notes = append(notes, n) },
			Copy:   walkerOnly,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !r.LowSpace || len(notes) != 1 || notes[0].Kind != NoticeLowSpace || notes[0].ID != r.ID {
			t.Fatalf("low %v notes %+v", r.LowSpace, notes)
		}
	})
	hardCase := func(t *testing.T, statfs StatfsFunc, o Options) {
		t.Helper()
		s := takeStore(t)
		_, err := Take(context.Background(), s, src, ReasonInterval, TakeOptions{Statfs: statfs, Copy: o})
		if !errors.Is(err, ErrDiskFull) || !strings.Contains(err.Error(), "disk is nearly full") {
			t.Fatalf("%v, want ErrDiskFull", err)
		}
		if l := leftovers(t, s); len(l) != 0 {
			t.Fatalf("leftovers %v", l)
		}
		if es, _ := s.List(Key(src.Path)); len(es) != 0 {
			t.Fatalf("listed %v", es)
		}
	}
	t.Run("hard floor before", func(t *testing.T) {
		st, _ := fakeStatfs(0, 5<<30)
		hardCase(t, st, Options{NoWhole: true})
	})
	t.Run("hard floor during, by entries", func(t *testing.T) {
		old := spaceEveryEntries
		spaceEveryEntries = 5
		t.Cleanup(func() { spaceEveryEntries = old })
		st, calls := fakeStatfs(2, 5<<30) // before the take, at the walk's start
		hardCase(t, st, Options{NoWhole: true, NoReflink: true})
		if *calls != 3 {
			t.Errorf("statfs read %d times", *calls)
		}
	})
	t.Run("hard floor during, by bytes", func(t *testing.T) {
		old := spaceEveryBytes
		spaceEveryBytes = 4096
		t.Cleanup(func() { spaceEveryBytes = old })
		st, calls := fakeStatfs(2, 5<<30)
		hardCase(t, st, Options{NoWhole: true, NoReflink: true})
		if *calls != 3 {
			t.Errorf("statfs read %d times", *calls)
		}
	})
	t.Run("hard floor after a whole clone", func(t *testing.T) {
		calls := fakeWhole(t, true, func(_ *os.File, _ string, dp *os.File, db string) error {
			mustWrite(t, filepath.Join(dp.Name(), db, "x"), "cloned", 0o644)
			return nil
		})
		st, _ := fakeStatfs(1, 5<<30) // before the take only
		hardCase(t, st, Options{})
		if *calls != 1 {
			t.Errorf("clone called %d times", *calls)
		}
	})
	t.Run("ENOSPC from a write", func(t *testing.T) {
		hardCase(t, plenty, Options{NoWhole: true, Hooks: &Hooks{BeforeWrite: func(rel string) error {
			if rel == "7" {
				return unix.ENOSPC
			}
			return nil
		}}})
	})
	t.Run("ENOSPC copying data", func(t *testing.T) {
		hardCase(t, plenty, Options{NoWhole: true, NoReflink: true, Hooks: &Hooks{Copied: func(rel string, n int64) error {
			if rel == "7" {
				return unix.ENOSPC
			}
			return nil
		}}})
	})
	t.Run("EDQUOT from a write", func(t *testing.T) {
		hardCase(t, plenty, Options{NoWhole: true, Hooks: &Hooks{BeforeWrite: func(rel string) error {
			if rel == "7" {
				return unix.EDQUOT
			}
			return nil
		}}})
	})
}

func TestTakeShrunk(t *testing.T) {
	top := tempTree(t)
	proj := filepath.Join(top, "proj")
	const n = 2100
	for i := range n {
		mustWrite(t, filepath.Join(proj, fmt.Sprintf("f%04d", i)), "x", 0o644)
	}
	src := Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: top, Rel: "proj"}
	s := takeStore(t)
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	var notes []Notice
	o := TakeOptions{Now: c.now, Statfs: plenty, Copy: walkerOnly, Notify: func(n Notice) { notes = append(notes, n) }}
	r1, err := Take(context.Background(), s, src, ReasonInterval, o)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1000; i < n; i++ {
		if err := os.Remove(filepath.Join(proj, fmt.Sprintf("f%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	r2, err := Take(context.Background(), s, src, ReasonInterval, o)
	if err != nil {
		t.Fatal(err)
	}
	want := &Shrunk{ID: r1.ID.String(), Entries: n, Bytes: n}
	if r2.Meta.Shrunk == nil || *r2.Meta.Shrunk != *want {
		t.Fatalf("shrunk %+v, want %+v", r2.Meta.Shrunk, want)
	}
	if es, _ := s.List(r2.ID.Key); es[0].Meta.Shrunk == nil {
		t.Error("meta.json does not record the shrink")
	}
	if len(notes) != 1 || notes[0].Kind != NoticeShrunk || notes[0].Prev != r1.ID || notes[0].ID != r2.ID {
		t.Fatalf("notes %+v", notes)
	}
	// The next one, no smaller, is not.
	r3, err := Take(context.Background(), s, src, ReasonInterval, o)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Meta.Shrunk != nil || len(notes) != 1 {
		t.Fatalf("shrunk again: %+v %d notes", r3.Meta.Shrunk, len(notes))
	}

	// A small tree that halves is under the thresholds.
	small := filepath.Join(top, "small")
	for i := range 10 {
		mustWrite(t, filepath.Join(small, fmt.Sprint(i)), "x", 0o644)
	}
	ssrc := Source{Path: "/work/dev/small", Kind: SourceRoot, Top: top, Rel: "small"}
	if _, err := Take(context.Background(), s, ssrc, ReasonInterval, o); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		os.Remove(filepath.Join(small, fmt.Sprint(i)))
	}
	r, err := Take(context.Background(), s, ssrc, ReasonInterval, o)
	if err != nil || r.Meta.Shrunk != nil || len(notes) != 1 {
		t.Fatalf("small tree: %+v %v, %d notes", r.Meta.Shrunk, err, len(notes))
	}
}

func TestShrank(t *testing.T) {
	for _, tc := range []struct {
		pe, pb, e, b int64
		want         bool
	}{
		{2000, 0, 1000, 0, true},
		{2000, 0, 1001, 0, false}, // not half
		{1001, 0, 500, 0, true},   // over 1000, and half or less
		{1001, 0, 501, 0, false},  // just over half
		{1002, 0, 501, 0, true},   // exactly half
		{1000, 0, 0, 0, false},    // not over 1000
		{0, 300 << 20, 0, 150 << 20, true},
		{0, 100<<20 + 2, 0, 50<<20 + 1, true}, // just over 100 MiB, exactly half
		{0, 100<<20 + 2, 0, 50<<20 + 2, false},
		{0, 100 << 20, 0, 0, false}, // not over 100 MiB
		{1001, 100 << 20, 1001, 0, false},
		{1001, 100<<20 + 1, 1001, 0, true}, // bytes alone
		{10, 10, 0, 0, false},
		{100, 0, 200, 0, false},
	} {
		if got := shrank(&Meta{Entries: tc.pe, Bytes: tc.pb}, tc.e, tc.b); got != tc.want {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}

// Take takes a checkpoint of src for reason: it takes the store's lock
// (waiting as Store.Lock does) and calls TakeLocked.
func Take(ctx context.Context, s *Store, src Source, reason string, o TakeOptions) (TakeResult, error) {
	l, err := s.Lock(ctx, 0)
	if err != nil {
		return TakeResult{}, err
	}
	defer l.Unlock()
	return TakeLocked(ctx, l, s, src, reason, o)
}
