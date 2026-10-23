package launcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
)

// ckDoc is a checkpoint environment with some checkpoints taken, and the
// seams doctor and status run with: one APFS volume on a Mac, plenty of
// space, and a clock that is now (move it with at).
type ckDoc struct {
	*ckEnv
	seams ckSeams
	now   time.Time
}

func newCkDoc(t *testing.T, paths ...string) *ckDoc {
	t.Helper()
	d := &ckDoc{ckEnv: newCkEnv(t, paths...), now: time.Now()}
	d.seams = ckSeams{
		fs: ckFS{goos: "darwin",
			info:   func(string) (uint64, string, error) { return 1, "apfs", nil },
			statfs: func(string) (uint64, uint64, error) { return 500 << 30, 1000 << 30, nil }},
		now:  func() time.Time { return d.now },
		apps: noOtherEnvs(t),
	}
	return d
}

// take takes a checkpoint of every configured path.
func (d *ckDoc) take(t *testing.T) {
	t.Helper()
	var out bytes.Buffer
	d.app.Stdout, d.app.Stderr = &out, &out
	d.app.DaemonDial = func(context.Context, bool) (DaemonCaller, error) {
		return nil, daemonproto.NewError(daemonproto.KindUnavailable, "no daemon answers")
	}
	if err := d.run(t, checkpointOpts{}, "create"); err != nil {
		t.Fatalf("create: %v\n%s", err, &out)
	}
	d.now = time.Now()
}

func (d *ckDoc) at(delta time.Duration) { d.now = time.Now().Add(delta) }

func (d *ckDoc) doctor(running bool, sched *daemonproto.CheckpointStatus) *checkup {
	c := &checkup{}
	d.app.doctorCheckpointsWith(c, running, sched, d.seams)
	return c
}

// row is the first finding at level l whose text has sub, or nil.
func (c *checkup) row(l level, sub string) *finding {
	for i := range c.rows {
		if c.rows[i].level == l && strings.Contains(c.rows[i].text, sub) {
			return &c.rows[i]
		}
	}
	return nil
}

func wantRow(t *testing.T, c *checkup, l level, sub, fix string) {
	t.Helper()
	r := c.row(l, sub)
	if r == nil {
		t.Fatalf("no level-%d row with %q in:\n%s", l, sub, c)
	}
	if l == levelProblem && (r.fix == "" || !strings.Contains(r.fix, fix)) {
		t.Errorf("row %q has fix %q, want one with %q", r.text, r.fix, fix)
	}
}

func noRow(t *testing.T, c *checkup, l level, sub string) {
	t.Helper()
	if r := c.row(l, sub); r != nil {
		t.Errorf("unexpected row %q in:\n%s", r.text, c)
	}
}

func TestDoctorCheckpointsNothingConfigured(t *testing.T) {
	d := newCkDoc(t)
	if c := d.doctor(true, nil); len(c.rows) != 0 {
		t.Errorf("rows without paths or a store:\n%s", c)
	}
}

func TestDoctorCheckpointsResolution(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj", "/work/dev/other", "~/.claude")
	c := d.doctor(false, nil)
	wantRow(t, c, levelOK, "/work/dev/proj: a writable part of a root, copied from "+d.dev+"/proj", "")
	wantRow(t, c, levelOK, "~/.claude: a kept path of the home, copied from "+filepath.Join(d.dataDir, "home", ".claude"), "")
	wantRow(t, c, levelProblem, "/work/dev/other: not copied: ", "caboose setup checkpoints")
	if c.count(levelProblem) != 1 {
		t.Errorf("\n%s", c)
	}
	// Only reading: the store was not made.
	wantRow(t, c, levelNote, "is not made yet", "")
	if _, err := os.Lstat(checkpoint.StoreDir(d.envDir)); err == nil {
		t.Error("doctor made the store")
	}
}

