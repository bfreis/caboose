package launcher

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/diskusage"
	"golang.org/x/sys/unix"
)

// smallUnshared reports a 32nd of each file's size as unshared: half of
// what stableWalker allocates. Each call moves its clock a second.
type smallUnshared struct{ now *time.Time }

func (p smallUnshared) UnsharedSize(dirfd int, name string) (int64, error) {
	*p.now = p.now.Add(time.Second)
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	return st.Size / 32, nil
}

// stableWalker is diskusage.Walker with allocated sizes of the test's own
// (a sixteenth of what the files hold, directories nothing): what a
// filesystem allocates for a directory differs from one to the next.
type stableWalker struct{ *diskusage.Walker }

func (s stableWalker) Walk(u *diskusage.Usage, p string, skip func(string) bool) {
	s.Walker.Walk(u, p, skip)
	u.Allocated = u.Apparent / 16
}

// stableDisk makes caboose disk's walks the test's: allocated as
// stableWalker, unshared as smallUnshared (or not measured, when method is
// ""), on smallUnshared's clock.
func stableDisk(t *testing.T, method string, budget time.Duration) {
	t.Helper()
	prev := newDiskWalker
	newDiskWalker = func(time.Duration) (diskWalker, string) {
		now := time.Unix(0, 0)
		w := &diskusage.Walker{Budget: budget, Now: func() time.Time { return now }}
		if method != "" {
			w.Unshared = smallUnshared{&now}
		}
		return stableWalker{w}, method
	}
	t.Cleanup(func() { newDiskWalker = prev })
}

func writeSized(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// diskEnv is lsConfig's environment with a file of every kind caboose
// disk tells apart, another environment with a builder cache, and
// caboose's own files.
func diskEnv(t *testing.T) *setupEnv {
	t.Helper()
	e, _ := adminEnv(t, lsConfig, nil)
	c := e.a.Cfg
	vmr := e.a.vmRoot()
	writeSized(t, filepath.Join(vmr, "images", "root-aaaaaaaaaaaaaaaa.img"), 64<<30)
	writeSized(t, filepath.Join(vmr, "images", "root-bbbbbbbbbbbbbbbb.img"), 64<<30)
	writeSized(t, filepath.Join(vmr, "images", "empty-ext4-4g.img"), 4<<30)
	writeSized(t, filepath.Join(vmr, c.Container, "scratch.img"), 64<<30)
	writeSized(t, filepath.Join(vmr, c.Container, "console.log"), 2000)
	writeSized(t, filepath.Join(vmr, builderName, "cache.img"), 128<<30)
	e.makeDisk("docker", 64<<30)
	e.makeDisk("cache", 96<<30)
	e.makeDisk("old", 32<<30)
	if err := e.a.vmImages().put(vmImage{Name: c.Image, ID: "sha256:aaaaaaaaaaaaaaaaff", Disk: "root-aaaaaaaaaaaaaaaa.img"}); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(c.DataDir, "home", ".claude", "memory.md"), 16000)
	// A checkpoint hard-linked to the live file: counted once, where it
	// is met first.
	ck := filepath.Join(c.EnvDir, "checkpoints", "claude", "20261031T120000Z")
	writeSized(t, filepath.Join(ck, "other.md"), 32000)
	if err := os.Link(filepath.Join(c.DataDir, "home", ".claude", "memory.md"), filepath.Join(ck, "memory.md")); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(c.DataDir, "local", "linux-arm64", "share", "claude", "versions", "2.1.0"), 160000)
	writeSized(t, filepath.Join(c.DataDir, "link.log"), 8000)
	work := filepath.Join(c.CabooseHome, "envs", "work")
	writeSized(t, filepath.Join(work, "data", "vm", builderName, "cache.img"), 128<<30)
	writeSized(t, filepath.Join(work, "config.toml"), 160)
	writeSized(t, filepath.Join(c.CabooseHome, "versions", "v1.0.0", "caboose"), 48000)
	writeSized(t, filepath.Join(c.CabooseHome, "cache", "apk", "pkg.apk"), 64000)
	writeSized(t, filepath.Join(c.CabooseHome, "daemon", "daemon.log"), 1600)
	// In envs/, but no environment's.
	writeSized(t, filepath.Join(c.CabooseHome, "envs", "Not An Env", "x"), 800)
	return e
}

