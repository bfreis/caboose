package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakePaths is a SandboxPaths with a fixed answer.
type fakePaths struct {
	set PathSet
	err error
}

func (f fakePaths) SandboxPaths(context.Context) (PathSet, error) { return f.set, f.err }

// rfix is a store with a checkpointed source, and a parent to restore
// into, all under t.TempDir.
type rfix struct {
	s      *Store
	home   string // CABOOSE_HOME
	top    string // the source's top
	src    Source
	parent string // where restores go
	clock  *clock
}

func newRfix(t *testing.T) *rfix {
	t.Helper()
	s := takeStore(t)
	home := filepath.Dir(filepath.Dir(filepath.Dir(s.Dir())))
	f := &rfix{s: s, home: home, top: tempTree(t), parent: tempTree(t),
		clock: &clock{t: time.Now().Add(time.Minute).UTC().Truncate(time.Second)}}
	f.src = Source{Path: "/work/dev/proj", Kind: SourceRoot, Top: f.top, Rel: "proj"}
	underTemp(t, f.parent)
	underTemp(t, s.Dir())
	return f
}

func (f *rfix) write(t *testing.T, rel, data string) {
	t.Helper()
	mustWrite(t, filepath.Join(f.top, "proj", rel), data, 0o644)
}

func (f *rfix) takeOpts() TakeOptions {
	return TakeOptions{Now: f.clock.now, Statfs: plenty, Copy: Options{NoWhole: true, NoReflink: true}}
}

