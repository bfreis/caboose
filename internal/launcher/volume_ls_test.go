package launcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
)

// lsConfig declares cache in two vm profiles, data in the one in use alone,
// and web in a container profile.
const lsConfig = adminConfig + `
[vm.default.volume.data]
path = "/data"
size = "8G"

[container.default.volume.web]
path = "/srv/web"
`

// lsEnv is lsConfig with disks for cache, a left-over old and dockerd,
// the VM running with cache on caboose-vmm's disk; a disk takes up a
// sixteenth of its size.
func lsEnv(t *testing.T) *setupEnv {
	t.Helper()
	e, _ := adminEnv(t, lsConfig, map[string]backend.VolumeState{"cache": {Running: true, Has: true, Live: true}})
	alloc := diskAllocated
	diskAllocated = func(fi os.FileInfo) int64 { return fi.Size() / 16 }
	t.Cleanup(func() { diskAllocated = alloc })
	e.makeDisk("cache", 96<<30)
	e.makeDisk("old", 32<<30)
	e.makeDisk("docker", 64<<30)
	// web's profile is a container's, whose volumes are no disks: a disk
	// of that name is still the name's, not left over.
	e.makeDisk("web", 4<<30)
	// Not volumes' disks: a shrink's backup, a name no volume has, and
	// files no volume's name is in.
	for _, n := range []string{"cache.img" + backend.BackupSuffix, "Not-A-Volume.img", "Not-A-Volume.img.before-restore", ".cache.img.restoring-x", "volumes.lock"} {
		if err := os.WriteFile(filepath.Join(e.a.volumesDir(), n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// What restores left: web's disk as it was, a copy of a checkpoint of
	// cache, and old's disk under a second name (a restore interrupted
	// before its last step).
	before := filepath.Join(e.a.volumesDir(), "web.img"+checkpoint.BeforeRestoreSuffix)
	if err := os.WriteFile(before, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(before, 2<<30); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.a.volumesDir(), checkpoint.RestoringName("cache", time.Unix(1700000000, 0))), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(e.a.volumeDisk("old"), e.a.volumeDisk("old")+checkpoint.BeforeRestoreSuffix); err != nil {
		t.Fatal(err)
	}
	return e
}

// caboose volume ls shows every volume by name, the profiles that declare
// it, its disk and how the running VM has it, a disk no profile declares
// with the command that deletes it, and dockerd's disk last.
func TestVolumeLsGolden(t *testing.T) {
	e := lsEnv(t)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls"}); err != nil {
		t.Fatal(err)
	}
	golden(t, &boxApp{tmp: filepath.Dir(e.a.Cfg.CabooseHome)}, "volume-ls.out", out.String())
}

// --json is the same listing, for a program.
func TestVolumeLsJSON(t *testing.T) {
	e := lsEnv(t)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	var l volumeListing
	if err := json.Unmarshal(out.Bytes(), &l); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	byName := map[string]listedVolume{}
	for _, v := range l.Volumes {
		byName[v.Name] = v
	}
	if len(l.Volumes) != 4 {
		t.Errorf("volumes %+v", l.Volumes)
	}
	c := byName["cache"]
	if c.Attached != attachedLive || c.Orphan || c.Disk == nil || c.Disk.Size != 96<<30 || c.Disk.Allocated != 6<<30 ||
		len(c.Profiles) != 2 || c.Profiles[0] != (declaredIn{Profile: "vm.big", Path: "/home/agent/.cache/go-build", Size: 128 << 30}) ||
		!c.Profiles[1].InUse {
		t.Errorf("cache %+v", c)
	}
	if o := byName["old"]; !o.Orphan || o.Disk == nil || len(o.Profiles) != 0 {
		t.Errorf("old %+v", o)
	}
	if d := byName["data"]; d.Disk != nil || d.Orphan || d.Attached != "" {
		t.Errorf("data %+v", d)
	}
	if w := byName["web"]; len(w.Profiles) != 1 || w.Profiles[0].Size != 0 || w.Orphan || w.Disk == nil {
		t.Errorf("web %+v", w)
	}
	if l.Docker == nil || l.Docker.Size != 64<<30 || l.Docker.DockerSize != 64<<30 {
		t.Errorf("docker %+v", l.Docker)
	}
}

// With nothing to show, --json says so with empty lists, as it does a
// left-over disk's profiles.
func TestVolumeLsJSONEmpty(t *testing.T) {
	e, _ := adminEnv(t, "[link]\nhost_exec = true\n", nil)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls", "--json"}); err != nil || !strings.Contains(out.String(), `"volumes": []`) {
		t.Errorf("%v %s", err, out)
	}
	e, _ = adminEnv(t, "[link]\nhost_exec = true\n", nil)
	e.makeDisk("old", 8<<30)
	out = e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls", "--json"}); err != nil || !strings.Contains(out.String(), `"profiles": []`) {
		t.Errorf("%v %s", err, out)
	}
}

