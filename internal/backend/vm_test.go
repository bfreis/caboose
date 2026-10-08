package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
// same files: one whose directory is a writable one's, is inside one, or
// holds one is refused, in either order.
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
		{"ro holds rw", []Mount{ro(home), rw(filepath.Join(home, ".claude"))}},
		{"rw is /", []Mount{rw("/"), ro(filepath.Join(home, ".claude"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := planShares(tc.ms); err == nil || !strings.Contains(err.Error(), "overlaps the writable mount") {
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
