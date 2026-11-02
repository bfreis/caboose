package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/bfreis/caboose/internal/agentproto"
	"golang.org/x/sys/unix"
)

// watchMask is what the tests' watches ask inotify for.
const watchMask = unix.IN_ATTRIB | unix.IN_MODIFY

// foreignMask are the bits of an event that no watch of watchMask is
// sent by this kernel's own inotify: what it reports carries only the
// bits asked for and these flags of its own. A file system shared with
// another machine (a Mac's directory in a Linux VM, as OrbStack's
// /work is) may also deliver that machine's notifications of what was
// written, late and with bits never asked for (IN_CLOSE_WRITE for the
// write a test made before it watched); those are not this kernel's
// answer to a touch, and events leaves them out.
const foreignMask = ^uint32(watchMask | unix.IN_ISDIR | unix.IN_IGNORED | unix.IN_Q_OVERFLOW | unix.IN_UNMOUNT)

// events reads what an inotify fd has queued, as "name:mask" in hex, "."
// for the watched directory itself, but for an event carrying any bit of
// foreignMask.
func events(t *testing.T, fd int) []string {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if err == unix.EAGAIN {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for off := 0; off+unix.SizeofInotifyEvent <= n; {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
		name := strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+int(ev.Len)]), "\x00")
		if name == "" {
			name = "."
		}
		if ev.Mask&foreignMask == 0 {
			out = append(out, name+":"+fmt.Sprintf("%x", ev.Mask))
		} else {
			t.Logf("left out %s:%x, an event this kernel's inotify does not send a watch of %x", name, ev.Mask, watchMask)
		}
		off += unix.SizeofInotifyEvent + int(ev.Len)
	}
	return out
}

func TestTouch(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(work, "f")
	must(os.WriteFile(file, []byte("x"), 0o640))
	past := time.Unix(1_000_000_000, 0)
	must(os.Chtimes(file, past, past))
	must(os.Mkdir(filepath.Join(work, "d"), 0o755))
	must(os.WriteFile(filepath.Join(outside, "secret"), nil, 0o600))
	must(os.Symlink(filepath.Join(outside, "secret"), filepath.Join(work, "link")))
	must(unix.Mkfifo(filepath.Join(work, "fifo"), 0o644))

	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	must(err)
	defer unix.Close(fd)
	for _, d := range []string{work, outside} {
		_, err := unix.InotifyAddWatch(fd, d, watchMask)
		must(err)
	}

	const attrib, dirAttrib = "4", "40000004"
	cases := []struct {
		paths []string
		want  []string
	}{
		{[]string{file}, []string{"f:" + attrib}},
		{[]string{filepath.Join(work, "d")}, []string{"d:" + dirAttrib}},
		// Gone: its directory is touched, once for all its gone paths.
		{[]string{filepath.Join(work, "d", "gone1"), filepath.Join(work, "d", "gone2")}, []string{"d:" + dirAttrib}},
		{[]string{filepath.Join(work, "link")}, nil},
		{[]string{filepath.Join(work, "fifo")}, nil},
		{[]string{filepath.Join(outside, "secret"), work + "/../" + filepath.Base(outside) + "/secret", "relative"}, nil},
	}
	for _, c := range cases {
		Touch([]string{work}, c.paths)
		if got := events(t, fd); !slices.Equal(got, c.want) {
			t.Errorf("Touch(%v): events %v, want %v", c.paths, got, c.want)
		}
	}
	fi, err := os.Stat(file)
	must(err)
	if !fi.ModTime().Equal(past) || fi.Mode().Perm() != 0o640 {
		t.Errorf("touching changed the file: mtime %v, mode %v", fi.ModTime(), fi.Mode())
	}
}

