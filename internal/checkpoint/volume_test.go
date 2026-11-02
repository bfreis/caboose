package checkpoint

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// copyClone stands in for a copy-on-write clone where the filesystem has
// none (most Linux ones): a plain copy of what src holds.
func copyClone(src, dir *os.File, name string) error {
	out, err := openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, io.NewSectionReader(src, 0, 1<<40))
	return err
}

// useCopyClone has the clones be copies for the test, unless the
// platform clones for real.
func useCopyClone(t *testing.T) {
	t.Helper()
	old := cloneFile
	cloneFile = copyClone
	t.Cleanup(func() { cloneFile = old })
}

// fakeHold is a Holder that records what it did, and can fail before or
// after during.
type fakeHold struct {
	calls  []string
	before error // the hold fails before during
	after  error // the hold fails after during ran (a thaw that found the freeze ended)
	frozen bool
}

func (h *fakeHold) Hold(_ context.Context, during func() error) (Held, error) {
	h.calls = append(h.calls, "hold")
	if h.before != nil {
		return Held{}, h.before
	}
	err := during()
	h.calls = append(h.calls, "during")
	held := Held{Frozen: h.frozen, FrozenFor: 42 * time.Millisecond}
	if h.after != nil {
		return held, errors.Join(err, h.after)
	}
	return held, err
}

// volumeSrc is a volume "cache" whose disk is a file holding data.
func volumeSrc(t *testing.T, data string, h Holder) Source {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cache.img"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return Source{Path: "~/cache", Kind: SourceVolume, Top: dir, Rel: "cache.img", File: true, Volume: "cache", Hold: h}
}

func takeVolume(t *testing.T, s *Store, src Source, now func() time.Time) (TakeResult, error) {
	t.Helper()
	l, err := s.Lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	return TakeLocked(context.Background(), l, s, src, ReasonInterval, TakeOptions{Now: now, Statfs: plenty})
}

// A volume's checkpoint is a clone of its disk made under the hold, kind
// volume, saying which volume, its disk's size, and how long it was
// frozen.
func TestTakeVolume(t *testing.T) {
	useCopyClone(t)
	s := takeStore(t)
	h := &fakeHold{frozen: true}
	src := volumeSrc(t, "ext4 bytes", h)
	c := &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	r, err := takeVolume(t, s, src, c.now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.calls, ",") != "hold,during" {
		t.Errorf("hold %q", h.calls)
	}
	m := r.Meta
	if m.Kind != KindVolume || m.Entries != 1 || m.Bytes != 10 || m.Volume == nil ||
		*m.Volume != (VolumeMeta{Name: "cache", Bytes: 10, Frozen: true, FrozenMS: 42}) {
		t.Errorf("meta %+v, volume %+v", m, m.Volume)
	}
	if m.Method != cloneMethod() {
		t.Errorf("method %s", m.Method)
	}
	got, err := os.ReadFile(filepath.Join(s.checkpointDir(r.ID), TreeName))
	if err != nil || string(got) != "ext4 bytes" {
		t.Errorf("tree %q, %v", got, err)
	}
	// Read back from the store, as list does.
	es, err := s.List(Key("~/cache"))
	if err != nil || len(es) != 1 || es[0].Meta.Volume == nil || es[0].Meta.Volume.Name != "cache" || es[0].Meta.Kind != KindVolume {
		t.Errorf("listed %+v, %v", es, err)
	}
	// Taken with the VM stopped: not frozen, no time.
	h2 := &fakeHold{}
	src.Hold = h2
	r, err = takeVolume(t, s, src, c.now)
	if err != nil || r.Meta.Volume.Frozen || r.Meta.Volume.FrozenMS != 0 {
		t.Errorf("unfrozen: %+v %v", r.Meta.Volume, err)
	}
}

// A hold that fails, before or after the clone, leaves no checkpoint and
// nothing in the store: a clone not held still throughout is never kept.
func TestTakeVolumeHoldFails(t *testing.T) {
	useCopyClone(t)
	for name, h := range map[string]*fakeHold{
		"before": {before: errors.New("freezing: not mounted")},
		"after":  {after: errors.New("thawed before its clone was done")},
	} {
		s := takeStore(t)
		_, err := takeVolume(t, s, volumeSrc(t, "x", h), nil)
		if err == nil || !strings.Contains(err.Error(), "volume cache") {
			t.Errorf("%s: err %v", name, err)
		}
		if es, _ := s.List(Key("~/cache")); len(es) != 0 {
			t.Errorf("%s: kept %v", name, es)
		}
		if l := leftovers(t, s); len(l) != 0 {
			t.Errorf("%s: left %q", name, l)
		}
	}
}