// caboose disk shows every environment's kinds and caboose's own, with
// apparent, allocated and unshared sizes, the totals, and what frees each
// kind present.
func TestDiskGolden(t *testing.T) {
	e := diskEnv(t)
	stableDisk(t, "APFS", 0)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Disk("", nil); err != nil {
		t.Fatal(err)
	}
	golden(t, &boxApp{tmp: filepath.Dir(e.a.Cfg.CabooseHome)}, "disk.out", out.String())
}

// Off macOS nothing reports unshared sizes: the column is '-' and the
// report says why; an environment chosen (-e, CABOOSE_ENV) shows that
// one alone, with caboose's own files, which leave out every
// environment's still.
func TestDiskUnmeasuredOneEnv(t *testing.T) {
	e := diskEnv(t)
	stableDisk(t, "", 0)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Disk("work", nil); err != nil {
		t.Fatal(err)
	}
	golden(t, &boxApp{tmp: filepath.Dir(e.a.Cfg.CabooseHome)}, "disk-unmeasured.out", out.String())
}

// Past the budget the rest is counted but not measured: '≥', and the
// partial line with how many files were. The checkpoints, the slowest to
// measure, come last, so the budget goes to the rest first.
func TestDiskPartial(t *testing.T) {
	e := diskEnv(t)
	// 21 files, the checkpoints' one among them: a second each.
	stableDisk(t, "APFS", 20*time.Second)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Disk("", []string{"--budget", "20s"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"partial: unshared sizes measured for 20 of 21 files in 20s",
		"\n    checkpoints        1     32 kB     2.0 kB         -\n",
		"\n    root disks         2    137 GB     8.6 GB    4.3 GB\n",
		"\n    total             15    554 GB      35 GB    ≥17 GB\n",
		", ≥",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}

// --json is the same report: each environment's kinds, the paths walked,
// and the totals adding up.
func TestDiskJSON(t *testing.T) {
	e := diskEnv(t)
	stableDisk(t, "APFS", 0)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Disk("", []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var r diskReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(r.Envs) != 2 || r.Envs[0].Env != "default" || r.Envs[1].Env != "work" || r.Caboose == nil || r.Unshared != "APFS" {
		t.Fatalf("%+v", r)
	}
	kinds := map[string]diskKind{}
	var sum diskusage.Usage
	for _, k := range r.Envs[0].Kinds {
		kinds[k.Kind] = k
		sum.Add(k.Usage)
	}
	if sum != r.Envs[0].Total {
		t.Errorf("kinds add up to %+v, total %+v", sum, r.Envs[0].Total)
	}
	if k := kinds["root disks"]; k.Files != 2 || k.Apparent != 128<<30 {
		t.Errorf("root disks %+v", k)
	}
	if k := kinds["volumes"]; k.Files != 3 || k.Apparent != (64+96+32)<<30 {
		t.Errorf("volumes %+v", k)
	}
	// The checkpoint's link to the kept home's file counts in the home.
	if k := kinds["checkpoints"]; k.Files != 1 || k.Apparent != 32000 {
		t.Errorf("checkpoints %+v", k)
	}
	if k := kinds["kept home"]; k.Files != 1 || k.Apparent != 16000 {
		t.Errorf("kept home %+v", k)
	}
	// other is what no other kind walks: config.toml, the record, the
	// logs.
	if k := kinds["other"]; k.Apparent > 1<<20 || k.Files < 3 {
		t.Errorf("other %+v", k)
	}
	var total diskusage.Usage
	for _, s := range r.Envs {
		total.Add(s.Total)
	}
	total.Add(r.Caboose.Total)
	if total != r.Total {
		t.Errorf("scopes add up to %+v, total %+v", total, r.Total)
	}
	// caboose's own leaves out every environment.
	for _, k := range r.Caboose.Kinds {
		if k.Apparent > 1<<20 {
			t.Errorf("caboose's own %s counts an environment's disk: %+v", k.Kind, k)
		}
	}
}

// A bad command line is a usage error, exit 2; an environment that is not
// there says how to list them.
func TestDiskArgs(t *testing.T) {
	e := diskEnv(t)
	for _, args := range [][]string{{"--nope"}, {"--env", "work"}, {"--budget"}, {"--budget", "soon"}, {"--budget", "-1s"}} {
		err := e.a.Disk("", args)
		if ee, ok := err.(*ExitError); !ok || ee.Code != 2 {
			t.Errorf("%q: %v", args, err)
		}
	}
	if err := e.a.Disk("gone", nil); err == nil || !strings.Contains(err.Error(), "caboose env list") {
		t.Errorf("missing env: %v", err)
	}
}

// unreadableWalker is stableWalker, but every path in unreadable is one
// it could not read, as a walk that cannot open a path's parent counts it.
type unreadableWalker struct {
	stableWalker
	unreadable []string
}

func (w unreadableWalker) Walk(u *diskusage.Usage, p string, skip func(string) bool) {
	if slices.Contains(w.unreadable, p) {
		u.Unreadable++
		return
	}
	w.stableWalker.Walk(u, p, skip)
}

// A kind that found nothing but what it could not read stays in the
// report, so that the line saying how much could not be read is there.
func TestDiskUnreadableKind(t *testing.T) {
	e := diskEnv(t)
	app := appbundle.InstallPath(e.a.Cfg.Home)
	prev := newDiskWalker
	newDiskWalker = func(time.Duration) (diskWalker, string) {
		return unreadableWalker{stableWalker{&diskusage.Walker{}}, []string{app}}, ""
	}
	t.Cleanup(func() { newDiskWalker = prev })
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Disk("", nil); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"\n    Caboose.app ", "\n1 entry could not be read, and is not counted\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}

// A file of the kept home with a hard link outside everything caboose
// disk walks is counted, with no unshared size: its kind shows none, and
// the report says why.
func TestDiskLinkedOutside(t *testing.T) {
	e := diskEnv(t)
	stableDisk(t, "APFS", 0)
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.Link(filepath.Join(e.a.Cfg.DataDir, "home", ".claude", "memory.md"), outside); err != nil {
		t.Fatal(err)
	}
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Disk("", nil); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"\n    kept home          1     16 kB     1.0 kB         -\n",
		"\n1 file has a hard link outside what was looked at: counted, but with no unshared size, as deleting what was looked at would not free it\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}

// With no environment chosen, one that does not exist (this one, named
// by CABOOSE_ENV or the default) is not listed; chosen, it is an error
// naming the commands that list and make one.
func TestDiskEnvChoice(t *testing.T) {
	e := diskEnv(t)
	stableDisk(t, "", 0)
	c := e.a.Cfg
	c.Env = "typo"
	c.EnvDir = filepath.Join(c.CabooseHome, "envs", "typo")
	c.DataDir = filepath.Join(c.EnvDir, "data")
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Disk("", nil); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Contains(s, "typo") || !strings.Contains(s, "environment default") || !strings.Contains(s, "environment work") {
		t.Errorf("%s", s)
	}
	err := e.a.Disk("typo", nil)
	if err == nil || !strings.Contains(err.Error(), "no environment typo: 'caboose env list' lists them, 'caboose -e typo setup' creates it") {
		t.Errorf("chosen, missing: %v", err)
	}
}
