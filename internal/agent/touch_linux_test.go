package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// events reads what an inotify fd has queued, as "name:mask" in hex, "."
// for the watched directory itself.
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
		out = append(out, name+":"+fmt.Sprintf("%x", ev.Mask))
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
		_, err := unix.InotifyAddWatch(fd, d, unix.IN_ATTRIB|unix.IN_MODIFY)
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
		Touch(work, c.paths)
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