// No holder: refused, nothing cloned. A clone that cannot be made (the
// filesystem has none) is ErrNoClone; a symlink for the disk is refused.
func TestTakeVolumeRefused(t *testing.T) {
	useCopyClone(t)
	s := takeStore(t)
	src := volumeSrc(t, "x", nil)
	if _, err := takeVolume(t, s, src, nil); err == nil || !strings.Contains(err.Error(), "no volume to hold") {
		t.Errorf("no holder: %v", err)
	}
	cloneFile = func(*os.File, *os.File, string) error { return unix.EXDEV }
	src.Hold = &fakeHold{frozen: true}
	if _, err := takeVolume(t, s, src, nil); !errors.Is(err, ErrNoClone) || !strings.Contains(err.Error(), "the store ("+s.Dir()+", under CABOOSE_HOME)") || !strings.Contains(err.Error(), "(in "+src.Top+", the data dir's vm/volumes, which CABOOSE_DATA_DIR moves)") {
		t.Errorf("no clone: %v", err)
	}
	cloneFile = copyClone
	link := volumeSrc(t, "x", &fakeHold{})
	if err := os.Rename(filepath.Join(link.Top, "cache.img"), filepath.Join(link.Top, "real.img")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.img", filepath.Join(link.Top, "cache.img")); err != nil {
		t.Fatal(err)
	}
	if _, err := takeVolume(t, s, link, nil); !errors.Is(err, ErrSourceSymlink) {
		t.Errorf("symlink: %v", err)
	}
	if es, _ := s.List(Key("~/cache")); len(es) != 0 || len(leftovers(t, s)) != 0 {
		t.Errorf("kept %v", es)
	}
}

// A volume's disk smaller than before (a shrink) is no sharp shrink of
// what it holds: nothing is pinned or flagged.
func TestTakeVolumeNoShrinkFlag(t *testing.T) {
	useCopyClone(t)
	s := takeStore(t)
	c := &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	src := volumeSrc(t, strings.Repeat("x", 300<<20), &fakeHold{})
	if _, err := takeVolume(t, s, src, c.now); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(src.Top, "cache.img"), 1<<20); err != nil {
		t.Fatal(err)
	}
	r, err := takeVolume(t, s, src, c.now)
	if err != nil || r.Meta.Shrunk != nil || r.Pinned != nil {
		t.Errorf("shrunk %+v, pinned %v, %v", r.Meta.Shrunk, r.Pinned, err)
	}
}

// Volumes' checkpoints are kept by the same rules as any other: of three
// taken within one hour five hours ago, one is kept once a new one is
// taken; the newest always.
func TestTakeVolumeRetention(t *testing.T) {
	useCopyClone(t)
	s := takeStore(t)
	src := volumeSrc(t, "x", &fakeHold{frozen: true})
	base := time.Date(2026, 10, 31, 7, 0, 0, 0, time.UTC)
	for _, at := range []time.Duration{0, 10 * time.Minute, 20 * time.Minute} {
		c := &clock{t: base.Add(at)}
		if _, err := takeVolume(t, s, src, c.now); err != nil {
			t.Fatal(err)
		}
	}
	c := &clock{t: base.Add(5 * time.Hour)}
	r, err := takeVolume(t, s, src, c.now)
	if err != nil || r.PruneErr != nil {
		t.Fatal(err, r.PruneErr)
	}
	es, err := s.List(Key("~/cache"))
	if err != nil || len(es) != 2 || len(r.Pruned) != 2 {
		t.Fatalf("kept %d, pruned %v, %v", len(es), r.Pruned, err)
	}
	for _, p := range r.Pruned {
		if _, err := os.Lstat(s.checkpointDir(p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
}

// A volume's checkpoint is never copied into a directory: it is restored
// in place.
func TestRestoreRefusesVolume(t *testing.T) {
	useCopyClone(t)
	s := takeStore(t)
	r, err := takeVolume(t, s, volumeSrc(t, "x", &fakeHold{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Restore(context.Background(), s, r.ID.String(), filepath.Join(t.TempDir(), "out"),
		RestoreOptions{CabooseHome: t.TempDir(), Paths: emptyPaths{}})
	if !errors.Is(err, ErrIsVolume) {
		t.Errorf("err %v", err)
	}
}

type emptyPaths struct{}

func (emptyPaths) SandboxPaths(context.Context) (PathSet, error) { return PathSet{}, nil }

// A take that fails the same way again is not logged again; the take that
// then succeeds logs how many more times it came.
func TestTakeLogsRepeatedFailureOnce(t *testing.T) {
	useCopyClone(t)
	s := takeStore(t)
	h := &fakeHold{before: errors.New("boom"), frozen: true}
	src := volumeSrc(t, "x", h)
	for range 3 {
		if _, err := takeVolume(t, s, src, nil); err == nil {
			t.Fatal("no failure")
		}
	}
	h.before = nil
	if _, err := takeVolume(t, s, src, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(), logName))
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if strings.Count(log, "failed: volume cache: boom") != 1 || !strings.Contains(log, "checkpoint of ~/cache: the failure logged before came 2 more time(s), then it succeeded") {
		t.Errorf("log:\n%s", log)
	}
}
