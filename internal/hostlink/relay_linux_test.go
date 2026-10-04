package hostlink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// End to end, with the real watcher, a real agent, and a real inotify
// watch on the "container's" side: an edit made in the host's directory
// reaches a watcher on the container's copy of it as IN_ATTRIB, and leaves
// that copy's contents and mtime as they were.
func TestRelayEndToEnd(t *testing.T) {
	host := t.TempDir()
	container := t.TempDir()
	for _, d := range []string{host, container} {
		if err := os.WriteFile(filepath.Join(d, "x.go"), []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(filepath.Join(container, "x.go"), past, past); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, container, unix.IN_ATTRIB); err != nil {
		t.Fatal(err)
	}

	watching := make(chan struct{})
	linked(t, Config{Ports: PortSet{}, Actions: &fakeActions{},
		Relay: []Root{{Host: host, Container: container}},
		Watch: func(dirs []string) (Watcher, error) {
			defer close(watching)
			return WatchRoots(dirs)
		}})
	<-watching
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(host, "x.go"), []byte("package x // edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := unix.Read(fd, buf)
		if err != nil || n < unix.SizeofInotifyEvent {
			got <- ""
			return
		}
		got <- strings.TrimRight(string(buf[unix.SizeofInotifyEvent:n]), "\x00")
	}()
	select {
	case name := <-got:
		if name != "x.go" {
			t.Fatalf("IN_ATTRIB for %q, want x.go", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the container's side")
	}
	fi, err := os.Stat(filepath.Join(container, "x.go"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(container, "x.go"))
	if !fi.ModTime().Equal(past) || string(b) != "package x" || fi.Mode().Perm() != 0o644 {
		t.Fatalf("the touch changed the file: mtime %v, mode %v, contents %q", fi.ModTime(), fi.Mode(), b)
	}
}