// Touch returns what it found of the paths under roots: the ones it
// touched and the ones it leaves alone, not the gone or the outside ones.
func TestTouchReturnsWhatItFound(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(work, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(work, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "o")
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := Touch([]string{work}, []string{file, filepath.Join(work, "gone"), link, outside, work})
	if want := []string{file, link, work}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}

func TestRechecksDueInOrder(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := rechecks{max: 10}
	if _, ok := r.next(); ok {
		t.Fatal("next with nothing waiting")
	}
	r.add([]string{"/a", "/b"}, t0.Add(time.Second))
	r.add([]string{"/c"}, t0.Add(2*time.Second))
	if at, ok := r.next(); !ok || !at.Equal(t0.Add(time.Second)) {
		t.Fatalf("next: %v %v", at, ok)
	}
	if got := r.due(t0.Add(time.Second - 1)); len(got) != 0 {
		t.Fatalf("due before their time: %q", got)
	}
	if got := r.due(t0.Add(time.Second)); !slices.Equal(got, []string{"/a", "/b"}) {
		t.Fatalf("due at 1 s: %q", got)
	}
	if got := r.due(t0.Add(time.Hour)); !slices.Equal(got, []string{"/c"}) {
		t.Fatalf("due later: %q", got)
	}
	if _, ok := r.next(); ok {
		t.Fatal("next after all were taken")
	}
}

func TestRechecksDropTheOldestPastMax(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := rechecks{max: 2}
	r.add([]string{"/a"}, t0)
	r.add([]string{"/b", "/c"}, t0.Add(time.Second))
	if got := r.due(t0.Add(time.Hour)); !slices.Equal(got, []string{"/b", "/c"}) {
		t.Fatalf("got %q, want the newest 2", got)
	}
}

// A path lstat found is looked up, and goes to the rechecks, even when
// what touch does next fails: the guest has it cached all the same.
func TestTouchFindsWhatItCannotOpen(t *testing.T) {
	// A sysctl written only: opening it to read fails with EACCES, root
	// or not.
	const p = "/proc/sys/vm/drop_caches"
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == nil {
		unix.Close(fd)
		t.Skipf("%s opens here: no failing open to test with", p)
	}
	if looked, err := touch(p); !looked || err == nil {
		t.Fatalf("touch(%s) = %v, %v: want it looked up, and the open failing", p, looked, err)
	}
	if got := Touch([]string{filepath.Dir(p)}, []string{p}); !slices.Equal(got, []string{p}) {
		t.Fatalf("found %q, want %s", got, p)
	}
}

// looks records a rechecker's lookups, finding the paths in found.
type looks struct {
	mu    sync.Mutex
	found map[string]bool
	got   []string
}

func (l *looks) look(p string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, p)
	return l.found[p]
}

func (l *looks) of(p string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, g := range l.got {
		if g == p {
			n++
		}
	}
	return n
}

// waitLooks waits until p was looked up n times, failing past a second.
func (l *looks) wait(t *testing.T, p string, n int) {
	t.Helper()
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if l.of(p) >= n {
			return
		}
	}
	t.Fatalf("%s looked up %d times, want %d (all: %q)", p, l.of(p), n, l.got)
}

const testAfter = 30 * time.Millisecond

func TestRecheckerLooksUpAgainWhenDue(t *testing.T) {
	l := &looks{found: map[string]bool{"/g": true}}
	r := newRechecker(testAfter, 10, l.look)
	done := make(chan struct{})
	defer close(done)
	go r.run(done)

	t0 := time.Now()
	r.add([]string{"/a"}, nil, t0)
	l.wait(t, "/a", 1)
	if d := time.Since(t0); d < testAfter {
		t.Fatalf("/a looked up again after %v, before its %v", d, testAfter)
	}
	// Once the timer has fired and stopped, a new path still comes due.
	r.add([]string{"/b"}, nil, time.Now())
	l.wait(t, "/b", 1)

	// A gone path is looked up at once, and once more later when the
	// guest still found it.
	t1 := time.Now()
	r.add(nil, []string{"/g", "/h"}, t1)
	l.wait(t, "/h", 1)
	l.wait(t, "/g", 2)
	if d := time.Since(t1); d < testAfter {
		t.Fatalf("/g looked up twice within %v, before its %v", d, testAfter)
	}
	time.Sleep(3 * testAfter)
	if n := l.of("/h"); n != 1 {
		t.Fatalf("/h, not found, was looked up %d times, want 1", n)
	}
	if n := l.of("/a"); n != 1 {
		t.Fatalf("/a was looked up %d times, want 1", n)
	}
}

