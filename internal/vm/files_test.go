package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// The initramfs is a newc cpio a kernel can read: /dev, /dev/console, the
// agent as /init, and the trailer, each entry 4-aligned.
func TestWriteInitramfs(t *testing.T) {
	var b bytes.Buffer
	agent := []byte("\x7fELF agent")
	if err := WriteInitramfs(&b, agent); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	type entry struct {
		name string
		mode uint64
		body []byte
	}
	var got []entry
	for len(data) > 0 {
		if len(data) < 110 || string(data[:6]) != "070701" {
			t.Fatalf("bad header at %q", data[:min(len(data), 16)])
		}
		field := func(i int) uint64 {
			v, err := strconv.ParseUint(string(data[6+8*i:14+8*i]), 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		mode, size, namesize := field(1), int(field(6)), int(field(11))
		name := string(data[110 : 110+namesize-1])
		off := (110 + namesize + 3) &^ 3
		body := data[off : off+size]
		got = append(got, entry{name, mode, body})
		data = data[(off+size+3)&^3:]
		if name == "TRAILER!!!" {
			break
		}
	}
	if len(data) != 0 {
		t.Fatalf("%d bytes after the trailer", len(data))
	}
	want := []string{"dev", "dev/console", "init", "TRAILER!!!"}
	if len(got) != len(want) {
		t.Fatalf("entries %+v", got)
	}
	for i, e := range got {
		if e.name != want[i] {
			t.Fatalf("entry %d is %q", i, e.name)
		}
	}
	if got[2].mode != 0o100755 || !bytes.Equal(got[2].body, agent) {
		t.Fatalf("init: mode %o, %q", got[2].mode, got[2].body)
	}
}

// A clone has src's bytes and size, and where src had a hole, so does it.
func TestCloneFile(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "empty.img"), filepath.Join(d, "scratch.img")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("superblock"), 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tail"), 64<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := CloneFile(src, dst); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("the clone differs")
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dst, &st); err != nil {
		t.Fatal(err)
	}
	if used := st.Blocks * 512; used > 8<<20 {
		t.Errorf("the clone uses %d bytes of a mostly empty 64 MiB", used)
	}
	if err := CloneFile(src, dst); err == nil {
		t.Error("cloned over an existing file")
	}
}
