package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
)

// newStore opens the store of environment "default" under a fresh
// CABOOSE_HOME in a t.TempDir.
func newStore(t *testing.T) *Store {
	t.Helper()
	home := t.TempDir()
	env := config.EnvDirFor(home, "default")
	if err := os.MkdirAll(env, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(home, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func lockT(t *testing.T, s *Store) *Lock {
	t.Helper()
	l, err := s.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Unlock() })
	return l
}

// take publishes a checkpoint of p stamped at, with a small tree.
func take(t *testing.T, s *Store, l *Lock, p string, at time.Time) ID {
	t.Helper()
	st, err := s.NextStamp(l, Key(p), at)
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := s.NewTemp(l, Key(p), st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, TreeName, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, TreeName, "sub", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := s.Publish(l, tmp, p, Meta{Kind: KindDir, Method: MethodCopy, Reason: ReasonInterval, Started: at, Finished: at})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOpenRefusals(t *testing.T) {
	t.Chdir(t.TempDir()) // relative paths below stay in a directory of the test's
	home := t.TempDir()
	env := config.EnvDirFor(home, "default")
	if err := os.MkdirAll(env, 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	otherEnv := config.EnvDirFor(other, "default")
	if err := os.MkdirAll(otherEnv, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"empty home":     {"", env},
		"relative home":  {"rel/home", env},
		"empty env":      {home, ""},
		"relative env":   {home, "envs/default"},
		"unclean env":    {home, env + "/"},
		"dotted env":     {home, home + "/envs/../envs/default"},
		"bad env name":   {home, home + "/envs/Bad Name"},
		"not under home": {home, otherEnv},
		"not envs dir":   {home, home + "/other/default"},
		"missing env":    {home, config.EnvDirFor(home, "missing")},
	}
	for name, c := range cases {
		if _, err := Open(c[0], c[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An environment directory that is a symlink resolving outside home.
	linked := config.EnvDirFor(home, "linked")
	if err := os.Symlink(otherEnv, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home, linked); err == nil || !strings.Contains(err.Error(), "not inside") {
		t.Errorf("store outside home accepted: %v", err)
	}
	if _, err := os.Lstat(StoreDir(otherEnv)); err == nil {
		t.Error("store created outside home")
	}
	// A store that is a symlink.
	sl := config.EnvDirFor(home, "sl")
	if err := os.MkdirAll(sl, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), StoreDir(sl)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home, sl); err == nil {
		t.Error("symlinked store accepted")
	}
	// The good one, created 0700, and opened again.
	s, err := Open(home, env)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(s.Dir())
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("store: %v %v", fi, err)
	}
	if _, err := Open(home, env); err != nil {
		t.Fatal(err)
	}
	// A home given through a symlink is resolved.
	hl := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(home, hl); err != nil {
		t.Fatal(err)
	}
	if s2, err := Open(hl, config.EnvDirFor(hl, "default")); err != nil || s2.Dir() != s.Dir() {
		t.Fatalf("home through a symlink: %v", err)
	}
}

func TestPublishAndList(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/work/dev/org/proj"
	at := time.Date(2026, 10, 17, 23, 10, 5, 0, time.UTC)
	id1 := take(t, s, l, p, at)
	id2 := take(t, s, l, p, at) // same second: a second later
	if id1.String() != Key(p)+"@20261017T231005Z" || id2.String() != Key(p)+"@20261017T231006Z" {
		t.Fatalf("ids %s %s", id1, id2)
	}
	es, err := s.List(Key(p))
	if err != nil || len(es) != 2 || !es[0].Stamp.Equal(id2.Stamp) || es[0].Err != nil || es[0].Meta.Path != p || es[0].Meta.Version != 1 {
		t.Fatalf("list %+v %v", es, err)
	}
	ks, err := s.Keys()
	if err != nil || len(ks) != 1 || ks[0].Key != Key(p) || ks[0].Path != p {
		t.Fatalf("keys %+v %v", ks, err)
	}
	if e, err := s.Resolve(ID{Key: Key(p), Latest: true}); err != nil || !e.Stamp.Equal(id2.Stamp) {
		t.Fatalf("resolve latest %v %v", e, err)
	}
	kd := filepath.Join(s.Dir(), Key(p))
	for _, f := range []string{kd, filepath.Join(kd, pathName), filepath.Join(kd, "20261017T231005Z", metaName)} {
		fi, err := os.Lstat(f)
		if err != nil {
			t.Fatal(err)
		}
		if want := os.FileMode(0o700); fi.IsDir() && fi.Mode().Perm() != want || !fi.IsDir() && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o", f, fi.Mode().Perm())
		}
	}
	if e, _ := os.ReadDir(kd); len(e) != 3 {
		t.Errorf("key dir holds %d entries, want path.json and two", len(e))
	}
	if _, err := s.List("../x-12345678"); err == nil {
		t.Error("bad key listed")
	}
	if es, err := s.List(Key("/none")); err != nil || len(es) != 0 {
		t.Errorf("missing key: %v %v", es, err)
	}
}

func TestPathJSONMismatch(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/work/a/x"
	k := Key(p)
	if err := os.Mkdir(filepath.Join(s.Dir(), k), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), k, pathName), []byte(`{"version":1,"path":"/work/b/x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp, err := s.NewTemp(l, k, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(l, tmp, p, Meta{}); err == nil || !strings.Contains(err.Error(), "/work/b/x") {
		t.Fatalf("mismatch accepted: %v", err)
	}
	if es, _ := s.List(k); len(es) != 0 {
		t.Fatal("published under a mismatched key")
	}
	if ks, _ := s.Keys(); len(ks) != 0 {
		t.Fatalf("a mismatched key listed: %v", ks)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir(), logName))
	if !strings.Contains(string(b), "refused") {
		t.Errorf("not logged: %q", b)
	}
}

func TestAtomicAppearance(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "~/.claude"
	k := Key(p)
	at := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	tmp, err := s.NewTemp(l, k, at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(tmp), ".tmp-"+k+"-20261017T010203Z-") || filepath.Dir(tmp) != s.Dir() {
		t.Fatalf("temp %s", tmp)
	}
	// A full tree, meta.json included, inside the .tmp-: never listed.
	if err := os.MkdirAll(filepath.Join(tmp, TreeName, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, metaName), []byte(`{"version":1,"id":"`+k+`@20261017T010203Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.Dir(), k), 0o700); err != nil {
		t.Fatal(err)
	}
	// A stamp directory without meta.json, and junk names.
	for _, d := range []string{"20261017T000000Z", "notastamp", "20261017T000001Z"} {
		if err := os.MkdirAll(filepath.Join(s.Dir(), k, d, TreeName), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), k, "notastamp", metaName), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A meta.json that is a symlink is listed as unreadable, not followed.
	if err := os.Symlink(filepath.Join(tmp, metaName), filepath.Join(s.Dir(), k, "20261017T000001Z", metaName)); err != nil {
		t.Fatal(err)
	}
	es, err := s.List(k)
	if err != nil || len(es) != 1 || es[0].Err == nil {
		t.Fatalf("list %+v %v", es, err)
	}
	// Publish refuses: meta.json is already there (exclusive create).
	if _, err := s.Publish(l, tmp, p, Meta{}); err == nil {
		t.Fatal("published over an existing meta.json")
	}
	if err := os.Remove(filepath.Join(tmp, metaName)); err != nil {
		t.Fatal(err)
	}
	id, err := s.Publish(l, tmp, p, Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Error("temp still there")
	}
	es, _ = s.List(k)
	if len(es) != 2 || !es[0].ID.Stamp.Equal(id.Stamp) || es[0].Err != nil {
		t.Fatalf("after publish %+v", es)
	}
	// Publishing onto an existing checkpoint is refused.
	tmp2, err := s.NewTemp(l, k, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(l, tmp2, p, Meta{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrote: %v", err)
	}
	// Publish of a directory NewTemp did not make, or for another path.
	if _, err := s.Publish(l, filepath.Join(s.Dir(), k), p, Meta{}); err == nil {
		t.Error("published a key dir")
	}
	if _, err := s.Publish(l, tmp2, "/other", Meta{}); err == nil {
		t.Error("published for another path")
	}
}

func TestMetaUnreadableAndNewer(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	p := "/w/x"
	id := take(t, s, l, p, time.Now())
	mp := filepath.Join(s.checkpointDir(id), metaName)
	for name, content := range map[string]string{
		"newer":   `{"version":2,"id":"` + id.String() + `"}`,
		"garbage": `{`,
		"other":   `{"version":1,"id":"x-12345678@20200101T000000Z"}`,
		"large":   `{"version":1,"x":"` + strings.Repeat("a", maxMetaSize) + `"}`,
	} {
		if err := os.WriteFile(mp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		es, err := s.List(Key(p))
		if err != nil || len(es) != 1 || es[0].Err == nil {
			t.Fatalf("%s: %+v %v", name, es, err)
		}
		if name == "newer" && !errors.Is(es[0].Err, ErrNewerVersion) {
			t.Errorf("newer: %v", es[0].Err)
		}
	}
}

func TestNextStamp(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	at := time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("x", 7200))
	k := Key("/p")
	st, err := s.NextStamp(l, k, at)
	if err != nil || FormatStamp(st) != "20260102T010405Z" || st.Nanosecond() != 0 {
		t.Fatalf("%v %v", st, err)
	}
	// A leftover stamp directory, complete or not, is never reused.
	if err := os.MkdirAll(filepath.Join(s.Dir(), k, "20260102T010405Z"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), k, "20260102T010406Z"), 0o700); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.NextStamp(l, k, at); FormatStamp(st) != "20260102T010407Z" {
		t.Fatalf("got %v", st)
	}
}

func TestLockRules(t *testing.T) {
	s := newStore(t)
	l, err := s.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TryLock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock while held: %v", err)
	}
	start := time.Now()
	if _, err := s.Lock(context.Background(), 200*time.Millisecond); !errors.Is(err, ErrLocked) || time.Since(start) < 200*time.Millisecond || time.Since(start) > 5*time.Second {
		t.Fatalf("Lock while held: %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Lock(ctx, time.Hour); !errors.Is(err, ErrLocked) {
		t.Fatalf("Lock with a done ctx: %v", err)
	}
	other := newStore(t)
	if !l.Held(s) || l.Held(other) {
		t.Fatal("Held")
	}
	ol := lockT(t, other)
	if _, err := s.NewTemp(ol, Key("/p"), time.Now()); err == nil {
		t.Error("another store's lock accepted")
	}
	if _, err := s.CleanLeftovers(ol, time.Now()); err == nil {
		t.Error("another store's lock accepted by CleanLeftovers")
	}
	if _, err := s.Prune(ol, Key("/p"), DefaultRules(time.Now()), nil); err == nil {
		t.Error("another store's lock accepted by Prune")
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if l.Held(s) {
		t.Fatal("released lock held")
	}
	if _, err := s.Prune(l, Key("/p"), DefaultRules(time.Now()), nil); err == nil {
		t.Error("released lock accepted")
	}
	if _, err := s.Publish(nil, filepath.Join(s.Dir(), ".tmp-x"), "/p", Meta{}); err == nil {
		t.Error("nil lock accepted")
	}
	l2, err := s.Lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l2.Unlock()
	l2.Unlock()
}

// TestTwoTakers runs takers that each lock, check whether a checkpoint is
// due (none newer than ten minutes), take one if so and unlock: exactly one
// checkpoint results.
// TestPublishLockCheck checks Publish refuses a released lock and another
// store's at the lock check, with everything else in order.
func TestPublishLockCheck(t *testing.T) {
	s := newStore(t)
	other := newStore(t)
	ol := lockT(t, other)
	l, err := s.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	p := "/w/x"
	tmp, err := s.NewTemp(l, Key(p), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ol, tmp, p, Meta{}); err == nil || !strings.Contains(err.Error(), "lock of another store") {
		t.Fatalf("another store's lock: %v", err)
	}
	l.Unlock()
	if _, err := s.Publish(l, tmp, p, Meta{}); err == nil || !strings.Contains(err.Error(), "lock was released") {
		t.Fatalf("released lock: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tmp, metaName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("meta.json written without the lock")
	}
	if es, _ := s.List(Key(p)); len(es) != 0 {
		t.Error("published without the lock")
	}
	l2 := lockT(t, s)
	if _, err := s.Publish(l2, tmp, p, Meta{}); err != nil {
		t.Fatalf("with the lock: %v", err)
	}
}

func TestTwoTakers(t *testing.T) {
	s := newStore(t)
	p := "/work/dev/x"
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := s.Lock(context.Background(), 10*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer l.Unlock()
			es, err := s.List(Key(p))
			if err != nil {
				errs <- err
				return
			}
			if len(es) > 0 && now.Sub(es[0].Stamp) < 10*time.Minute {
				return
			}
			st, err := s.NextStamp(l, Key(p), now)
			if err != nil {
				errs <- err
				return
			}
			tmp, err := s.NewTemp(l, Key(p), st)
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(5 * time.Millisecond) // widen any window
			if _, err := s.Publish(l, tmp, p, Meta{}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if es, _ := s.List(Key(p)); len(es) != 1 {
		t.Fatalf("%d checkpoints, want 1", len(es))
	}
}

func skipRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
}

func TestCleanLeftovers(t *testing.T) {
	skipRoot(t)
	s := newStore(t)
	l := lockT(t, s)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Old, so a check that followed the symlink would find an old leftover.
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatal(err)
	}
	mkd := func(name string, mtime time.Time) string {
		d := filepath.Join(s.Dir(), name)
		if err := os.MkdirAll(filepath.Join(d, "tree", "ro", "zero"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "tree", "ro", "zero", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(d, "tree", "out")); err != nil {
			t.Fatal(err)
		}
		os.Chmod(filepath.Join(d, "tree", "ro", "zero"), 0)
		os.Chmod(filepath.Join(d, "tree", "ro"), 0o555)
		if err := os.Chtimes(d, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return d
	}
	oldTmp := mkd(".tmp-a-12345678-20261017T000000Z-1", old)
	oldTrash := mkd(".trash-a-12345678-20261017T000000Z-2", old)
	fresh := mkd(".tmp-a-12345678-20261017T000000Z-3", now.Add(-59*time.Minute))
	t.Cleanup(func() {
		os.Chmod(filepath.Join(fresh, "tree", "ro"), 0o700)
		os.Chmod(filepath.Join(fresh, "tree", "ro", "zero"), 0o700)
	})
	evil := filepath.Join(s.Dir(), ".tmp-evil")
	if err := os.Symlink(outside, evil); err != nil {
		t.Fatal(err)
	}
	os.Chmod(outside, 0o555)
	t.Cleanup(func() { os.Chmod(outside, 0o700) })
	s.Exposed = func() error { return errors.New("the store is inside root R") }
	if _, err := s.CleanLeftovers(l, now); err == nil || !strings.Contains(err.Error(), "inside root R") {
		t.Fatalf("exposed store cleaned: %v", err)
	}
	if _, err := os.Lstat(oldTmp); err != nil {
		t.Fatal("removed while exposed")
	}
	s.Exposed = nil
	removed, err := s.CleanLeftovers(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
	for _, d := range []string{oldTmp, oldTrash} {
		if _, err := os.Lstat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s kept", d)
		}
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Error("fresh leftover removed")
	}
	if fi, err := os.Lstat(evil); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink leftover touched")
	}
	if fi, err := os.Stat(outside); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("outside changed: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Error("outside content removed")
	}
}

// CleanLeftovers removes only .tmp- and .trash- directories: a key's
// directory, its checkpoints, the log and the lock stay, however old.
func TestCleanLeftoversKeepsCheckpoints(t *testing.T) {
	s := newStore(t)
	l := lockT(t, s)
	at := time.Now().Add(-3 * time.Hour)
	id := take(t, s, l, "/work/dev/proj", at)
	s.Logf("a line")
	other := filepath.Join(s.Dir(), "not-a-leftover")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	kd := filepath.Join(s.Dir(), id.Key)
	cd := filepath.Join(kd, FormatStamp(id.Stamp))
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{cd, kd, other, filepath.Join(s.Dir(), lockName), filepath.Join(s.Dir(), logName)} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.CleanLeftovers(l, time.Now())
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed %v (%v)", removed, err)
	}
	for _, p := range []string{filepath.Join(cd, metaName), filepath.Join(cd, TreeName, "sub", "f"), filepath.Join(kd, pathName), other,
		filepath.Join(s.Dir(), lockName), filepath.Join(s.Dir(), logName)} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if es, err := s.List(id.Key); err != nil || len(es) != 1 {
		t.Errorf("listed %v (%v)", es, err)
	}
}