// The touch loop hands what it found, and the gone paths under the
// roots, to the rechecker.
func TestTouchChangedRechecks(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(work, "f")
	stale := filepath.Join(work, "stale")
	for _, p := range []string{file, stale} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pr, pw := io.Pipe()
	sess := agentproto.NewSession(pr, pw, false)
	defer sess.Close()
	l := &Link{sess: sess, changed: make(chan changes, 1), roots: []string{work}}
	lk := &looks{found: map[string]bool{file: true, stale: true}}
	l.recheck = newRechecker(testAfter, 10, lk.look)
	go l.recheck.run(sess.Done())
	go l.touchChanged()

	gone := filepath.Join(work, "gone")
	outside := filepath.Join(t.TempDir(), "o")
	// stale is gone on the host, and the guest still finds it.
	l.changed <- changes{paths: []string{file, gone}, gone: []string{stale, outside}}
	lk.wait(t, file, 1)
	lk.wait(t, stale, 2)
	time.Sleep(3 * testAfter)
	if n, m := lk.of(gone), lk.of(outside); n != 0 || m != 0 {
		t.Fatalf("looked up %d times what Touch did not find, %d times what is outside the roots", n, m)
	}
}

func TestDropLogSaysTheFirstThenOnceAMinute(t *testing.T) {
	var d dropLog
	t0 := time.Unix(1_000_000, 0)
	if n, ok := d.note(t0); !ok || n != 1 {
		t.Fatalf("first drop: %d, %v", n, ok)
	}
	if _, ok := d.note(t0.Add(time.Second)); ok {
		t.Fatal("a second drop within a minute was said")
	}
	if n, ok := d.note(t0.Add(dropLogEvery + time.Second)); !ok || n != 2 {
		t.Fatalf("a minute on: %d, %v, want the 2 since", n, ok)
	}
}

func TestRecheckerDropsTheOldestGonePastMax(t *testing.T) {
	l := &looks{}
	r := newRechecker(testAfter, 2, l.look)
	r.add(nil, []string{"/a", "/b", "/c"}, time.Now())
	r.add(nil, []string{"/d"}, time.Now())
	done := make(chan struct{})
	defer close(done)
	go r.run(done)
	l.wait(t, "/c", 1)
	l.wait(t, "/d", 1)
	time.Sleep(testAfter)
	if a, b := l.of("/a"), l.of("/b"); a != 0 || b != 0 {
		t.Fatalf("the oldest gone were looked up %d and %d times, want dropped (all: %q)", a, b, l.got)
	}
}

func TestLinkConfigRechecksUnderVMAlone(t *testing.T) {
	for iso, want := range map[string]bool{"vm": true, "container": false, "gvisor": false, "": false} {
		if got := linkConfig(iso).Recheck; got != want {
			t.Errorf("CABOOSE_ISOLATION=%q: Recheck = %v, want %v", iso, got, want)
		}
	}
}

// runRecheckLink runs the link with recheck as given, a lookup that
// records into lk, and the host's hello naming root; it gives the host's
// end of the session.
func runRecheckLink(t *testing.T, recheck bool, root string, lk *looks) *agentproto.Session {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	dir, err := os.MkdirTemp("", "cbr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	sess := agentproto.NewSession(hr, hw, true)
	go RunLink(ar, aw, Config{Socket: sock, ProcRoot: procFixture(t), Interval: time.Hour, Recheck: recheck, Look: lk.look})
	t.Cleanup(func() { sess.Close() })
	select {
	case b := <-sess.Control():
		if m, err := agentproto.Decode(b); err != nil || m.Type != agentproto.TypeHello {
			t.Fatalf("first message %+v, %v, want a hello", m, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no hello from the link")
	}
	sess.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, Roots: []string{root}})
	waitSocket(t, sock)
	return sess
}

// A change message's Gone paths cross the link to the rechecker, which
// looks them up at once, when the link rechecks; and nothing is looked up
// when it does not.
func TestLinkLooksUpTheGonePathsOfAChangeMessage(t *testing.T) {
	for _, recheck := range []bool{true, false} {
		root := t.TempDir()
		gone := filepath.Join(root, "gone")
		lk := &looks{}
		sess := runRecheckLink(t, recheck, root, lk)
		// The hello is read in order with the changes: send until seen.
		for i := 0; i < 50 && lk.of(gone) == 0; i++ {
			sess.Send(agentproto.Message{Type: agentproto.TypeChanged, Gone: []string{gone}})
			time.Sleep(20 * time.Millisecond)
		}
		if got := lk.of(gone) > 0; got != recheck {
			t.Errorf("Recheck=%v: gone path looked up = %v (all: %q)", recheck, got, lk.got)
		}
	}
}