func (f *rfix) take(t *testing.T) TakeResult {
	t.Helper()
	r, err := takeT(t, func() (TakeResult, error) {
		return Take(context.Background(), f.s, f.src, ReasonInterval, f.takeOpts())
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *rfix) opts(t *testing.T) RestoreOptions {
	return RestoreOptions{
		CabooseHome: f.home,
		Paths:       fakePaths{},
		Now:         f.clock.now,
		Statfs:      plenty,
		StoreStatfs: plenty,
		Copy:        Options{NoWhole: true, NoReflink: true},
	}
}

// live is a RestoreOptions.Live giving the fixture's source.
func (f *rfix) live(p string) (*Source, error) {
	if p != f.src.Path {
		return nil, errors.New("not configured")
	}
	src := f.src
	return &src, nil
}

func (f *rfix) restore(t *testing.T, id, to string, o RestoreOptions) (Restored, error) {
	t.Helper()
	type out struct {
		r   Restored
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := Restore(context.Background(), f.s, id, to, o)
		ch <- out{r, err}
	}()
	select {
	case r := <-ch:
		return r.r, r.err
	case <-time.After(30 * time.Second):
		t.Fatal("Restore did not return")
		return Restored{}, nil
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

func storeCount(t *testing.T, f *rfix) int {
	t.Helper()
	es, err := f.s.List(Key(f.src.Path))
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

func logText(t *testing.T, s *Store) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Dir(), logName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRestore(t *testing.T) {
	f := newRfix(t)
	f.write(t, "a", "A")
	f.write(t, "sub/b", "B")
	mustSymlink(t, "../a", filepath.Join(f.top, "proj", "sub", "lnk"))
	mustWrite(t, filepath.Join(f.top, "proj", ".git", "config"), gitConfigWithCommand, 0o644)
	r := f.take(t)
	to := filepath.Join(f.parent, "restored")
	res, err := f.restore(t, r.ID.String(), to, f.opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != to || res.ID != r.ID || res.Path != f.src.Path || res.Method != MethodCopy || res.Entries != 6 || res.HomePath || res.Left != "" {
		t.Errorf("%+v", res)
	}
	if res.BeforeRestore != nil || res.NoBeforeRestore == "" {
		t.Errorf("before-restore %v %q", res.BeforeRestore, res.NoBeforeRestore)
	}
	if readT(t, filepath.Join(to, "a")) != "A" || readT(t, filepath.Join(to, "sub", "b")) != "B" {
		t.Error("contents")
	}
	if l, err := os.Readlink(filepath.Join(to, "sub", "lnk")); err != nil || l != "../a" {
		t.Errorf("symlink %q %v", l, err)
	}
	// A repository's git config is the sandbox's file like any other: it is
	// restored as it was, and nothing is reported.
	if readT(t, filepath.Join(to, ".git", "config")) != gitConfigWithCommand {
		t.Error("config changed")
	}
	if len(res.Sanitized) != 0 {
		t.Errorf("sanitized %+v", res.Sanitized)
	}
	if got := names(t, f.parent); !slices.Equal(got, []string{"restored"}) {
		t.Errorf("parent holds %v", got)
	}
	if lt := logText(t, f.s); !strings.Contains(lt, "restored "+r.ID.String()+" to "+to+" method=copy") {
		t.Errorf("log %q", lt)
	}
}

func TestRestoreFile(t *testing.T) {
	f := newRfix(t)
	f.write(t, "notes", "N")
	f.src = Source{Path: "~/notes", Kind: SourceHome, Top: f.top, Rel: "home/notes", File: true}
	mustWrite(t, filepath.Join(f.top, "home", "notes"), "N", 0o644)
	r := f.take(t)
	to := filepath.Join(f.parent, "notes")
	res, err := f.restore(t, r.ID.String(), to, f.opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if readT(t, to) != "N" || !res.HomePath {
		t.Errorf("%+v", res)
	}
}

func TestRestoreTargetExists(t *testing.T) {
	f := newRfix(t)
	f.write(t, "a", "A")
	r := f.take(t)
	cases := map[string]func(p string){
		"file":     func(p string) { mustWrite(t, p, "mine", 0o644) },
		"emptydir": func(p string) { _ = os.Mkdir(p, 0o755) },
		"dangling": func(p string) { mustSymlink(t, filepath.Join(f.parent, "nowhere"), p) },
	}
	for name, mk := range cases {
		to := filepath.Join(f.parent, name)
		mk(to)
	}
	before := names(t, f.parent)
	o := f.opts(t)
	o.Live = f.live
	for name := range cases {
		to := filepath.Join(f.parent, name)
		_, err := f.restore(t, r.ID.String(), to, o)
		if !errors.Is(err, ErrTargetExists) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := names(t, f.parent); !slices.Equal(got, before) {
		t.Errorf("parent now %v, was %v", got, before)
	}
	if readT(t, filepath.Join(f.parent, "file")) != "mine" {
		t.Error("file changed")
	}
	if n := storeCount(t, f); n != 1 {
		t.Errorf("%d checkpoints: a before-restore was taken", n)
	}
	// Its parent missing, or a file.
	for _, to := range []string{filepath.Join(f.parent, "missing", "x"), filepath.Join(f.parent, "file", "x")} {
		if _, err := f.restore(t, r.ID.String(), to, o); err == nil {
			t.Errorf("%s: restored", to)
		}
	}
	// A bad ID.
	if _, err := f.restore(t, "../../etc@latest", filepath.Join(f.parent, "y"), o); err == nil {
		t.Error("bad id accepted")
	}
	if got := names(t, f.parent); !slices.Equal(got, before) {
		t.Errorf("parent now %v, was %v", got, before)
	}
}

func TestRestoreTargetAppears(t *testing.T) {
	// What appears: a directory with contents, an empty directory (which a
	// plain rename would replace), a file.
	for _, kind := range []string{"full", "empty", "file"} {
		f := newRfix(t)
		f.write(t, "a", "A")
		r := f.take(t)
		to := filepath.Join(f.parent, "restored")
		testHookBeforeRename = func(target string) {
			if target != to {
				t.Errorf("hook for %s", target)
			}
			switch kind {
			case "full":
				mustWrite(t, filepath.Join(to, "mine"), "mine", 0o644)
			case "empty":
				if err := os.Mkdir(to, 0o755); err != nil {
					t.Error(err)
				}
			case "file":
				mustWrite(t, to, "mine", 0o644)
			}
		}
		t.Cleanup(func() { testHookBeforeRename = nil })
		res, err := f.restore(t, r.ID.String(), to, f.opts(t))
		if !errors.Is(err, ErrTargetExists) || !strings.Contains(err.Error(), "nothing was overwritten") {
			t.Fatalf("%s: %v", kind, err)
		}
		switch kind {
		case "full":
			if got := names(t, to); !slices.Equal(got, []string{"mine"}) || readT(t, filepath.Join(to, "mine")) != "mine" {
				t.Errorf("target holds %v", got)
			}
		case "empty":
			if got := names(t, to); len(got) != 0 {
				t.Errorf("target holds %v", got)
			}
		case "file":
			if readT(t, to) != "mine" {
				t.Error("file changed")
			}
		}
		if res.Left == "" || readT(t, filepath.Join(res.Left, "a")) != "A" || !strings.Contains(err.Error(), res.Left) {
			t.Errorf("%s: copy left at %q", kind, res.Left)
		}
	}
}

func TestRestoreNoReplaceUnsupported(t *testing.T) {
	for _, e := range []error{errNoReplace, unix.EINVAL, unix.ENOSYS} {
		f := newRfix(t)
		f.write(t, "a", "A")
		f.write(t, "sub/b", "B")
		r := f.take(t)
		renameNoReplace = func(*os.File, string, *os.File, string) error { return e }
		t.Cleanup(func() { renameNoReplace = renameNoReplaceOS })
		to := filepath.Join(f.parent, "restored")
		res, err := f.restore(t, r.ID.String(), to, f.opts(t))
		if !errors.Is(err, ErrNoReplace) || !strings.Contains(err.Error(), "move it yourself") {
			t.Fatalf("%v: %v", e, err)
		}
		if exists(to) {
			t.Errorf("%v: target made", e)
		}
		if res.Left == "" || readT(t, filepath.Join(res.Left, "sub", "b")) != "B" {
			t.Errorf("%v: copy left at %q", e, res.Left)
		}
	}
}

// A target inside CABOOSE_HOME or a path a sandbox can write (named
// through any alias) is refused before anything is written or taken; one
// in a read-only root is restored, its readers reported.
func TestRestoreTargetPolicy(t *testing.T) {
	f := newRfix(t)
	f.write(t, "a", "A")
	r := f.take(t)
	writable := filepath.Join(f.parent, "w")
	readonly := filepath.Join(f.parent, "ro")
	for _, d := range []string{writable, readonly, filepath.Join(f.home, "restores")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The writable path is given through a symlinked alias.
	alias := filepath.Join(tempTree(t), "alias")
	mustSymlink(t, writable, alias)
	set := PathSet{
		Writable: []EnvPath{{Env: "b", Path: alias}, {Env: "a", Path: writable}, {Env: "c", Path: filepath.Join(f.parent, "other")}},
		ReadOnly: []EnvPath{{Env: "r", Path: readonly}, {Env: "a", Path: readonly}},
	}
	o := f.opts(t)
	o.Paths = fakePaths{set: set}
	o.Live = f.live

	for to, want := range map[string]string{
		filepath.Join(writable, "x"):           "the sandboxes of environments a, b can write",
		filepath.Join(f.home, "restores", "x"): "inside CABOOSE_HOME",
	} {
		_, err := f.restore(t, r.ID.String(), to, o)
		if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", to, err)
		}
		if exists(to) || len(names(t, filepath.Dir(to))) != 0 {
			t.Errorf("%s: written", to)
		}
	}
	if n := storeCount(t, f); n != 1 {
		t.Errorf("refused, yet %d checkpoints", n)
	}
	if !strings.Contains(logText(t, f.s), "refused") {
		t.Error("refusal not logged")
	}
	res, err := f.restore(t, r.ID.String(), filepath.Join(readonly, "x"), o)
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if !slices.Equal(res.Readers, []string{"a", "r"}) || len(res.Warnings) != 0 {
		t.Errorf("read-only: %+v", res)
	}
}

func TestRestoreFallbackSaid(t *testing.T) {
	f := newRfix(t)
	f.write(t, "a", "A")
	r := f.take(t)
	o := f.opts(t)
	o.Paths = fakePaths{set: PathSet{Fallback: true}}
	res, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fallback || !strings.Contains(strings.Join(res.Warnings, "\n"), "could not ask its daemon") {
		t.Errorf("%+v", res)
	}
	// A provider that fails stops the restore before anything is written.
	o.Paths = fakePaths{err: errors.New("boom")}
	if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "y"), o); err == nil {
		t.Error("restored without knowing the paths")
	}
	o.Paths = nil
	if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "y"), o); err == nil {
		t.Error("restored with no paths provider")
	}
	if got := names(t, f.parent); !slices.Equal(got, []string{"x"}) {
		t.Errorf("parent holds %v", got)
	}
}

func TestRestoreLatestPinned(t *testing.T) {
	f := newRfix(t)
	f.write(t, "v", "1")
	// An old checkpoint, then the newest, 14 days old: what a prune would
	// drop if it ran.
	f.clock.set(time.Now().Add(-15 * 24 * time.Hour).UTC())
	f.take(t)
	f.write(t, "v", "2")
	f.clock.set(time.Now().Add(-14 * 24 * time.Hour).UTC())
	newest := f.take(t)
	f.write(t, "v", "3")
	f.clock.set(time.Now().UTC())
	o := f.opts(t)
	o.Live = f.live
	var listedFirst []Entry
	o.Copy.Hooks = &Hooks{BeforeWrite: func(rel string) error {
		if listedFirst == nil {
			listedFirst, _ = f.s.List(Key(f.src.Path))
		}
		return nil
	}}
	var took []TakeOptions
	restoreTake = func(ctx context.Context, l *Lock, s *Store, src Source, reason string, to TakeOptions) (TakeResult, error) {
		took = append(took, to)
		return TakeLocked(ctx, l, s, src, reason, to)
	}
	t.Cleanup(func() { restoreTake = TakeLocked })
	to := filepath.Join(f.parent, "x")
	res, err := f.restore(t, Key(f.src.Path)+"@latest", to, o)
	if err != nil {
		t.Fatal(err)
	}
	// The source is pinned for any prune, and the take prunes nothing.
	if len(took) != 1 || !took[0].NoPrune || !slices.Contains(took[0].Pinned, newest.ID) {
		t.Errorf("before-restore take options %+v", took)
	}
	if res.ID != newest.ID || readT(t, filepath.Join(to, "v")) != "2" {
		t.Errorf("restored %s (%q), newest was %s", res.ID, readT(t, filepath.Join(to, "v")), newest.ID)
	}
	if res.BeforeRestore == nil || !res.BeforeRestore.Stamp.After(newest.ID.Stamp) {
		t.Fatalf("before-restore %v (%q)", res.BeforeRestore, res.NoBeforeRestore)
	}
	// Taken first: listed before the copy wrote anything.
	if len(listedFirst) != 3 || listedFirst[0].ID != *res.BeforeRestore || listedFirst[0].Meta.Reason != ReasonBeforeRestore {
		t.Errorf("listed at the copy's start: %+v", listedFirst)
	}
	b, err := os.ReadFile(filepath.Join(f.s.checkpointDir(*res.BeforeRestore), TreeName, "v"))
	if err != nil || string(b) != "3" {
		t.Errorf("before-restore holds %q %v", b, err)
	}
	if n := storeCount(t, f); n != 3 {
		t.Errorf("%d checkpoints after the restore: the before-restore take pruned", n)
	}
	// The path no longer resolving: said, and the restore goes on.
	o.Live = func(string) (*Source, error) { return nil, errors.New("runs through a symlink") }
	o.Copy.Hooks = nil
	res, err = f.restore(t, newest.ID.String(), filepath.Join(f.parent, "y"), o)
	if err != nil || res.BeforeRestore != nil || !strings.Contains(res.NoBeforeRestore, "runs through a symlink") {
		t.Errorf("%+v %v", res, err)
	}
}

func TestRestoreFreeSpace(t *testing.T) {
	low := func(string) (uint64, uint64, error) { return 1 << 20, 1000 << 30, nil }
	t.Run("before", func(t *testing.T) {
		f := newRfix(t)
		f.write(t, "a", "A")
		r := f.take(t)
		o := f.opts(t)
		// Low on the target's volume only: the store's has room.
		o.Statfs = func(p string) (uint64, uint64, error) {
			if p == f.parent {
				return low(p)
			}
			return plenty(p)
		}
		o.Live = f.live
		if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o); !errors.Is(err, ErrDiskFull) {
			t.Fatal(err)
		}
		if len(names(t, f.parent)) != 0 || storeCount(t, f) != 1 {
			t.Error("written")
		}
	})
	t.Run("during", func(t *testing.T) {
		f := newRfix(t)
		for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
			f.write(t, n, n)
		}
		r := f.take(t)
		prev := spaceEveryEntries
		spaceEveryEntries = 2
		t.Cleanup(func() { spaceEveryEntries = prev })
		o := f.opts(t)
		o.Statfs, _ = fakeStatfs(2, 1<<20) // the restore's check, the walker's first; then low
		if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o); !errors.Is(err, ErrDiskFull) {
			t.Fatal(err)
		}
		if got := names(t, f.parent); len(got) != 0 {
			t.Errorf("parent holds %v", got)
		}
	})
	t.Run("after", func(t *testing.T) {
		f := newRfix(t)
		f.write(t, "f", "F")
		r := f.take(t)
		o := f.opts(t)
		o.Statfs = func(p string) (uint64, uint64, error) {
			if m, _ := filepath.Glob(filepath.Join(p, restoreTempPrefix+"*", restoreOut, "f")); len(m) > 0 {
				return 1 << 20, 1000 << 30, nil
			}
			return 900 << 30, 1000 << 30, nil
		}
		if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o); !errors.Is(err, ErrDiskFull) {
			t.Fatal(err)
		}
		if got := names(t, f.parent); len(got) != 0 {
			t.Errorf("parent holds %v", got)
		}
	})
	t.Run("ENOSPC", func(t *testing.T) {
		f := newRfix(t)
		f.write(t, "f", "F")
		r := f.take(t)
		o := f.opts(t)
		o.Copy.Hooks = &Hooks{Copied: func(string, int64) error { return unix.ENOSPC }}
		if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o); !errors.Is(err, ErrDiskFull) {
			t.Fatal(err)
		}
		if got := names(t, f.parent); len(got) != 0 {
			t.Errorf("parent holds %v", got)
		}
	})
}