func TestDoctorCheckpointsMethod(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	c := d.doctor(false, nil)
	wantRow(t, c, levelOK, "clonefile", "")
	noRow(t, c, levelNote, "warning")

	// Another volume on a Mac: a warning, which is a note that says so.
	d.seams.fs.info = func(p string) (uint64, string, error) {
		if strings.HasPrefix(p, d.dev) {
			return 2, "apfs", nil
		}
		return 1, "apfs", nil
	}
	c = d.doctor(false, nil)
	wantRow(t, c, levelNote, "warning: /work/dev/proj: not on the store's APFS volume: copies at first, unchanged files linked after; slower", "")

	// On Linux the same walker is a plain note, and says what decides it.
	d.seams.fs.goos = "linux"
	d.seams.fs.info = func(string) (uint64, string, error) { return 1, "ext4", nil }
	c = d.doctor(false, nil)
	wantRow(t, c, levelNote, "reflink if the filesystem supports it, else copy", "")
	noRow(t, c, levelNote, "warning")
	d.seams.fs.info = func(string) (uint64, string, error) { return 1, "btrfs", nil }
	wantRow(t, d.doctor(false, nil), levelOK, "(per-file clones on btrfs)", "")
}

func TestDoctorCheckpointsStore(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	ckWriteFile(t, filepath.Join(d.dev, "proj", "a.txt"), "a")
	d.take(t)
	store := checkpoint.StoreDir(d.envDir)
	c := d.doctor(false, nil)
	wantRow(t, c, levelOK, "mode 0700", "")
	wantRow(t, c, levelOK, "no root or mount of any environment reaches the store", "")
	wantRow(t, c, levelOK, "newest checkpoint", "")
	if n := c.count(levelProblem); n != 0 {
		t.Errorf("%d problems:\n%s", n, c)
	}
	if err := os.Chmod(store, 0o755); err != nil {
		t.Fatal(err)
	}
	wantRow(t, d.doctor(false, nil), levelProblem, "has mode 0755, not 0700", "chmod 700 "+store)
	if err := os.Chmod(store, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCheckpointsExposed(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	d.take(t)
	// A root over CABOOSE_HOME.
	d.app.Cfg.Roots = append(d.app.Cfg.Roots, config.Root{Name: "all", Host: d.tmp, Container: "/work/all"})
	c := d.doctor(false, nil)
	wantRow(t, c, levelProblem, "is inside root /work/all of environment default", "narrow the root")
	wantRow(t, c, levelProblem, "", "move CABOOSE_HOME")
	d.app.Cfg.Roots = d.app.Cfg.Roots[:2]
	noRow(t, d.doctor(false, nil), levelProblem, "inside")

	// A mount of the running sandbox.
	d.box.Status = "running"
	d.box.SandboxMounts = []backend.Mount{{Source: d.home, Target: "/work/h"}}
	wantRow(t, d.doctor(true, nil), levelProblem, "mount /work/h of environment default's sandbox", "narrow the root")
	d.box.SandboxMounts = nil

	// A sandbox that cannot say what it mounts: not checked, not passed.
	d.app.Backend = errMounts{d.box}
	c = d.doctor(true, nil)
	wantRow(t, c, levelUnchecked, "whether a sandbox can reach the store", "")
	noRow(t, c, levelOK, "no root or mount")
	d.app.Backend = d.box

	// The daemon says it refuses.
	c = d.doctor(true, &daemonproto.CheckpointStatus{Exposed: "the store is inside root R"})
	wantRow(t, c, levelProblem, "no checkpoint is taken: the store is inside root R", "narrow the root")
}

func TestDoctorCheckpointsStale(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	d.take(t)
	d.at(20 * time.Minute)
	noRow(t, d.doctor(true, nil), levelProblem, "newest checkpoint")
	d.at(30 * time.Minute)
	sched := &daemonproto.CheckpointStatus{LastError: "disk on fire", Paths: []daemonproto.CheckpointPathStatus{{Path: "/work/dev/proj"}}}
	c := d.doctor(true, sched)
	wantRow(t, c, levelProblem, "the newest checkpoint is 30 min ago", "caboose checkpoint create /work/dev/proj")
	wantRow(t, c, levelProblem, "last error: disk on fire", "")
	// While the sandbox is stopped nothing is due.
	noRow(t, d.doctor(false, nil), levelProblem, "newest checkpoint")
	// A take that failed is a problem on its own.
	sched.Paths[0].Failing = "no space"
	wantRow(t, d.doctor(false, sched), levelProblem, "the last checkpoint failed: no space", "caboose checkpoint create")
}

func TestDoctorCheckpointsSpace(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	d.take(t)
	free := func(avail uint64) {
		d.seams.fs.statfs = func(string) (uint64, uint64, error) { return avail, 1000 << 30, nil }
	}
	free(500 << 30)
	wantRow(t, d.doctor(false, nil), levelOK, "free where the store is", "")
	free(30 << 30) // below max(10 GiB, 5%) = 50 GiB
	c := d.doctor(false, nil)
	wantRow(t, c, levelProblem, "below the soft floor of 54 GB: the daemon skips its checkpoints until there is more ('caboose checkpoint create' still takes one, down to 11 GB), and prunes none early", "move CABOOSE_HOME")
	free(5 << 30) // below max(1 GiB, 1%) = 10 GiB
	wantRow(t, d.doctor(false, nil), levelProblem, "below the hard floor of", "free space")
	d.seams.fs.statfs = func(string) (uint64, uint64, error) { return 0, 0, os.ErrPermission }
	wantRow(t, d.doctor(false, nil), levelUnchecked, "free space", "")
}

// A sharp shrink stands until the checkpoints are as large again, and
// doctor names the earlier checkpoint to restore from.
func TestDoctorCheckpointsShrink(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	for i := 0; i < 1300; i++ {
		ckWriteFile(t, filepath.Join(d.dev, "proj", fmt.Sprintf("d%d", i%10), fmt.Sprintf("f%d", i)), "x")
	}
	d.take(t)
	s, err := checkpoint.Open(d.home, d.envDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	es, err := s.List(checkpoint.Key("/work/dev/proj"))
	if err != nil || len(es) != 1 {
		t.Fatalf("%v %v", es, err)
	}
	first := es[0].ID.String()
	// Removed through saferm, rooted at the test's own project dir.
	for i := 1; i < 10; i++ {
		if err := saferm.RemoveAll(filepath.Join(d.dev, "proj"), filepath.Join(d.dev, "proj", fmt.Sprintf("d%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	d.take(t)
	c := d.doctor(false, nil)
	wantRow(t, c, levelProblem, "holds far less than the one before it ("+first+")", "caboose checkpoint restore "+first+" --to DIR")

	// The earlier one is pinned: doctor, status and the listing say so,
	// until it is released.
	es, err = s.List(checkpoint.Key("/work/dev/proj"))
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %v", es, err)
	}
	second := es[0].ID.String()
	wantRow(t, c, levelNote, "/work/dev/proj: "+first+" is pinned, since "+second+" held half or less of it: it is kept until a later checkpoint holds as much again, or until caboose checkpoint release "+first, "")
	if got := d.app.checkpointLine(linkView{}, false, d.seams); !strings.HasSuffix(got, "; 1 pinned after a sharp shrink (caboose checkpoint list)") {
		t.Errorf("status: %q", got)
	}
	var out bytes.Buffer
	d.app.Stdout = &out
	if err := d.run(t, checkpointOpts{}, "list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  "+first+" is pinned: "+second+" held half or less of it. It is kept until a later checkpoint holds as much again, or until:\n    caboose -e default checkpoint release "+first+"\n") ||
		!regexp.MustCompile(`(?m)^  `+regexp.QuoteMeta(first)+` .*  \(pinned\)$`).MatchString(out.String()) ||
		!strings.Contains(out.String(), "restore it (it is pinned, so it does not age out):") || strings.Contains(out.String(), "ages out:") {
		t.Errorf("list:\n%s", &out)
	}
	out.Reset()
	if err := d.run(t, checkpointOpts{}, "list", "--json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"pinned": true,
          "pinned_by": "`+second+`"`) {
		t.Errorf("list --json:\n%s", &out)
	}
	out.Reset()
	if err := d.run(t, checkpointOpts{}, "release", second); err == nil || !strings.Contains(err.Error(), "is not pinned") {
		t.Errorf("released one not pinned: %v", err)
	}
	for _, bad := range [][]string{{"release"}, {"release", "a", "b"}, {"release", "--x"}, {"release", es[0].ID.Key + "@latest"}, {"release", "nope"}} {
		if err := d.run(t, checkpointOpts{}, bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := d.run(t, checkpointOpts{}, "release", first); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Released "+first+": it is kept or pruned by the retention rules from now on.") {
		t.Errorf("release: %q", &out)
	}
	noRow(t, d.doctor(false, nil), levelNote, "is pinned")
	if got := d.app.checkpointLine(linkView{}, false, d.seams); strings.Contains(got, "pinned") {
		t.Errorf("status after the release: %q", got)
	}

	// Pins that cannot be read: one problem, nothing pruned.
	if err := os.WriteFile(filepath.Join(checkpoint.StoreDir(d.envDir), "pins.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantRow(t, d.doctor(false, nil), levelProblem, "the pins cannot be read, so nothing is pruned", "pins.json aside")
}

func TestDoctorCheckpointsNotes(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj", "/work/tools")
	ckWriteFile(t, filepath.Join(d.dev, "proj", "a.txt"), "a")
	d.take(t)
	store := checkpoint.StoreDir(d.envDir)

	// Leftovers older than an hour are named; younger ones are a take in
	// progress.
	old, young := filepath.Join(store, ".tmp-old"), filepath.Join(store, ".trash-young")
	ckMkdirs(t, old, young)
	when := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, when, when); err != nil {
		t.Fatal(err)
	}
	c := d.doctor(false, nil)
	wantRow(t, c, levelNote, "1 directory left by a checkpoint that did not finish (.tmp-old)", "")

	// A checkpoint whose meta.json cannot be read.
	s, err := checkpoint.Open(d.home, d.envDir)
	if err != nil {
		t.Fatal(err)
	}
	es, _ := s.List(checkpoint.Key("/work/dev/proj"))
	s.Close()
	meta := filepath.Join(store, es[0].ID.Key, checkpoint.FormatStamp(es[0].Stamp), "meta.json")
	if err := os.WriteFile(meta, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantRow(t, d.doctor(false, nil), levelNote, "/work/dev/proj: 1 checkpoint cannot be read", "")
	if err := os.WriteFile(meta, mustRead(t, filepath.Join(store, es[0].ID.Key, "path.json")), 0o600); err != nil {
		t.Fatal(err)
	}

	// Keys of a path no longer configured.
	d.app.Cfg.Checkpoints = []string{"/work/tools"}
	c = d.doctor(false, nil)
	wantRow(t, c, levelNote, "kept for /work/dev/proj, which is no longer in [checkpoints] paths", "")

	// CABOOSE_DATA_DIR.
	noRow(t, c, levelNote, "CABOOSE_DATA_DIR")
	d.app.Cfg.Getenv = func(k string) string {
		if k == "CABOOSE_DATA_DIR" {
			return "/x"
		}
		return ""
	}
	wantRow(t, d.doctor(false, nil), levelNote, "CABOOSE_DATA_DIR is set", "")
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// With no path set but a store left, its keys are one note each.
func TestDoctorCheckpointsStoreWithoutPaths(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	d.take(t)
	d.app.Cfg.Checkpoints = nil
	c := d.doctor(false, nil)
	wantRow(t, c, levelNote, "kept for /work/dev/proj, which is no longer in [checkpoints] paths", "")
	if c.count(levelProblem) != 0 {
		t.Errorf("\n%s", c)
	}
}

// The status line.
func TestStatusCheckpointsLine(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj", "~/.claude")
	if got := d.app.checkpointLine(linkView{}, false, d.seams); got != "2 paths, none taken yet" {
		t.Errorf("before any: %q", got)
	}
	d.take(t)
	d.at(4 * time.Minute)
	if got := d.app.checkpointLine(linkView{}, true, d.seams); !strings.HasPrefix(got, "2 paths, newest 4 min ago (") || !strings.HasSuffix(got, ")") {
		t.Errorf("healthy: %q", got)
	}
	d.app.Cfg.Checkpoints = nil
	if got := d.app.checkpointLine(linkView{}, true, d.seams); got != "" {
		t.Errorf("no paths: %q", got)
	}
	d.app.Cfg.Checkpoints = []string{"/work/dev/proj"}
	if got := d.app.checkpointLine(linkView{}, true, d.seams); !strings.HasPrefix(got, "1 path, newest 4 min ago") {
		t.Errorf("one path: %q", got)
	}
}

// A failing schedule is shown, whatever it fails on, and never hidden.
func TestStatusCheckpointsLineShowsFailures(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	d.take(t)
	lv := func(s *daemonproto.CheckpointStatus) linkView {
		return linkView{Env: &daemonproto.EnvStatus{Name: "default", Checkpoints: s}}
	}
	for _, tc := range []struct {
		name    string
		running bool
		at      time.Duration
		sched   *daemonproto.CheckpointStatus
		want    string
	}{
		{"exposed", true, 0, &daemonproto.CheckpointStatus{Exposed: "the store is inside root R"},
			"1 path, 1 failing: no checkpoint is taken: the store is inside root R"},
		{"a take failed", false, 0, &daemonproto.CheckpointStatus{Paths: []daemonproto.CheckpointPathStatus{{Path: "/work/dev/proj", Failing: "no space left\x1b[2J"}}},
			"1 path, 1 failing: /work/dev/proj: no space left"},
		{"low space", false, 0, &daemonproto.CheckpointStatus{LowSpace: true},
			"; the disk is low on space"},
		{"below the soft floor, skipped", true, 40 * time.Minute, &daemonproto.CheckpointStatus{LowSpace: true,
			SpaceSkipped: checkpoint.ErrLowSpace.Error() + ": 20480 MiB free, under the 51200 MiB the floor asks"},
			"1 path, 1 failing: scheduled checkpoints are skipped: the disk is below its soft floor (20480 MiB free, under the 51200 MiB the floor asks)"},
		{"hard floor", false, 0, &daemonproto.CheckpointStatus{LowSpace: true, Paths: []daemonproto.CheckpointPathStatus{{Path: "/work/dev/proj", Failing: "the disk is nearly full"}}},
			"1 path, 1 failing: /work/dev/proj: the disk is nearly full"},
		{"stale", true, 40 * time.Minute, nil, "1 path, 1 failing: /work/dev/proj: the newest checkpoint is 40 min ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.at(tc.at)
			got := d.app.checkpointLine(lv(tc.sched), tc.running, d.seams)
			if !strings.Contains(got, tc.want) || strings.ContainsRune(got, 0x1b) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// Low on space alone is no failure: the newest, then the note.
	d.at(0)
	if got := d.app.checkpointLine(lv(&daemonproto.CheckpointStatus{LowSpace: true}), true, d.seams); strings.Contains(got, "failing") ||
		!regexp.MustCompile(`^1 path, (newest .* \(.+\)|none taken yet); the disk is low on space$`).MatchString(got) {
		t.Errorf("soft low space: %q", got)
	}
	// Stale only while the sandbox runs.
	d.at(40 * time.Minute)
	if got := d.app.checkpointLine(linkView{}, false, d.seams); strings.Contains(got, "failing") {
		t.Errorf("stopped sandbox: %q", got)
	}
	// An unresolved path, and two failures counted.
	d.at(0)
	d.app.Cfg.Checkpoints = []string{"/work/dev/proj", "/work/dev/other"}
	got := d.app.checkpointLine(lv(&daemonproto.CheckpointStatus{LowSpace: true}), false, d.seams)
	if !strings.HasPrefix(got, "2 paths, 1 failing: /work/dev/other is not copied: ") {
		t.Errorf("got %q", got)
	}
	// A store that cannot be read.
	d.app.Cfg.Checkpoints = []string{"/work/dev/proj"}
	store := checkpoint.StoreDir(d.envDir)
	if err := os.Chmod(store, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(store, 0o700)
	if os.Geteuid() != 0 {
		if got := d.app.checkpointLine(linkView{}, false, d.seams); !strings.Contains(got, "1 failing: cannot read the store") {
			t.Errorf("unreadable: %q", got)
		}
	}
}

// Status prints the line after the daemon and link lines, with the daemon
// answering, even when the sandbox is not running, and starts nothing.
func TestStatusPrintsCheckpointsLine(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj")
	d.take(t)
	var out bytes.Buffer
	d.app.Stdout, d.app.Stderr = &out, &out
	fd := &fakeDaemon{Stat: &daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "default", Link: daemonproto.LinkRunning,
		Checkpoints: &daemonproto.CheckpointStatus{Exposed: "the store is inside root R"}}}}}
	d.app.DaemonDial = fd.dial
	if err := d.app.Status(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	i, j, k := strings.Index(s, "daemon    :"), strings.Index(s, "link      :"), strings.Index(s, "checkpoints: 1 path, 1 failing: no checkpoint is taken: the store is inside root R\n")
	if i < 0 || j < i || k < j || strings.Index(s, "\nnot running") < k {
		t.Errorf("order or content wrong:\n%s", s)
	}
	for _, d := range fd.dials {
		if d {
			t.Error("status started the daemon")
		}
	}
}

// Status only reads: neither its checkpoints line nor the whole command
// makes the checkpoint store (the environment's checkpoints/), whatever
// the paths, the sandbox and the daemon say, and with or without a
// daemon answering.
func TestStatusMakesNoStore(t *testing.T) {
	d := newCkDoc(t, "/work/dev/proj", "~/.claude", "/work/dev/other")
	store := checkpoint.StoreDir(d.envDir)
	noStore := func(when string) {
		t.Helper()
		if _, err := os.Lstat(store); !os.IsNotExist(err) {
			t.Fatalf("%s: %s was made (%v)", when, store, err)
		}
	}
	noStore("at the start")
	for _, running := range []bool{false, true} {
		for _, sched := range []*daemonproto.CheckpointStatus{nil, {LowSpace: true}, {Exposed: "the store is inside root R"}} {
			lv := linkView{Env: &daemonproto.EnvStatus{Name: "default", Checkpoints: sched}}
			if got := d.app.checkpointLine(lv, running, d.seams); !strings.HasPrefix(got, "3 paths, ") {
				t.Errorf("line %q", got)
			}
			noStore(fmt.Sprintf("the line, running %v, %+v", running, sched))
		}
	}

	var out bytes.Buffer
	d.app.Stdout, d.app.Stderr = &out, &out
	d.app.DaemonDial = (&fakeDaemon{Stat: &daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "default",
		Link: daemonproto.LinkRunning, Checkpoints: &daemonproto.CheckpointStatus{}}}}}).dial
	for _, status := range []string{"", "exited"} {
		d.box.Status = status
		if err := d.app.Status(); err != nil {
			t.Fatal(err)
		}
		noStore("caboose status with the daemon, sandbox " + status)
	}
	if !strings.Contains(out.String(), "checkpoints: 3 paths, ") {
		t.Errorf("no checkpoints line:\n%s", &out)
	}
	d.app.DaemonDial = func(context.Context, bool) (DaemonCaller, error) {
		return nil, daemonproto.NewError(daemonproto.KindUnavailable, "no daemon answers")
	}
	if err := d.app.Status(); err != nil {
		t.Fatal(err)
	}
	noStore("caboose status without the daemon")
}