// With nothing to show, it says how to add a volume; a volume on the
// framework's disk says so; an unknown argument is a usage error.
func TestVolumeLsStates(t *testing.T) {
	e, _ := adminEnv(t, "[link]\nhost_exec = true\n", nil)
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls"}); err != nil || !strings.Contains(out.String(), "No volumes") {
		t.Errorf("%v %s", err, out)
	}
	e, _ = adminEnv(t, adminConfig, map[string]backend.VolumeState{"cache": {Running: true, Has: true}})
	out = e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls"}); err != nil || !strings.Contains(out.String(), "on Virtualization.framework's own disk") {
		t.Errorf("%v %s", err, out)
	}
	// A stopped VM has nothing attached.
	e, _ = adminEnv(t, adminConfig, map[string]backend.VolumeState{"cache": {Has: true, Live: true}})
	out = e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls"}); err != nil || !strings.Contains(out.String(), "cache: no disk yet") || strings.Contains(out.String(), "attached") {
		t.Errorf("%v %s", err, out)
	}
	if err := e.a.Volume([]string{"ls", "--all"}); err == nil || !strings.Contains(err.Error(), "usage: caboose volume ls [--json] (unknown flag --all)") {
		t.Errorf("%v", err)
	}
}

// caboose prune deletes no volume's disk or docker volume, but names each
// one no profile declares, with the command that deletes it: never
// dockerd's, nor one only a container profile declares.
func TestPruneNamesLeftOverVolumes(t *testing.T) {
	b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
	b.mountLocal("local/linux-arm64")
	mkdirs(t, b.data, "local/linux-arm64/share/claude", "local/linux-arm64/bin", "vm/volumes")
	b.Cfg.CabooseHome, b.Cfg.Home = filepath.Join(b.tmp, "caboose-home"), b.tmp
	f, err := config.ParseFile("config.toml", []byte(lsConfig))
	if err != nil {
		t.Fatal(err)
	}
	b.Cfg.File = f
	for _, n := range []string{"cache", "old", "docker", "web"} {
		if err := os.WriteFile(b.volumeDisk(n), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"cache.img" + checkpoint.BeforeRestoreSuffix, checkpoint.RestoringName("cache", time.Unix(1, 0))} {
		if err := os.WriteFile(filepath.Join(b.volumesDir(), n), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b.box.EngineVols = []backend.EngineVolume{{Name: "web", Engine: "caboose-default-web", Size: "3MB"}, {Name: "gone", Engine: "caboose-default-gone", Size: "1GB"}}
	if err := b.Prune(nil); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	_, errs := b.said()
	if !strings.Contains(errs, "caboose: "+b.volumeDisk("cache")+checkpoint.BeforeRestoreSuffix+" (") || !strings.Contains(errs, "is volume cache's disk as it was before a checkpoint's restore") ||
		!strings.Contains(errs, ".cache.img.restoring-1000000000 (") || !strings.Contains(errs, "is a copy of a checkpoint that an interrupted restore of volume cache left") {
		t.Errorf("leftovers not named:\n%s", errs)
	}
	if !strings.Contains(errs, "docker volume caboose-default-gone (1GB) is volume gone's, which no profile declares;\ncaboose:   'caboose volume rm gone --delete-disk' deletes it.\n") ||
		strings.Contains(errs, "caboose-default-web") || len(b.box.RemovedVolumes) != 0 {
		t.Errorf("docker volumes, stderr:\n%s", errs)
	}
	if !strings.Contains(errs, "caboose: "+b.volumeDisk("old")+" (") || !strings.Contains(errs, "is the disk of volume old, which no profile declares;\ncaboose:   'caboose volume rm old --delete-disk' deletes it.\n") ||
		strings.Contains(errs, "the disk of volume cache") || strings.Contains(errs, "docker.img") || strings.Contains(errs, "web.img") || !isFile(b.volumeDisk("old")) {
		t.Errorf("stderr:\n%s", errs)
	}
}

// Under container, ls lists the container's docker volumes with their
// size ("size unknown" when the engine said none): a declared one by its
// profiles, one no profile declares as left over, and a declared one the
// engine has not got yet as made at the container's creation; the engine
// not answering fails it.
func TestVolumeLsContainer(t *testing.T) {
	e, b := adminEnv(t, ctrConfig+"\n[container.default.volume.web]\npath = \"/srv/web\"\n", nil)
	ctr := e.a.Cfg.Container
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: ctr + "-cache", Size: "1.2GB"}, {Name: "old", Engine: ctr + "-old", Size: "0B"}, {Name: "slow", Engine: ctr + "-slow"}}
	out := e.a.Stdout.(*bytes.Buffer)
	if err := e.a.Volume([]string{"ls"}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"cache: docker volume " + ctr + "-cache, 1.2GB\n",
		"old: docker volume " + ctr + "-old, 0B; no profile declares it: 'caboose volume rm old --delete-disk' deletes it\n",
		"slow: docker volume " + ctr + "-slow, size unknown; no profile declares it",
		"web: no docker volume yet: a container of a profile that declares it makes " + ctr + "-web as it is created\n",
	} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("no %q in:\n%s", w, out)
		}
	}
	out.Reset()
	if err := e.a.Volume([]string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	var l volumeListing
	if err := json.Unmarshal(out.Bytes(), &l); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	byName := map[string]listedVolume{}
	for _, v := range l.Volumes {
		byName[v.Name] = v
	}
	if c := byName["cache"]; c.DockerVolume == nil || *c.DockerVolume != (backend.EngineVolume{Name: "cache", Engine: ctr + "-cache", Size: "1.2GB"}) || c.Orphan || len(c.Profiles) != 2 {
		t.Errorf("cache %+v", c)
	}
	if o := byName["old"]; !o.Orphan || o.DockerVolume == nil {
		t.Errorf("old %+v", o)
	}
	if w := byName["web"]; w.DockerVolume != nil || w.Orphan {
		t.Errorf("web %+v", w)
	}
	b.ListVolumesErr = errors.New("engine down")
	if err := e.a.Volume([]string{"ls"}); err == nil || !strings.Contains(err.Error(), "listing the docker volumes of "+ctr+": engine down") {
		t.Errorf("err = %v", err)
	}
	// Under vm, docker is not asked.
	v, vb := adminEnv(t, adminConfig, nil)
	vb.EngineVols = []backend.EngineVolume{{Name: "old", Engine: ctr + "-old"}}
	out = v.a.Stdout.(*bytes.Buffer)
	if err := v.a.Volume([]string{"ls"}); err != nil || strings.Contains(out.String(), "old") {
		t.Errorf("vm: %v\n%s", err, out)
	}
}