func TestRestoreSharesNoInode(t *testing.T) {
	f := newRfix(t)
	for _, n := range []string{"a", "b", "sub/c"} {
		f.write(t, n, n)
	}
	f.take(t)
	r := f.take(t)
	if r.Meta.Linked == 0 {
		t.Fatal("the second checkpoint linked nothing: the test proves nothing")
	}
	to := filepath.Join(f.parent, "x")
	if _, err := f.restore(t, r.ID.String(), to, f.opts(t)); err != nil {
		t.Fatal(err)
	}
	store := inodes(t, f.s.Dir())
	for ino, p := range inodes(t, to) {
		if sp, ok := store[ino]; ok {
			t.Errorf("%s shares its inode with %s", p, sp)
		}
	}
}

func TestRestoreFailureRemovesOnlyTemp(t *testing.T) {
	f := newRfix(t)
	for _, n := range []string{"a", "b", "c"} {
		f.write(t, n, n)
	}
	r := f.take(t)
	mustWrite(t, filepath.Join(f.parent, "keep-file"), "k", 0o644)
	mustWrite(t, filepath.Join(f.parent, "keep-dir", "x"), "x", 0o644)
	before := snapshot(t, f.parent)
	o := f.opts(t)
	n := 0
	o.Copy.Hooks = &Hooks{BeforeWrite: func(rel string) error {
		if n++; n == 3 {
			return errors.New("injected")
		}
		return nil
	}}
	if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatal(err)
	}
	after := snapshot(t, f.parent)
	delete(before, f.parent)
	delete(after, f.parent)
	if len(after) != len(before) {
		t.Errorf("parent now holds %v", after)
	}
	for p, e := range before {
		if after[p] != e {
			t.Errorf("%s changed: %+v, was %+v", p, after[p], e)
		}
	}
	// A failure while making the copy safe too: the copy is swapped for a
	// file, which is not the directory the walk expects.
	o.Copy.Hooks = nil
	testHookAfterCopy = func(out string) {
		if err := os.Rename(out, out+".moved"); err != nil {
			t.Error(err)
		}
		mustWrite(t, out, "not a directory", 0o644)
	}
	t.Cleanup(func() { testHookAfterCopy = nil })
	if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "y"), o); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatal(err)
	}
	if got := names(t, f.parent); !slices.Equal(got, []string{"keep-dir", "keep-file"}) {
		t.Errorf("parent holds %v", got)
	}
}

