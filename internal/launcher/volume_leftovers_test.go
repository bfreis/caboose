package launcher

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
)

// A start never makes an empty disk for a volume whose disk is missing
// while an interrupted restore's files are beside it: it refuses, saying
// how to put back the disk as it was, or to finish the restore.
func TestStartRefusesAfterInterruptedRestore(t *testing.T) {
	c := &config.Config{Volumes: []config.Volume{{Name: "cache", Path: "/home/agent/.cache/x", Size: 8 << 30}}}
	h, _ := scratchHost(t, c)
	disk := h.a.volumeDisk("cache")
	if err := os.MkdirAll(filepath.Dir(disk), 0o700); err != nil {
		t.Fatal(err)
	}
	before := disk + checkpoint.BeforeRestoreSuffix
	copy1 := filepath.Join(filepath.Dir(disk), checkpoint.RestoringName("cache", time.Unix(1, 0)))
	copy2 := filepath.Join(filepath.Dir(disk), checkpoint.RestoringName("cache", time.Unix(2, 0)))
	for _, p := range []string{before, copy1} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.Volume("cache")
	if err == nil || !strings.Contains(err.Error(), "'mv "+before+" "+disk+"' puts it back") || !strings.Contains(err.Error(), "'rm "+copy1+"' deletes the checkpoint's copy") {
		t.Errorf("the disk as it was beside: %v", err)
	}
	if isFile(disk) {
		t.Fatal("made a disk")
	}
	// Only copies: the volume had no disk before the restore.
	if err := os.Remove(before); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copy2, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = h.Volume("cache")
	if err == nil || !strings.Contains(err.Error(), "'mv "+copy2+" "+disk+"' finishes the restore ('rm "+copy1+"' deletes older copies), or 'rm "+copy1+" "+copy2+"' starts the volume empty") {
		t.Errorf("copies only: %v", err)
	}
	if isFile(disk) {
		t.Fatal("made a disk")
	}
	// Another volume's, or a shrink's backup, stops nothing.
	for _, p := range []string{copy1, copy2} {
		if err := os.Rename(p, strings.Replace(p, ".cache.img", ".other.img", 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(disk+".before-shrink", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := h.Volume("cache"); err != nil || !isFile(d.Path) {
		t.Errorf("with another volume's files: %v", err)
	}
}

// volume rm refuses while a restore's leftovers are beside the disk,
// saying what each is; --delete-disk deletes them with it, after asking.
func TestVolumeRmRestoreLeft(t *testing.T) {
	one := strings.Replace(adminConfig, "[vm.big.volume.cache]", "[vm.big.volume.other]", 1)
	e, _ := adminEnv(t, one, nil)
	disk := e.makeDisk("cache", 8<<30)
	before := disk + checkpoint.BeforeRestoreSuffix
	if err := os.WriteFile(before, []byte("then"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(filepath.Dir(disk), checkpoint.RestoringName("cache", time.Unix(1, 0)))
	if err := os.WriteFile(cp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := e.readConfig()
	err := e.a.Volume([]string{"rm", "cache"})
	if err == nil || !strings.Contains(err.Error(), before+" (") || !strings.Contains(err.Error(), cp+" (") || !strings.Contains(err.Error(), "add --delete-disk, which deletes them with the disk") || e.readConfig() != cfg {
		t.Errorf("no flag: %v", err)
	}
	e.a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("n\n")), nil }
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err == nil || !isFile(before) || !isFile(cp) || !isFile(disk) {
		t.Errorf("answered no: %v", err)
	}
	if !strings.Contains(e.errb.String(), "and "+before+" (") || !strings.Contains(e.errb.String(), "the disk as it was before a checkpoint's restore") {
		t.Errorf("what it deletes:\n%s", e.errb)
	}
	e.a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("y\n")), nil }
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if isFile(before) || isFile(cp) || isFile(disk) || !strings.Contains(e.errb.String(), "deleted "+before) {
		t.Errorf("answered yes: %v %v %v\n%s", isFile(before), isFile(cp), isFile(disk), e.errb)
	}
}

func TestLeftoverOf(t *testing.T) {
	for _, c := range []struct{ file, vol, kind string }{
		{"cache.img.before-restore", "cache", leftoverBeforeRestore},
		{"cache.img.before-shrink", "cache", leftoverBeforeShrink},
		{".cache.img.restoring-17", "cache", leftoverRestoring},
		{"docker.img.before-restore", "docker", leftoverBeforeRestore},
		{"cache.img", "", ""},
		{"Bad.img.before-restore", "", ""},
		{".cache.img.restoring-", "", ""},
		{".cache.img.restoring-1a", "", ""},
		{"cache.img.restoring-1", "", ""},
		{"volumes.lock", "", ""},
	} {
		vol, kind, ok := leftoverOf(c.file)
		if vol != c.vol || kind != c.kind || ok != (c.kind != "") {
			t.Errorf("leftoverOf(%q) = %q %q %v", c.file, vol, kind, ok)
		}
	}
}
