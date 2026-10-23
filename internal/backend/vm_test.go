package backend

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/vm"
)

// A directory is a share of its own; a file is mounted from its
// directory's, shared once; a missing source is made a directory. A
// read-only mount is a read-only share of its own, bound read-only.
func TestPlanShares(t *testing.T) {
	d := t.TempDir()
	home := filepath.Join(d, "home")
	for _, p := range []string{filepath.Join(d, "repo"), filepath.Join(home, ".claude"), filepath.Join(d, "managed")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".other.json"), filepath.Join(d, "managed", "CLAUDE.md")} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shares, mounts, err := planShares([]Mount{
		{Source: filepath.Join(d, "repo") + "/", Target: "/work"},
		{Source: filepath.Join(home, ".claude.json"), Target: "/home/agent/.claude.json"},
		{Source: filepath.Join(home, ".other.json"), Target: "/home/agent/.other.json"},
		{Source: filepath.Join(home, ".claude"), Target: "/home/agent/.claude"},
		{Source: filepath.Join(d, "new"), Target: "/new"},
		{Source: filepath.Join(d, "managed"), Target: "/ro", ReadOnly: true},
		{Source: filepath.Join(d, "managed", "CLAUDE.md"), Target: "/ro.md", ReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantShares := []vm.Share{
		{Name: "m0", Path: filepath.Join(d, "repo")},
		{Name: "m1", Path: home},
		{Name: "m2", Path: filepath.Join(home, ".claude")},
		{Name: "m3", Path: filepath.Join(d, "new")},
		{Name: "m4", Path: filepath.Join(d, "managed"), ReadOnly: true},
	}
	if !reflect.DeepEqual(shares, wantShares) {
		t.Errorf("shares %+v", shares)
	}
	tag := vm.ShareTag
	wantMounts := []agentproto.GuestMount{
		{Tag: tag, Path: "m0", Target: "/work"},
		{Tag: tag, Path: "m1/.claude.json", Target: "/home/agent/.claude.json"},
		{Tag: tag, Path: "m1/.other.json", Target: "/home/agent/.other.json"},
		{Tag: tag, Path: "m2", Target: "/home/agent/.claude"},
		{Tag: tag, Path: "m3", Target: "/new"},
		{Tag: tag, Path: "m4", Target: "/ro", ReadOnly: true},
		{Tag: tag, Path: "m4/CLAUDE.md", Target: "/ro.md", ReadOnly: true},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Errorf("mounts %+v", mounts)
	}
	if fi, err := os.Stat(filepath.Join(d, "new")); err != nil || !fi.IsDir() {
		t.Errorf("missing source not made a directory: %v", err)
	}
	if _, _, err := planShares([]Mount{{Source: "rel", Target: "/x"}}); err == nil {
		t.Error("a relative source was taken")
	}
}

// A read-only share is read-only only while no writable share reaches the
// same files: one whose directory is a writable one's, or is inside one,
// is refused, in either order.
func TestPlanSharesRefusesOverlap(t *testing.T) {
	d := t.TempDir()
	home := filepath.Join(d, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rw := func(src string) Mount { return Mount{Source: src, Target: "/rw"} }
	ro := func(src string) Mount { return Mount{Source: src, Target: "/ro", ReadOnly: true} }
	for _, tc := range []struct {
		name string
		ms   []Mount
	}{
		{"same dir", []Mount{rw(filepath.Join(home, ".claude")), ro(filepath.Join(home, ".claude"))}},
		{"same dir, ro first", []Mount{ro(filepath.Join(home, ".claude")), rw(filepath.Join(home, ".claude"))}},
		{"a file's dir", []Mount{rw(filepath.Join(home, ".claude.json")), ro(filepath.Join(home, ".claude.json"))}},
		{"ro inside rw", []Mount{rw(home), ro(filepath.Join(home, ".claude"))}},
		{"rw is /", []Mount{rw("/"), ro(filepath.Join(home, ".claude"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := planShares(tc.ms); err == nil || !strings.Contains(err.Error(), "the guest could write it through that one") {
				t.Errorf("err %v", err)
			}
		})
	}
	// A sibling whose name only starts with the other's is no overlap.
	if err := os.MkdirAll(filepath.Join(d, "home2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := planShares([]Mount{rw(home), ro(filepath.Join(d, "home2"))}); err != nil {
		t.Errorf("disjoint dirs refused: %v", err)
	}
}

// A read-only root with writable parts inside it: the parent a read-only
// share, each part a writable one, bound parent first, each at its own
// path under the parent's target. A part's share holds only the part.
func TestPlanSharesReadOnlyParentWritableChild(t *testing.T) {
	d := t.TempDir()
	root := filepath.Join(d, "dev")
	for _, p := range []string{"a/proj", "b"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shares, mounts, err := planShares([]Mount{
		{Source: root, Target: "/work/dev", ReadOnly: true},
		{Source: filepath.Join(root, "a/proj"), Target: "/work/dev/a/proj"},
		{Source: filepath.Join(root, "b"), Target: "/work/dev/b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantShares := []vm.Share{
		{Name: "m0", Path: root, ReadOnly: true},
		{Name: "m1", Path: filepath.Join(root, "a/proj")},
		{Name: "m2", Path: filepath.Join(root, "b")},
	}
	if !reflect.DeepEqual(shares, wantShares) {
		t.Errorf("shares %+v", shares)
	}
	tag := vm.ShareTag
	wantMounts := []agentproto.GuestMount{
		{Tag: tag, Path: "m0", Target: "/work/dev", ReadOnly: true},
		{Tag: tag, Path: "m1", Target: "/work/dev/a/proj"},
		{Tag: tag, Path: "m2", Target: "/work/dev/b"},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Errorf("mounts %+v", mounts)
	}
	// Still refused the other way round: a read-only part of a writable
	// root could be written through the root's share.
	if _, _, err := planShares([]Mount{
		{Source: root, Target: "/work/dev"},
		{Source: filepath.Join(root, "b"), Target: "/work/dev/b", ReadOnly: true},
	}); err == nil {
		t.Error("a read-only share inside a writable one was taken")
	}
}

// The guest binds the mounts in the order given, so one listed before a
// mount whose target holds its own would be covered by it: refused,
// whichever is read-only.
func TestPlanSharesRefusesChildBeforeParent(t *testing.T) {
	d := t.TempDir()
	root := filepath.Join(d, "dev")
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ms := range [][]Mount{
		{{Source: filepath.Join(root, "proj"), Target: "/work/dev/proj"}, {Source: root, Target: "/work/dev", ReadOnly: true}},
		{{Source: filepath.Join(d, "x"), Target: "/a/b/"}, {Source: filepath.Join(d, "y"), Target: "/a"}},
	} {
		if _, _, err := planShares(ms); err == nil || !strings.Contains(err.Error(), "would cover it") {
			t.Errorf("%v: err %v", ms, err)
		}
	}
	// Siblings, and a name that only starts with another's, are no nesting.
	if _, _, err := planShares([]Mount{
		{Source: filepath.Join(d, "p"), Target: "/work/dev2"},
		{Source: root, Target: "/work/dev", ReadOnly: true},
	}); err != nil {
		t.Errorf("siblings refused: %v", err)
	}
}

// caboose's -e goes over the image's ENV, in place.
func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"PATH=/bin", "HOME=/root", "LANG=C"}, []string{"HOME=/home/agent", "TZ=UTC"})
	want := []string{"PATH=/bin", "HOME=/home/agent", "LANG=C", "TZ=UTC"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestLastLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 5, "a\nb\nc\n"},
		{"a\nb\nc\n", 3, "a\nb\nc\n"},
		{"", 3, ""},
		{"a\n", 0, ""},
	} {
		if got := lastLines(c.in, c.n); got != c.want {
			t.Errorf("lastLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// stubHost is a VMHost with one image, which never starts a VM.
type stubHost struct{ img VMImage }

func (h stubHost) Image(string) (VMImage, error) { return h.img, nil }
func (h stubHost) Machine() (vm.Machine, error)  { return vm.Machine{CPUs: 1}, nil }
func (h stubHost) NewScratch(string) error       { return nil }
func (h stubHost) Volume(string) (string, error) { return "", nil }
func (h stubHost) StartVMM(vm.Dir) error         { return os.ErrPermission }

// What a VM cannot be is refused before anything is written.
func TestCreateRefuses(t *testing.T) {
	img := VMImage{ID: "i", Disk: "/d", User: "agent", Entrypoint: []string{"/e"}}
	for _, c := range []struct {
		spec Spec
		say  string
	}{
		{Spec{Runtime: "runsc"}, "runtime"},
		{Spec{RunArgs: []string{"--cpus=2"}}, "docker run arguments"},
		{Spec{Groups: []string{"20"}}, "groups"},
		{Spec{}, "UID or UID:GID"},
	} {
		v := NewVM("box", vm.Dir(filepath.Join(t.TempDir(), "box")), stubHost{img})
		v.Stderr = nil
		err := v.Create(c.spec)
		if err == nil || !strings.Contains(err.Error(), c.say) {
			t.Errorf("%+v: err = %v, want it to say %q", c.spec, err, c.say)
		}
		if v.State() != "absent" {
			t.Errorf("%+v: left a record", c.spec)
		}
	}
}

// Started is vmm.pid's mtime while vmm serves the socket, and false with
// no listener or no vmm.pid.
func TestVMStarted(t *testing.T) {
	d, err := os.MkdirTemp("", "cbvm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	v := NewVM("t", vm.Dir(d), nil)
	if _, ok := v.Started(); ok {
		t.Fatal("Started with no socket")
	}
	l, err := net.Listen("unix", vm.Dir(d).Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, ok := v.Started(); ok {
		t.Fatal("Started with no vmm.pid")
	}
	if err := os.WriteFile(vm.Dir(d).PID(), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(vm.Dir(d).PID(), at, at); err != nil {
		t.Fatal(err)
	}
	got, ok := v.Started()
	if !ok || !got.Equal(at) {
		t.Fatalf("Started = %v, %v; want %v", got, ok, at)
	}
	l.Close()
	if _, ok := v.Started(); ok {
		t.Fatal("Started once the socket is closed")
	}
}

// The console is the guest's to fill: Logs reads its last consoleTail
// bytes alone, from the first whole line in them, and makes what it
// writes printable.
func TestVMLogsTailPrintable(t *testing.T) {
	dir := t.TempDir()
	v := &VM{Dir: vm.Dir(dir)}
	f, err := os.Create(v.Dir.Console())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("first line, far back\n" + strings.Repeat("x", consoleTail) + "\nlast \x1b]52;c;evil\x07line\n"); err != nil {
		t.Fatal(err)
	}
	// Sparse past the end of what is read: never read.
	if err := f.Truncate(1 << 40); err != nil {
		t.Skip(err)
	}
	if _, err := f.WriteAt([]byte("\nend \x1b[2Jline\n"), 1<<40); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var out bytes.Buffer
	if err := v.Logs(&out, nil, 1); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != `end \u001B[2Jline`+"\n" {
		t.Errorf("logs %q", got)
	}
	// A small console is read whole. Its lines end in \r\n, as a tty's
	// do: one \r is dropped from each, and a tab kept, never escaped; any
	// other \r is.
	if err := os.WriteFile(v.Dir.Console(), []byte("a\tb\r\nb\x07\r\r\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := v.Logs(&out, nil, 5); err != nil || out.String() != "a\tb\nb\\u0007\\u000D\nc\n" {
		t.Errorf("small: %q %v", out.String(), err)
	}
}