func TestRestoreExposed(t *testing.T) {
	f := newRfix(t)
	f.write(t, "a", "A")
	r := f.take(t)
	o := f.opts(t)
	o.Live = f.live
	f.s.Exposed = func() error { return ErrExposed }
	if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o); !errors.Is(err, ErrExposed) {
		t.Errorf("exposed: %v", err)
	}
	f.s.Exposed = nil
	if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o); err == nil {
		t.Error("restored with no exposed-store check")
	}
	if len(names(t, f.parent)) != 0 || storeCount(t, f) != 1 {
		t.Error("written")
	}
}

func TestRestoreUnreadableMeta(t *testing.T) {
	f := newRfix(t)
	f.write(t, "a", "A")
	r := f.take(t)
	if err := os.WriteFile(filepath.Join(f.s.checkpointDir(r.ID), metaName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), f.opts(t)); err == nil {
		t.Error("restored a checkpoint whose meta.json cannot be read")
	}
	if _, err := f.restore(t, r.ID.Key+"@latest", filepath.Join(f.parent, "x"), f.opts(t)); err == nil {
		t.Error("latest resolved to an unreadable checkpoint")
	}
	if len(names(t, f.parent)) != 0 {
		t.Error("written")
	}
}

func TestRestoreTargetPolicyUnit(t *testing.T) {
	dir := tempTree(t)
	home := filepath.Join(dir, "home")
	w := filepath.Join(dir, "w")
	ro := filepath.Join(dir, "ro")
	for _, d := range []string{home, w, ro} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A second name for w.
	alias := filepath.Join(dir, "walias")
	mustSymlink(t, w, alias)
	set := PathSet{
		Writable: []EnvPath{{"e", alias}, {"m", filepath.Join(dir, "missing")}},
		ReadOnly: []EnvPath{{"r", ro}, {"e", ro}},
	}
	cases := []struct {
		target  string
		refused string
		readers []string
	}{
		{filepath.Join(w, "x"), "environment e can write", nil},
		{filepath.Join(w, "x", "y"), "environment e can write", nil},
		{filepath.Join(dir, "missing"), "environment m can write", nil},
		{filepath.Join(home, "x"), "inside CABOOSE_HOME", nil},
		{filepath.Join(ro, "x"), "", []string{"e", "r"}},
		{filepath.Join(dir, "wx"), "", nil},
		{filepath.Join(dir, "x"), "", nil},
	}
	for _, c := range cases {
		readers, err := restoreTargetPolicy(c.target, home, set)
		if c.refused != "" {
			if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), c.refused) {
				t.Errorf("%s: %v", c.target, err)
			}
			continue
		}
		if err != nil || !slices.Equal(readers, c.readers) {
			t.Errorf("%s: %v %v", c.target, readers, err)
		}
	}
}

