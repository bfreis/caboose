package launcher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
)

// vmCkEnv is newCkEnv under vm, with volume cache at ~/cache in the
// profile, and the VM created with it, an older volume old at /data/old,
// and dockerd's disk.
func vmCkEnv(t *testing.T) *ckEnv {
	t.Helper()
	e := newCkEnv(t)
	e.app.Cfg.Isolation = config.KindVM
	e.app.Cfg.Volumes = []config.Volume{{Name: "cache", Path: config.ContainerHome + "/cache", Size: 64 << 30}}
	e.box.Status = "running"
	e.box.SandboxLabels = map[string]string{assets.LabelVolumes: `{"cache":"/home/agent/cache","docker":"/var/lib/docker","old":"/data/old"}`}
	for _, n := range []string{"cache", "old", "docker"} {
		ckWriteFile(t, e.app.volumeDisk(n), "disk")
	}
	return e
}

// Under vm a volume's path is its disk, whether the profile or the VM
// names it, ahead of a keep entry; a path inside one, and dockerd's, are
// not copied, saying why.
func TestCheckpointVolumeSources(t *testing.T) {
	e := vmCkEnv(t)
	ckWriteFile(t, filepath.Join(e.dataDir, "home", ".config", "caboose", "sandbox.toml"), "[[keep]]\npath = \"~/cache\"\n")
	got, why := e.resolve(t, "~/cache", "/data/old", "~/cache/sub", "/var/lib/docker", "/var/lib/docker/volumes")
	vdir := phys(t, e.app.volumesDir())
	for _, c := range []struct{ p, name string }{{"~/cache", "cache"}, {"/data/old", "old"}} {
		s, ok := got[c.p]
		if !ok {
			t.Errorf("%s unresolved: %s", c.p, why[c.p])
			continue
		}
		if s.Kind != checkpoint.SourceVolume || s.Top != vdir || s.Rel != c.name+".img" || !s.File || s.Volume != c.name || s.Hold == nil {
			t.Errorf("%s: %+v", c.p, s)
		}
	}
	wantUnresolved(t, got, why, "~/cache/sub", "inside volume cache (/home/agent/cache), whose disk a checkpoint clones whole: list ~/cache")
	wantUnresolved(t, got, why, "/var/lib/docker", "dockerd's own disk")
	wantUnresolved(t, got, why, "/var/lib/docker/volumes", "dockerd's own disk")

	// No VM yet, so no label: dockerd's path still says why it is not
	// copied.
	e = vmCkEnv(t)
	e.box.SandboxLabels = nil
	got, why = e.resolve(t, "/var/lib/docker")
	wantUnresolved(t, got, why, "/var/lib/docker", "dockerd's own disk")

	// A volume whose disk is not there yet: not resolved.
	e = vmCkEnv(t)
	e.app.Cfg.Volumes = append(e.app.Cfg.Volumes, config.Volume{Name: "new", Path: "/data/new", Size: 1 << 30})
	got, why = e.resolve(t, "/data/new")
	wantUnresolved(t, got, why, "/data/new", "does not exist")

	// Not under vm: no volumes, the path resolved as any other.
	e = vmCkEnv(t)
	e.app.Cfg.Isolation = config.KindContainer
	got, why = e.resolve(t, "/data/old")
	if s, ok := got["/data/old"]; ok && s.Kind == checkpoint.SourceVolume {
		t.Errorf("a volume under container: %+v", s)
	}
	if !strings.Contains(why["/data/old"], "not a writable part of a root") {
		t.Errorf("under container: %q", why["/data/old"])
	}
}

// holdBox is a fake sandbox that holds volumes: what the hold was asked.
type holdBox struct {
	*backendtest.Fake
	asked []string
}

func (b *holdBox) HoldVolume(_ context.Context, name string, during func() error) (backend.VolumeHeld, error) {
	b.asked = append(b.asked, name)
	return backend.VolumeHeld{Frozen: true, FrozenFor: 7 * time.Millisecond}, during()
}

func (b *holdBox) HoldStopped(during func() error) error { return during() }

// The source's hold is the sandbox's, asked for the volume by name when
// the checkpoint is taken; a sandbox that holds no volumes refuses.
func TestCheckpointVolumeHold(t *testing.T) {
	e := vmCkEnv(t)
	got, _ := e.resolve(t, "~/cache")
	s := got["~/cache"]
	if _, err := s.Hold.Hold(context.Background(), func() error { return nil }); err == nil || !strings.Contains(err.Error(), "no VM") {
		t.Errorf("a sandbox that holds no volumes: %v", err)
	}
	hb := &holdBox{Fake: e.box}
	e.app.Backend = hb
	ran := false
	held, err := s.Hold.Hold(context.Background(), func() error { ran = true; return nil })
	if err != nil || !ran || !held.Frozen || held.FrozenFor != 7*time.Millisecond || strings.Join(hb.asked, ",") != "cache" {
		t.Errorf("held %+v, %v, ran %v, asked %q", held, err, ran, hb.asked)
	}
}

func TestVolumeWords(t *testing.T) {
	if got := volumeWords("cache", 64<<30, true, 12); got != "volume cache's disk, 64G, frozen 12 ms" {
		t.Errorf("%q", got)
	}
	if got := volumeWords("cache", 64<<30, false, 0); got != "volume cache's disk, 64G, the VM stopped" {
		t.Errorf("%q", got)
	}
}
