package apkobuild

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	pkglock "chainguard.dev/apko/pkg/lock"
	"golang.org/x/sys/unix"
)

// A package apko cached for a build is found by the lock that built it:
// pruning with that lock keeps it, pruning with none removes it, unless it
// is newer than recent.
func TestPruneCacheMatchesApko(t *testing.T) {
	ctx := context.Background()
	o := fixtureOptions(newFixture(t, "x86_64", "1.0"), "x86_64")
	o.CacheDir = t.TempDir()
	l, err := resolveList(ctx, Spec{Packages: []string{"fixture-hello"}}, []string{"fixture-hello"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, l, o, "fixture:test", io.Discard); err != nil {
		t.Fatal(err)
	}
	k, err := cacheKey(l.l.Contents.Packages[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(o.CacheDir, k)
	if fi, err := os.Lstat(pkg); err != nil || !fi.IsDir() {
		t.Fatalf("apko did not cache the package at %s: %v", pkg, err)
	}

	now := time.Now()
	if r, err := PruneCache(o.CacheDir, []*Lock{l}, now); err != nil || r != (CachePrune{}) {
		t.Errorf("with its lock: %+v, %v", r, err)
	}
	if r, err := PruneCache(o.CacheDir, nil, now.Add(-time.Hour)); err != nil || r != (CachePrune{}) {
		t.Errorf("a recent package: %+v, %v", r, err)
	}
	if _, err := os.Lstat(pkg); err != nil {
		t.Fatalf("removed: %v", err)
	}
	r, err := PruneCache(o.CacheDir, nil, now)
	if err != nil || r.Packages != 1 || r.Indexes != 0 || r.Bytes <= 0 {
		t.Errorf("with no lock: %+v, %v", r, err)
	}
	if _, err := os.Lstat(pkg); err == nil {
		t.Error("the package is still there")
	}
}

// A fake cache: what no lock names goes, old indexes go but the newest,
// keys and what PruneCache does not recognise stay, and no link is
// followed out of the cache.
func TestPruneCacheTree(t *testing.T) {
	tmp := t.TempDir()
	cache, outside := filepath.Join(tmp, "cache"), filepath.Join(tmp, "outside")
	old := time.Now().Add(-48 * time.Hour)
	file := func(rel, content string) {
		t.Helper()
		p := filepath.Join(cache, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, rel string) {
		t.Helper()
		p := filepath.Join(cache, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(outside, "pkg-1-r0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "victim"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := "https%3A%2F%2Fpackages.example.invalid%2Fos"
	arch := repo + "/aarch64/"
	file(arch+"kept-1.0-r0/expand/stream.tar.gz", "kept")
	link("expand/stream.tar.gz", arch+"kept-1.0-r0/abc.dat.tar.gz")
	file(arch+"gone-2.0-r1/expand/stream.tar.gz", "12345")
	link("expand/stream.tar.gz", arch+"gone-2.0-r1/abc.dat.tar.gz")
	link(filepath.Join(outside, "victim"), arch+"gone-2.0-r1/out.ctl.tar.gz")
	link(filepath.Join(outside, "pkg-1-r0"), arch+"linked-1-r0")
	file(arch+"APKINDEX/1.tmp", "old index")
	link("1.tmp", arch+"APKINDEX/AAAA.tar.gz")
	file(arch+"APKINDEX/2.tmp", "new index!")
	link("2.tmp", arch+"APKINDEX/BBBB.tar.gz")
	file(arch+"APKINDEX/3.tmp", "orphan")
	file(arch+"notes-r0", "a file, not a package")
	file(arch+"something/else", "unknown")
	file("https%3A%2F%2Fpackages.example.invalid%2F/os/key.rsa.pub/1.tmp", "key")
	link("1.tmp", "https%3A%2F%2Fpackages.example.invalid%2F/os/key.rsa.pub/K.etag")

	// Everything is old, the newest index by a minute less.
	if err := filepath.WalkDir(cache, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return lutimes(p, old)
	}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{arch + "APKINDEX/BBBB.tar.gz", arch + "APKINDEX/AAAA.tar.gz"} {
		mt := old
		if rel == arch+"APKINDEX/BBBB.tar.gz" {
			mt = old.Add(time.Minute)
		}
		if err := lutimes(filepath.Join(cache, rel), mt); err != nil {
			t.Fatal(err)
		}
	}

	keep := lockOf(t, "https://packages.example.invalid/os/aarch64/kept-1.0-r0.apk")
	r, err := PruneCache(cache, []*Lock{keep}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r.Packages != 1 || r.Indexes != 1 || r.Bytes != int64(len("12345")+len("old index")+len("orphan")) {
		t.Errorf("result %+v", r)
	}
	var left []string
	if err := filepath.WalkDir(cache, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(cache, p)
			left = append(left, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		CacheLockFile,
		"https%3A%2F%2Fpackages.example.invalid%2F/os/key.rsa.pub/1.tmp",
		"https%3A%2F%2Fpackages.example.invalid%2F/os/key.rsa.pub/K.etag",
		arch + "APKINDEX/2.tmp",
		arch + "APKINDEX/BBBB.tar.gz",
		arch + "kept-1.0-r0/abc.dat.tar.gz",
		arch + "kept-1.0-r0/expand/stream.tar.gz",
		arch + "linked-1-r0",
		arch + "notes-r0",
		arch + "something/else",
	}
	slices.Sort(left)
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Errorf("left:\n%q\nwant:\n%q", left, want)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "victim")); err != nil || string(b) != "keep me" {
		t.Errorf("followed a link out of the cache: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "pkg-1-r0")); err != nil {
		t.Errorf("followed a linked dir out of the cache: %v", err)
	}

	// A missing cache is an empty one; a lock with a URL that names no
	// package prunes nothing.
	if r, err := PruneCache(filepath.Join(tmp, "none"), nil, time.Now()); err != nil || r != (CachePrune{}) {
		t.Errorf("missing cache: %+v, %v", r, err)
	}
	if _, err := PruneCache(cache, []*Lock{lockOf(t, "https://packages.example.invalid/os/aarch64/")}, time.Now()); err == nil {
		t.Error("a lock naming no .apk pruned")
	}
	if _, err := os.Lstat(filepath.Join(cache, arch+"kept-1.0-r0")); err != nil {
		t.Error("pruned after a bad lock")
	}
}

func TestCacheKey(t *testing.T) {
	for in, want := range map[string]string{
		"https://packages.wolfi.dev/os/x86_64/jq-1.7-r0.apk":      "https%3A%2F%2Fpackages.wolfi.dev%2Fos/x86_64/jq-1.7-r0",
		"https://h.invalid/a/b/aarch64/x-1-r2.apk?token=1#frag":   "https%3A%2F%2Fh.invalid%2Fa%2Fb/aarch64/x-1-r2",
		"http://fixture.invalid/os/x86_64/fixture-hello-1-r0.apk": "http%3A%2F%2Ffixture.invalid%2Fos/x86_64/fixture-hello-1-r0",
	} {
		if got, err := cacheKey(in); err != nil || got != want {
			t.Errorf("cacheKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// lockOf is a lock of one package at url.
func lockOf(t *testing.T, url string) *Lock {
	t.Helper()
	l, err := newLock(pkglock.Lock{
		Version:  "v1",
		Config:   &pkglock.Config{Name: ConfigName(Spec{Packages: []string{"p"}}, []string{"p"})},
		Contents: pkglock.LockContents{Packages: []pkglock.LockPkg{{Name: "p", URL: url, Version: "1-r0", Architecture: "aarch64"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// lutimes sets the modification time of p itself, a link or not.
func lutimes(p string, mt time.Time) error {
	tv := unix.NsecToTimeval(mt.UnixNano())
	return unix.Lutimes(p, []unix.Timeval{tv, tv})
}

// While anything holds the cache's lock shared (a build using it), a
// prune removes nothing and says so; once it is released, the prune does.
func TestPruneCacheSkipsWhileTheCacheIsInUse(t *testing.T) {
	cache := t.TempDir()
	pkg := filepath.Join(cache, "https%3A%2F%2Fpackages.example.invalid%2Fos", "x86_64", "old-1.0-r0")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(pkg, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	r, err := PruneCache(cache, nil, time.Now().Add(-time.Hour))
	if !errors.Is(err, ErrCacheInUse) || r != (CachePrune{}) {
		t.Errorf("while in use: %+v, %v", r, err)
	}
	if _, err := os.Lstat(pkg); err != nil {
		t.Fatalf("removed while in use: %v", err)
	}
	unlock()
	if r, err := PruneCache(cache, nil, time.Now().Add(-time.Hour)); err != nil || r.Packages != 1 {
		t.Errorf("after: %+v, %v", r, err)
	}
	if _, err := os.Lstat(pkg); err == nil {
		t.Error("the package is still there")
	}
}

// The lock file is never opened through a symlink, nor used when it is no
// plain file.
func TestCacheLockRefusesALink(t *testing.T) {
	cache := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(target, filepath.Join(cache, CacheLockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LockCache(cache); err == nil {
		t.Error("locked through a symlink")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("created the symlink's target: %v", err)
	}
	if _, err := PruneCache(cache, nil, time.Now()); err == nil {
		t.Error("pruned with a symlink for a lock")
	}
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, CacheLockFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LockCache(other); err == nil {
		t.Error("locked a directory")
	}
	fresh := t.TempDir()
	unlock, err := LockCache(fresh)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if fi, err := os.Lstat(filepath.Join(fresh, CacheLockFile)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("lock file %v %v", fi, err)
	}
}