// A sandbox that can write the target's parent swaps the copy, or the
// temporary directory holding it, for a symlink to a directory of its
// choosing while the copy is made safe: nothing there is touched.
func TestRestoreSwappedWhileSanitizing(t *testing.T) {
	for _, kind := range []string{"out", "tmp", "tmp-and-out"} {
		t.Run(kind, func(t *testing.T) {
			f := newRfix(t)
			f.write(t, "a", "A")
			mustWrite(t, filepath.Join(f.top, "proj", ".git", "config"), gitConfigWithCommand, 0o644)
			mustWrite(t, filepath.Join(f.top, "proj", ".git", "hooks", "pre-push"), "evil", 0o755)
			// A setuid file, so the sanitizer has something to write.
			f.write(t, "s", "S")
			if err := unix.Chmod(filepath.Join(f.top, "proj", "s"), 0o4755); err != nil {
				t.Fatal(err)
			}
			r := f.take(t)
			victimParent := tempTree(t)
			underTemp(t, victimParent)
			victim := filepath.Join(victimParent, restoreOut)
			mustWrite(t, filepath.Join(victim, ".git", "config"), gitConfigWithCommand, 0o644)
			mustWrite(t, filepath.Join(victim, ".git", "hooks", "pre-push"), "mine", 0o755)
			mustWrite(t, filepath.Join(victim, "a"), "victim", 0o644)
			mustWrite(t, filepath.Join(victim, "s"), "victim", 0o644)
			if err := unix.Chmod(filepath.Join(victim, "s"), 0o4755); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, victimParent)
			swapped, tmpPath := false, ""
			testHookSanitizeEntry = func(string) {
				if swapped {
					return
				}
				swapped = true
				m, _ := filepath.Glob(filepath.Join(f.parent, restoreTempPrefix+"*"))
				if len(m) != 1 {
					t.Errorf("temp dirs %v", m)
					return
				}
				tmp := m[0]
				tmpPath = tmp
				switch kind {
				case "out":
					if err := os.Rename(filepath.Join(tmp, restoreOut), filepath.Join(tmp, "real")); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(victim, filepath.Join(tmp, restoreOut)); err != nil {
						t.Error(err)
					}
				case "tmp", "tmp-and-out":
					if err := os.Rename(tmp, tmp+".real"); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(victimParent, tmp); err != nil {
						t.Error(err)
					}
					if kind == "tmp-and-out" {
						if err := os.Rename(filepath.Join(tmp+".real", restoreOut), filepath.Join(tmp+".real", "moved")); err != nil {
							t.Error(err)
						}
					}
				}
			}
			t.Cleanup(func() { testHookSanitizeEntry = nil })
			to := filepath.Join(f.parent, "x")
			_, err := f.restore(t, r.ID.String(), to, f.opts(t))
			if !swapped {
				t.Fatal("never swapped")
			}
			after := snapshot(t, victimParent)
			if len(after) != len(before) {
				t.Errorf("victim now holds %v", after)
			}
			for p, e := range before {
				if after[p] != e {
					t.Errorf("victim's %s changed: %+v, was %+v", p, after[p], e)
				}
			}
			switch kind {
			case "tmp":
				// The held directories carried on: the copy restored is the
				// real one, made safe.
				if err != nil {
					t.Fatal(err)
				}
				if readT(t, filepath.Join(to, "a")) != "A" || readT(t, filepath.Join(to, ".git", "config")) != gitConfigWithCommand {
					t.Error("restored copy")
				}
				if got := modeOf(t, filepath.Join(to, "s")); got != 0o755 {
					t.Errorf("restored setuid file mode %o", got)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "replaced") {
					t.Fatalf("%v", err)
				}
				if exists(to) {
					t.Error("target made")
				}
			}
			// What was at the temporary directory's path is gone, the
			// symlink planted there removed as a link.
			if _, err := os.Lstat(tmpPath); err == nil {
				t.Errorf("%s left", tmpPath)
			}
			for _, p := range []string{victimParent, victim} {
				if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
					t.Errorf("%s: %v", p, err)
				}
			}
		})
	}
}

// What the restore's copy leaves out is reported, not dropped silently.
func TestRestoreReportsSkipped(t *testing.T) {
	f := newRfix(t)
	f.write(t, "a", "a")
	f.write(t, "sub/c", "c")
	r := f.take(t)
	o := f.opts(t)
	o.Copy.Hooks = &Hooks{Dev: func(rel string, dev uint64) uint64 {
		if rel == "sub" {
			return dev + 1 // as if another filesystem: not descended
		}
		return dev
	}}
	res, err := f.restore(t, r.ID.String(), filepath.Join(f.parent, "x"), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || len(res.SkippedExamples) != 1 || res.SkippedExamples[0] != "sub" {
		t.Fatalf("skipped %d %v", res.Skipped, res.SkippedExamples)
	}
}
