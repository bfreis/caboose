package agent

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// One touch, as a watcher sees it: once from a watch on the directory,
// twice from a watch on the file and one on its directory (inotify
// reports the event to each watch). A probe that watches both counts
// every touch twice.
func TestTouchOneTouchSeenByWatches(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(work, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		watches []string
		want    int
	}{
		{"directory only", []string{work}, 1},
		{"file and directory", []string{file, work}, 2},
	} {
		fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.watches {
			if _, err := unix.InotifyAddWatch(fd, w, unix.IN_ATTRIB|unix.IN_MODIFY); err != nil {
				t.Fatal(err)
			}
		}
		Touch([]string{work}, []string{file})
		got := events(t, fd)
		unix.Close(fd)
		t.Logf("%s: %v", c.name, got)
		if len(got) != c.want {
			t.Errorf("%s: %d events %v, want %d", c.name, len(got), got, c.want)
		}
	}
}

// A batch naming a directory and something gone from it touches the
// directory twice; the second event is the same as the first, still
// unread, so inotify merges them.
func TestTouchDirectoryTouchedTwiceMerges(t *testing.T) {
	work := t.TempDir()
	d := filepath.Join(work, "d")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, work, unix.IN_ATTRIB|unix.IN_MODIFY); err != nil {
		t.Fatal(err)
	}
	Touch([]string{work}, []string{d, filepath.Join(d, "gone")})
	got := events(t, fd)
	t.Logf("events: %v", got)
	if len(got) != 1 {
		t.Errorf("%d events %v, want 1 (merged)", len(got), got)
	}
}
