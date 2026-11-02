package launcher

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm"
)

// stateBox is a sandbox whose VM has volumes as the test says.
type stateBox struct {
	*backendtest.Fake
	states map[string]backend.VolumeState
	// running is the state of a name states lacks.
	running bool
}

func (b *stateBox) VolumeState(name string) (backend.VolumeState, error) {
	if s, ok := b.states[name]; ok {
		return s, nil
	}
	return backend.VolumeState{Running: b.running}, nil
}

func (b *stateBox) ResizeVolume(_ context.Context, _ string, _ int64, _ func(string, ...any)) (backend.VolumeResize, error) {
	return backend.VolumeResize{}, errors.New("not in these tests")
}

const adminConfig = `isolation = "vm.default"

[vm.default]
cpus = 2

# the build cache
[vm.default.volume.cache]
path = "~/.cache/go-build"
size = "64G"

[vm.big]
memory = "16G"

[vm.big.volume.cache]
path = "~/.cache/go-build"
size = "128G"

[container.default]

# the links
[link]
host_exec = true
`

// adminEnv is an environment whose config.toml is config, its VM's
// volumes as states say, with this machine's disk ample.
func adminEnv(t *testing.T, config string, states map[string]backend.VolumeState) (*setupEnv, *stateBox) {
	t.Helper()
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig(config)
	free := vmFreeSpace
	vmFreeSpace = func(string) (int64, error) { return 1 << 50, nil }
	t.Cleanup(func() { vmFreeSpace = free })
	b := &stateBox{Fake: &backendtest.Fake{}, states: states}
	e.a.Backend = b
	return e, b
}

// readConfig is config.toml as it is now.
func (e *setupEnv) readConfig() string {
	e.t.Helper()
	b, err := os.ReadFile(e.configPath())
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// makeDisk makes volume name's disk, size long.
func (e *setupEnv) makeDisk(name string, size int64) string {
	e.t.Helper()
	p := e.a.volumeDisk(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Truncate(p, size); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// loaded is config.toml as a launch loads it.
func (e *setupEnv) loaded() *config.Config {
	e.t.Helper()
	c, err := config.Load(e.a.Cfg.Getenv, config.OSFS{}, e.a.Cfg.Env)
	if err != nil {
		e.t.Fatalf("%v\n%s", err, e.readConfig())
	}
	return c
}

// caboose volume create adds the table after the profile's own, with a
// vm volume's size (64G unless given), and says what comes next: the
// disk at the next start, or a restart for a running VM.
func TestVolumeCreate(t *testing.T) {
	e, _ := adminEnv(t, adminConfig, nil)
	if err := e.a.Volume([]string{"create", "data", "/data"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if !strings.Contains(e.errb.String(), "the VM's creation, at the next start, makes its disk") {
		t.Errorf("no VM: %s", e.errb)
	}
	want := "[vm.default.volume.cache]\npath = \"~/.cache/go-build\"\nsize = \"64G\"\n\n[vm.default.volume.data]\npath = \"/data\"\nsize = \"64G\"\n\n[vm.big]\n"
	if got := e.readConfig(); !strings.Contains(got, want) || !strings.Contains(got, "# the build cache\n") || !strings.Contains(got, "# the links\n[link]") {
		t.Errorf("config.toml:\n%s", got)
	}
	if c := e.loaded(); len(c.Volumes) != 2 || c.Volumes[1] != (config.Volume{Name: "data", Path: "/data", Size: 64 << 30}) {
		t.Errorf("volumes %+v", c.Volumes)
	}
	for _, w := range []string{`config.toml: [vm.default.volume.data] path = "/data", size = "64G"`, "the VM's creation"} {
		if !strings.Contains(e.errb.String(), w) {
			t.Errorf("said %s, not %q", e.errb, w)
		}
	}
	if isFile(e.a.volumeDisk("data")) {
		t.Error("the disk was made")
	}

	// Another profile, a size of its own, a running VM: the restart is
	// the profile in use's.
	e, b := adminEnv(t, adminConfig, nil)
	b.running = true
	if err := e.a.Volume([]string{"create", "--size", "8G", "web", "/srv/web", "--profile=vm.big"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.readConfig(), "[vm.big.volume.web]\npath = \"/srv/web\"\nsize = \"8G\"\n\n[container.default]") ||
		!strings.Contains(e.errb.String(), "vm.big is not the profile in use (vm.default)") || strings.Contains(e.errb.String(), "restart") {
		t.Errorf("another profile:\n%s\n%s", e.readConfig(), e.errb)
	}
	e, b = adminEnv(t, adminConfig, nil)
	b.Status = "running"
	if err := e.a.Volume([]string{"create", "web", "/srv/web"}); err != nil || !strings.Contains(e.errb.String(), "the VM runs without it: 'caboose restart' recreates the VM with the volume") {
		t.Errorf("running: %v\n%s", err, e.errb)
	}
	e, b = adminEnv(t, adminConfig, nil)
	b.Status = "exited"
	if err := e.a.Volume([]string{"create", "web", "/srv/web"}); err != nil || !strings.Contains(e.errb.String(), "the VM, stopped, was created without it") {
		t.Errorf("stopped: %v\n%s", err, e.errb)
	}

	// A disk left over is the volume's, and said so.
	e, _ = adminEnv(t, adminConfig, nil)
	e.makeDisk("old", 96<<30)
	if err := e.a.Volume([]string{"create", "old", "/old", "--size", "32G"}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"is here already (96G", "with what it holds", "it is larger than 32G: kept"} {
		if !strings.Contains(e.errb.String(), w) {
			t.Errorf("said %s, not %q", e.errb, w)
		}
	}
}

// Under container and gvisor a volume is a path alone: a size is refused
// by the file's keys, and the volume is said to be a docker volume, which
// the container gets as it is created.
func TestVolumeCreateContainer(t *testing.T) {
	e, _ := adminEnv(t, adminConfig, nil)
	if err := e.a.Volume([]string{"create", "web", "/srv/web", "--profile", "container.default"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.readConfig(), "[container.default.volume.web]\npath = \"/srv/web\"\n\n# the links") ||
		!strings.Contains(e.errb.String(), "under container it is docker volume "+e.a.Cfg.Container+"-web, which the container's creation makes if the engine has none, and keeps when the container goes") ||
		!strings.Contains(e.errb.String(), "container.default is not the profile in use (vm.default)") {
		t.Errorf("%s\n%s", e.readConfig(), e.errb)
	}
	before := e.readConfig()
	err := e.a.Volume([]string{"create", "api", "/srv/api", "--profile", "container.default", "--size", "8G"})
	// Refused as config.toml would be refused, not as an edit that
	// could not be made.
	if err == nil || !strings.Contains(err.Error(), "size") || !strings.Contains(err.Error(), "nothing was written") ||
		strings.Contains(err.Error(), "safely") || e.readConfig() != before {
		t.Errorf("a size: %v", err)
	}
}

// What create refuses, as loading config.toml would, writing nothing.
func TestVolumeCreateRefusals(t *testing.T) {
	many := adminConfig
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8", "b9"} {
		many += "\n[vm.default.volume." + n + "]\npath = \"/v/" + n + "\"\n"
	}
	for _, tc := range []struct {
		config string
		args   []string
		want   string
		code   int
	}{
		{adminConfig, []string{"create", "Data", "/data"}, "'Data' is not a volume name", 1},
		{adminConfig, []string{"create", "a.b", "/data"}, "'a.b' is not a volume name", 1},
		{adminConfig, []string{"create", "docker", "/data"}, "dockerd storage", 1},
		{adminConfig, []string{"create", "cache", "/data"}, "[vm.default.volume.cache] is in config.toml already, at /home/agent/.cache/go-build: 'caboose volume resize cache SIZE'", 1},
		{adminConfig, []string{"create", "data", "/data", "--profile", "vm.nope"}, "no isolation profile vm.nope (its profiles: container.default, vm.big, vm.default)", 1},
		{adminConfig, []string{"create", "data", "/work/dev/x"}, "root dev at /work/dev", 1},
		{adminConfig, []string{"create", "data", "/work"}, `"/work" is at, inside or around root dev`, 1},
		{adminConfig, []string{"create", "data", "~/.claude/x"}, "is inside ~/.claude", 1},
		{adminConfig, []string{"create", "data", "~/.cache/go-build/sub"}, "too deep", 1},
		{adminConfig + "\n[vm.default.volume.srv]\npath = \"/srv/a\"\n", []string{"create", "data", "/srv/a/data"}, "volumes srv and data of [vm.default] nest", 1},
		{adminConfig, []string{"create", "data", "~/.cache/go-build"}, "volumes cache and data of [vm.default] are both at", 1},
		{adminConfig, []string{"create", "data", "relative"}, "not an absolute, clean path", 1},
		{adminConfig, []string{"create", "data", "/data", "--size", "2G"}, "outside 4G to 1024G", 1},
		{adminConfig, []string{"create", "data", "/data", "--size", "lots"}, "size", 1},
		{many, []string{"create", "c1", "/v/c1"}, "a VM takes at most 19", 1},
		{"[link]\nhost_exec = true\n", []string{"create", "data", "/data"}, "config.toml defines none in use (isolation container)", 1},
		{adminConfig, []string{"create", "data"}, "usage: caboose volume create NAME PATH", 2},
		{adminConfig, []string{"create", "data", "/data", "--size"}, "--size needs a value", 2},
		{adminConfig, []string{"create", "data", "/data", "--sise", "8G"}, "unknown flag --sise", 2},
		{adminConfig, []string{"create", "data", "/data", "--size", "8G", "--size", "9G"}, "--size given twice", 2},
	} {
		e, _ := adminEnv(t, tc.config, nil)
		before := e.readConfig()
		err := e.a.Volume(tc.args)
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.args, err)
		}
		if e.readConfig() != before {
			t.Errorf("%q: config.toml changed:\n%s", tc.args, e.readConfig())
		}
	}
}

// caboose volume rm comments the table out and keeps the disk, saying
// how to delete it; refused while the running VM has the volume.
func TestVolumeRm(t *testing.T) {
	e, _ := adminEnv(t, adminConfig, nil)
	disk := e.makeDisk("cache", 64<<30)
	if err := e.a.Volume([]string{"rm", "cache"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if got := e.readConfig(); !strings.Contains(got, "# the build cache\n#[vm.default.volume.cache]\n#path = \"~/.cache/go-build\"\n#size = \"64G\"\n\n[vm.big]") {
		t.Errorf("config.toml:\n%s", got)
	}
	if c := e.loaded(); len(c.Volumes) != 0 || len(c.File.Volumes["vm.big"]) != 1 {
		t.Errorf("volumes %+v, file %+v", c.Volumes, c.File.Volumes)
	}
	if !isFile(disk) || !strings.Contains(e.errb.String(), "is kept: vm.big still declares the volume") {
		t.Errorf("disk %v, said %s", isFile(disk), e.errb)
	}
	e.errb.Reset()
	e.reload()
	if err := e.a.Volume([]string{"rm", "cache", "--profile", "vm.big"}); err != nil {
		t.Fatal(err)
	}
	if !isFile(disk) || !strings.Contains(e.errb.String(), "'caboose volume rm cache --delete-disk' deletes it") {
		t.Errorf("disk %v, said %s", isFile(disk), e.errb)
	}

	running := map[string]backend.VolumeState{"cache": {Running: true, Has: true, Live: true}}
	e, _ = adminEnv(t, adminConfig, running)
	before := e.readConfig()
	err := e.a.Volume([]string{"rm", "cache"})
	if err == nil || !strings.Contains(err.Error(), "the running VM has volume cache: 'caboose stop' first") || e.readConfig() != before {
		t.Errorf("running: %v", err)
	}

	// A stopped VM created with it still mounts it at its next start.
	e, _ = adminEnv(t, adminConfig, map[string]backend.VolumeState{"cache": {Has: true}})
	if err := e.a.Volume([]string{"rm", "cache"}); err != nil || !strings.Contains(e.errb.String(), "'caboose restart' recreates it without") ||
		!strings.Contains(e.readConfig(), "#[vm.default.volume.cache]") {
		t.Errorf("stopped: %v\n%s\n%s", err, e.errb, e.readConfig())
	}
}

// What rm refuses, changing nothing.
func TestVolumeRmRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		code int
	}{
		{[]string{"rm", "docker"}, "'caboose prune --docker' empties it", 1},
		{[]string{"rm", "Bad"}, "'Bad' is not a volume name", 1},
		{[]string{"rm", "nope"}, "no profile in config.toml declares a volume nope", 1},
		{[]string{"rm", "cache", "--profile", "container.default"}, "[container.default.volume.cache] is not in config.toml (vm.big, vm.default declare volume cache)", 1},
		{[]string{"rm", "cache", "--delete-disk"}, "vm.big still declares volume cache, on the same disk: remove it there first ('caboose volume rm cache --profile vm.big')", 1},
		{[]string{"rm"}, "usage: caboose volume rm NAME", 2},
		{[]string{"rm", "cache", "--delete-disk=yes"}, "unknown flag --delete-disk=yes", 2},
	} {
		e, _ := adminEnv(t, adminConfig, nil)
		disk := e.makeDisk("cache", 64<<30)
		before := e.readConfig()
		err := e.a.Volume(tc.args)
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.args, err)
		}
		if e.readConfig() != before || !isFile(disk) {
			t.Errorf("%q: changed: disk %v\n%s", tc.args, isFile(disk), e.readConfig())
		}
	}
	// A volume of another profile alone is named, not removed.
	e, _ := adminEnv(t, strings.Replace(adminConfig, "[vm.default.volume.cache]", "[vm.default.volume.other]", 1), nil)
	before := e.readConfig()
	err := e.a.Volume([]string{"rm", "cache"})
	if err == nil || !strings.Contains(err.Error(), "the profile in use (vm.default) has no volume cache; vm.big declares volume cache: name one with --profile") || e.readConfig() != before {
		t.Errorf("another profile's: %v", err)
	}
}

// rm --delete-disk deletes the disk once no vm profile declares the
// volume, after asking: a no, or no terminal, changes nothing;
// CABOOSE_FORCE=1 does not ask. A disk no profile declares goes the same
// way.
func TestVolumeRmDeleteDisk(t *testing.T) {
	one := strings.Replace(adminConfig, "[vm.big.volume.cache]", "[vm.big.volume.other]", 1)
	answer := func(e *setupEnv, s string) {
		e.a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(s)), nil }
	}
	e, _ := adminEnv(t, one, nil)
	disk := e.makeDisk("cache", 64<<30)
	before := e.readConfig()
	answer(e, "n\n")
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err == nil || !strings.Contains(err.Error(), "aborted; nothing was changed") || !isFile(disk) || e.readConfig() != before {
		t.Errorf("no: %v", err)
	}
	e.a.Terminal = func() (io.ReadCloser, error) { return nil, errors.New("no terminal") }
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err == nil || !strings.Contains(err.Error(), "refusing to delete it non-interactively") || !isFile(disk) || e.readConfig() != before {
		t.Errorf("no terminal: %v", err)
	}
	answer(e, "y\n")
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if isFile(disk) || !strings.Contains(e.readConfig(), "#[vm.default.volume.cache]") || !strings.Contains(e.errb.String(), "deleted "+disk) {
		t.Errorf("yes: disk %v\n%s\n%s", isFile(disk), e.readConfig(), e.errb)
	}

	// Left over: no profile declares it.
	e, _ = adminEnv(t, adminConfig, nil)
	disk = e.makeDisk("old", 8<<30)
	err := e.a.Volume([]string{"rm", "old"})
	if err == nil || !strings.Contains(err.Error(), "its disk, "+disk+", is left over ('caboose volume rm old --delete-disk' deletes it)") || !isFile(disk) {
		t.Errorf("left over, no flag: %v", err)
	}
	before = e.readConfig()
	e.a.Cfg.Getenv = func(k string) string {
		return map[string]string{"HOME": e.a.Cfg.Home, "CABOOSE_HOME": e.a.Cfg.CabooseHome, "CABOOSE_FORCE": "1"}[k]
	}
	if err := e.a.Volume([]string{"rm", "old", "--delete-disk"}); err != nil {
		t.Fatal(err)
	}
	if isFile(disk) || e.readConfig() != before || !strings.Contains(e.errb.String(), "CABOOSE_FORCE=1 set") {
		t.Errorf("left over: disk %v\n%s", isFile(disk), e.errb)
	}
	// A disk another process holds, or something not a disk, is not
	// deleted.
	disk = e.makeDisk("held", 8<<30)
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	err = e.a.Volume([]string{"rm", "held", "--delete-disk"})
	f.Close()
	if err == nil || !strings.Contains(err.Error(), "is in use by another process") || !isFile(disk) {
		t.Errorf("held: %v", err)
	}
	if err := os.Mkdir(e.a.volumeDisk("odd"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = e.a.Volume([]string{"rm", "odd", "--delete-disk"})
	if err == nil || !strings.Contains(err.Error(), "is not a disk image") {
		t.Errorf("a directory: %v", err)
	}
	if _, serr := os.Stat(e.a.volumeDisk("odd")); serr != nil {
		t.Errorf("the directory went: %v", serr)
	}
	// Nothing to delete.
	if err := e.a.Volume([]string{"rm", "old", "--delete-disk"}); err != nil || !strings.Contains(e.errb.String(), "volume old has no disk or docker volume here: nothing to delete") {
		t.Errorf("gone: %v\n%s", err, e.errb)
	}
}

// forceEnv sets CABOOSE_FORCE=1 for e's commands.
func forceEnv(e *setupEnv) {
	get := e.a.Cfg.Getenv
	e.a.Cfg.Getenv = func(k string) string {
		if k == "CABOOSE_FORCE" {
			return "1"
		}
		return get(k)
	}
}

// rm finds the volume's table however TOML lets the file write it -- a
// quoted part, dotted keys of its profile, a header-looking line inside a
// multi-line string -- and removes it, with or without its disk. One it
// cannot remove is refused, with the change to make by hand, and its disk
// is kept.
func TestVolumeRmTomlForms(t *testing.T) {
	const head = "isolation = \"vm.default\"\n\n[vm.default]\ncpus = 2\n"
	const link = "\n[link]\nhost_exec = true\n"
	for _, cfg := range []string{
		head + "\n[vm.\"default\".volume.cache]\npath = \"~/.cache/go-build\"\n" + link,
		head + "volume.cache.path = \"~/.cache/go-build\"\n" + link,
		// A path of /x/[link], on three lines.
		head + "\n[vm.default.volume.cache]\npath = \"\"\"\n/x/\\\n[link]\\\n\"\"\"\nsize = \"8G\"\n" + link,
	} {
		for _, del := range []bool{false, true} {
			e, _ := adminEnv(t, cfg, nil)
			forceEnv(e)
			disk := e.makeDisk("cache", 8<<30)
			args := []string{"rm", "cache"}
			if del {
				args = append(args, "--delete-disk")
			}
			if err := e.a.Volume(args); err != nil {
				t.Errorf("%q, %v: %v\n%s", cfg, del, err, e.errb)
				continue
			}
			c := e.loaded()
			if len(c.Volumes) != 0 || !c.File.Has("link.host_exec") || isFile(disk) != !del {
				t.Errorf("%q, %v: volumes %v, disk %v\n%s", cfg, del, c.Volumes, isFile(disk), e.readConfig())
			}
		}
	}
	// An inline table of volumes: no line is the volume's own.
	cfg := head + "volume = { cache = { path = \"~/.cache/go-build\" } }\n" + link
	for _, del := range []bool{false, true} {
		e, _ := adminEnv(t, cfg, nil)
		forceEnv(e)
		disk := e.makeDisk("cache", 8<<30)
		args := []string{"rm", "cache"}
		if del {
			args = append(args, "--delete-disk")
		}
		err := e.a.Volume(args)
		if err == nil || !strings.Contains(err.Error(), "[vm.default.volume.cache] is still there") ||
			!strings.Contains(err.Error(), "# (remove the [vm.default.volume.cache] table)") ||
			strings.Contains(e.errb.String(), "commented out") || e.readConfig() != cfg || !isFile(disk) {
			t.Errorf("inline, %v: %v\n%s\ndisk %v", del, err, e.errb, isFile(disk))
		}
	}
}

// A config.toml the edit cannot read as TOML is refused with the change to
// make by hand, not as a setting config.toml does not take.
func TestVolumeEditBrokenFile(t *testing.T) {
	e, _ := adminEnv(t, adminConfig, nil)
	broken := adminConfig + "\n[link\n"
	if err := os.WriteFile(e.configPath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	err := e.a.Volume([]string{"create", "data", "/data"})
	if err == nil || !strings.Contains(err.Error(), "cannot edit") || !strings.Contains(err.Error(), "[vm.default.volume.data]\n") || e.readConfig() != broken {
		t.Errorf("%v", err)
	}
}

// rm --delete-disk holds the VM's start lock from before it looks at the
// VM and the disk until the disk is deleted, so that no start opens the
// disk in between; the disk is checked again after the question.
func TestVolumeRmHoldsStartLock(t *testing.T) {
	one := strings.Replace(adminConfig, "[vm.big.volume.cache]", "[vm.big.volume.other]", 1)
	e, _ := adminEnv(t, one, nil)
	disk := e.makeDisk("cache", 8<<30)
	var events []string
	hold := holdVMStart
	t.Cleanup(func() { holdVMStart = hold })
	holdVMStart = func(*App) (func(), error) {
		events = append(events, "lock")
		if !isFile(disk) || !strings.Contains(e.readConfig(), "\n[vm.default.volume.cache]") {
			t.Error("locked after a change")
		}
		return func() {
			events = append(events, "unlock")
			if isFile(disk) || strings.Contains(e.readConfig(), "\n[vm.default.volume.cache]") {
				t.Errorf("unlocked before the change: disk %v\n%s", isFile(disk), e.readConfig())
			}
		}, nil
	}
	e.a.Terminal = func() (io.ReadCloser, error) {
		events = append(events, "ask")
		return io.NopCloser(strings.NewReader("y\n")), nil
	}
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if strings.Join(events, ",") != "lock,ask,unlock" {
		t.Errorf("events %v", events)
	}

	// No lock without --delete-disk.
	e, _ = adminEnv(t, one, nil)
	events = nil
	if err := e.a.Volume([]string{"rm", "cache"}); err != nil || len(events) != 0 {
		t.Errorf("%v, events %v", err, events)
	}

	// A disk taken while the question waited is kept, and nothing changes.
	e, _ = adminEnv(t, one, nil)
	disk = e.makeDisk("cache", 8<<30)
	holdVMStart = func(*App) (func(), error) { return func() {}, nil }
	before := e.readConfig()
	var held *os.File
	t.Cleanup(func() {
		if held != nil {
			held.Close()
		}
	})
	e.a.Terminal = func() (io.ReadCloser, error) {
		f, err := os.OpenFile(disk, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		held = f
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader("y\n")), nil
	}
	err := e.a.Volume([]string{"rm", "cache", "--delete-disk"})
	if err == nil || !strings.Contains(err.Error(), "is in use by another process") || !strings.Contains(err.Error(), "nothing was changed") ||
		!isFile(disk) || e.readConfig() != before {
		t.Errorf("taken: %v, disk %v\n%s", err, isFile(disk), e.readConfig())
	}
}

// Only the profile in use is the VM's: another profile's volume, or one
// of a container profile in use, is removed while the VM runs with a
// volume of that name, with no word of the VM; but not with its disk.
func TestVolumeRmOtherProfile(t *testing.T) {
	running := map[string]backend.VolumeState{"cache": {Running: true, Has: true, Live: true}}
	e, _ := adminEnv(t, adminConfig, running)
	if err := e.a.Volume([]string{"rm", "cache", "--profile", "vm.big"}); err != nil || strings.Contains(e.errb.String(), "the VM") {
		t.Errorf("vm.big: %v\n%s", err, e.errb)
	}
	if c := e.loaded(); len(c.File.Volumes["vm.big"]) != 0 || len(c.Volumes) != 1 {
		t.Errorf("volumes %+v", c.File.Volumes)
	}
	cfg := strings.Replace(adminConfig, `isolation = "vm.default"`, `isolation = "container.default"`, 1) +
		"\n[container.default.volume.cache]\npath = \"/c\"\n"
	e, _ = adminEnv(t, cfg, map[string]backend.VolumeState{"cache": {Has: true}})
	if err := e.a.Volume([]string{"rm", "cache"}); err != nil || strings.Contains(e.errb.String(), "the VM") {
		t.Errorf("container.default: %v\n%s", err, e.errb)
	}
	if c := e.loaded(); len(c.File.Volumes["container.default"]) != 0 {
		t.Errorf("volumes %+v", c.File.Volumes)
	}
	// Its disk is the VM's whichever profile declares it.
	only := strings.Replace(adminConfig, "[vm.default.volume.cache]", "[vm.default.volume.other]", 1)
	e, _ = adminEnv(t, only, running)
	forceEnv(e)
	disk := e.makeDisk("cache", 8<<30)
	before := e.readConfig()
	err := e.a.Volume([]string{"rm", "cache", "--profile", "vm.big", "--delete-disk"})
	if err == nil || !strings.Contains(err.Error(), "the running VM has volume cache") || !isFile(disk) || e.readConfig() != before {
		t.Errorf("--delete-disk: %v", err)
	}
}

// A flag's value may not be empty.
func TestVolumeEmptyFlagValue(t *testing.T) {
	for _, args := range [][]string{
		{"create", "data", "/data", "--profile="},
		{"create", "data", "/data", "--profile", ""},
		{"create", "data", "/data", "--size="},
		{"rm", "cache", "--profile="},
	} {
		e, _ := adminEnv(t, adminConfig, nil)
		before := e.readConfig()
		err := e.a.Volume(args)
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 2 || !strings.Contains(err.Error(), "needs a value") || e.readConfig() != before {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// rm --delete-disk never follows a symbolic link at the disk's place.
func TestVolumeRmSymlink(t *testing.T) {
	e, _ := adminEnv(t, adminConfig, nil)
	forceEnv(e)
	target := filepath.Join(t.TempDir(), "mine.img")
	if err := os.WriteFile(target, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.a.volumesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.a.volumeDisk("link")); err != nil {
		t.Fatal(err)
	}
	err := e.a.Volume([]string{"rm", "link", "--delete-disk"})
	if err == nil || !strings.Contains(err.Error(), "is not a disk image") || !isFile(target) {
		t.Errorf("%v, target %v", err, isFile(target))
	}
	if _, err := os.Lstat(e.a.volumeDisk("link")); err != nil {
		t.Errorf("the link went: %v", err)
	}
}

// diskAllocated is the blocks a file takes up, a sparse one's holes not
// counted.
func TestDiskAllocated(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full")
	if err := os.WriteFile(full, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(dir, "sparse")
	if err := os.WriteFile(sparse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sparse, 1<<30); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if n := diskAllocated(fi); n < 1<<20 || n > 1<<20+1<<16 {
		t.Errorf("full: %d", n)
	}
	if fi, err = os.Stat(sparse); err != nil {
		t.Fatal(err)
	}
	if n := diskAllocated(fi); n >= 1<<20 {
		t.Errorf("sparse: %d", n)
	}
}

// With no sandbox of the tests', and an isolation that is not vm, the
// environment's VM is still the one in the data dir: a VM left from when
// config.toml picked vm.
func TestEnvVMWithoutVMIsolation(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig(strings.Replace(adminConfig, `isolation = "vm.default"`, `isolation = "container.default"`, 1))
	if err := e.a.vmDir(e.a.Cfg.Container).WriteState(vm.State{Volumes: []string{"cache"}}); err != nil {
		t.Fatal(err)
	}
	vs, err := e.a.vmVolumeState("cache")
	if err != nil || !vs.Has {
		t.Errorf("%+v, %v", vs, err)
	}
}

// ctrConfig has cache in the container profile in use and in a gvisor
// one, which share its docker volume.
const ctrConfig = `isolation = "container.default"

[container.default]

[container.default.volume.cache]
path = "~/.cache/go-build"

[gvisor.default]

[gvisor.default.volume.cache]
path = "~/.cache/go-build"
`

// ctrOnlyConfig has cache in the container profile in use alone.
var ctrOnlyConfig = strings.Replace(ctrConfig, "[gvisor.default.volume.cache]", "[gvisor.default.volume.other]", 1)

// cacheLabel is the volumes label of a container created with cache.
var cacheLabel = map[string]string{assets.LabelVolumes: `{"cache":"/home/agent/.cache/go-build"}`}

// Under the container profile in use, create says what the container's
// next creation, or a restart, does with the volume.
func TestVolumeCreateContainerInUse(t *testing.T) {
	e, _ := adminEnv(t, ctrConfig, nil)
	if err := e.a.Volume([]string{"create", "web", "/srv/web"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.errb.String(), "the container's creation, at the next start, mounts it at /srv/web") {
		t.Errorf("absent:\n%s", e.errb)
	}
	e, b := adminEnv(t, ctrConfig, nil)
	b.Status = "running"
	if err := e.a.Volume([]string{"create", "web", "/srv/web"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.errb.String(), "the container was created without it: 'caboose restart' recreates it with the volume") {
		t.Errorf("running:\n%s", e.errb)
	}
}

// Under container, rm keeps the docker volume and says so; --delete-disk
// deletes it, asking, once no other container or gvisor profile declares
// it and the container no longer has it.
func TestVolumeRmContainer(t *testing.T) {
	e, b := adminEnv(t, ctrOnlyConfig, nil)
	vol := e.a.Cfg.Container + "-cache"
	b.Status, b.SandboxLabels = "running", cacheLabel
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: vol, Size: "1.2GB"}}

	// The container has it: refused, naming the way out.
	before := e.readConfig()
	err := e.a.Volume([]string{"rm", "cache", "--delete-disk"})
	if err == nil || !strings.Contains(err.Error(), "the container was created with volume cache, and holds its docker volume: 'caboose volume rm cache', then 'caboose restart'") ||
		e.readConfig() != before || len(b.RemovedVolumes) != 0 {
		t.Errorf("held: %v", err)
	}
	// Without the flag: commented out, the docker volume kept.
	if err := e.a.Volume([]string{"rm", "cache"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.readConfig(), "#[container.default.volume.cache]") || len(b.RemovedVolumes) != 0 ||
		!strings.Contains(e.errb.String(), "the container, created with volume cache, keeps it until 'caboose restart'") ||
		!strings.Contains(e.errb.String(), "its docker volume, "+vol+", is kept, with what it holds") {
		t.Errorf("rm:\n%s\n%s", e.readConfig(), e.errb)
	}

	// gvisor.default still declares it, on the same docker volume: kept,
	// said, and the entry commented out all the same.
	e, b = adminEnv(t, ctrConfig, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: vol, Size: "1.2GB"}}
	forceEnv(e)
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("declared elsewhere: %v\n%s", err, e.errb)
	}
	if len(b.RemovedVolumes) != 0 || !strings.Contains(e.readConfig(), "#[container.default.volume.cache]") ||
		!strings.Contains(e.errb.String(), "its docker volume, "+vol+", is kept: gvisor.default still declares the volume") ||
		strings.Contains(e.errb.String(), "nothing to delete") {
		t.Errorf("declared elsewhere: removed %q\n%s\n%s", b.RemovedVolumes, e.readConfig(), e.errb)
	}

	// Asked, and refused without a yes.
	e, b = adminEnv(t, ctrOnlyConfig, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: vol, Size: "1.2GB"}}
	before = e.readConfig()
	answerTerm(e, "n\n")
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err == nil || !strings.Contains(err.Error(), "aborted; nothing was changed") ||
		e.readConfig() != before || len(b.RemovedVolumes) != 0 {
		t.Errorf("no: %v", err)
	}
	if !strings.Contains(e.errb.String(), "this deletes docker volume "+vol+" (1.2GB): everything volume cache holds") {
		t.Errorf("not said:\n%s", e.errb)
	}
	e.a.Terminal = func() (io.ReadCloser, error) { return nil, errors.New("no terminal") }
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err == nil || !strings.Contains(err.Error(), "refusing to delete it non-interactively") || len(b.RemovedVolumes) != 0 {
		t.Errorf("no terminal: %v", err)
	}
	answerTerm(e, "y\n")
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if !slices.Equal(b.RemovedVolumes, []string{"cache"}) || !strings.Contains(e.readConfig(), "#[container.default.volume.cache]") ||
		!strings.Contains(e.errb.String(), "deleted docker volume "+vol) {
		t.Errorf("yes: removed %q\n%s\n%s", b.RemovedVolumes, e.readConfig(), e.errb)
	}
}

// Under container, a docker volume no profile declares is left over: rm
// names it, --delete-disk deletes it; the engine refusing is said.
func TestVolumeRmContainerLeftOver(t *testing.T) {
	e, b := adminEnv(t, ctrConfig, nil)
	vol := e.a.Cfg.Container + "-old"
	b.EngineVols = []backend.EngineVolume{{Name: "old", Engine: vol}}
	err := e.a.Volume([]string{"rm", "old"})
	if err == nil || !strings.Contains(err.Error(), "its docker volume, "+vol+", is left over ('caboose volume rm old --delete-disk' deletes it)") {
		t.Errorf("no flag: %v", err)
	}
	forceEnv(e)
	b.RemoveVolumeErr = errors.New("volume is in use")
	if err := e.a.Volume([]string{"rm", "old", "--delete-disk"}); err == nil || !strings.Contains(err.Error(), "cannot delete docker volume "+vol+": volume is in use") {
		t.Errorf("refused: %v", err)
	}
	b.RemoveVolumeErr = nil
	if err := e.a.Volume([]string{"rm", "old", "--delete-disk"}); err != nil {
		t.Fatal(err)
	}
	if len(b.EngineVols) != 0 || !strings.Contains(e.errb.String(), "this deletes docker volume "+vol+" (size unknown)") {
		t.Errorf("left %+v\n%s", b.EngineVols, e.errb)
	}
	// Under vm, docker volumes are not caboose's to name or delete.
	v, vb := adminEnv(t, adminConfig, nil)
	vb.EngineVols = []backend.EngineVolume{{Name: "old", Engine: "x"}}
	if err := v.a.Volume([]string{"rm", "old"}); err == nil || strings.Contains(err.Error(), "docker volume") {
		t.Errorf("vm, no flag: %v", err)
	}
	forceEnv(v)
	if err := v.a.Volume([]string{"rm", "old", "--delete-disk"}); err != nil || len(vb.RemovedVolumes) != 0 {
		t.Errorf("vm: %v, removed %q", err, vb.RemovedVolumes)
	}
}

// Under container, --delete-disk leaves a vm disk of the name that a vm
// profile still declares, and deletes one no vm profile declares, with
// the docker volume, under one question.
func TestVolumeRmContainerVMDisk(t *testing.T) {
	cfg := ctrOnlyConfig + `
[vm.default]

[vm.default.volume.cache]
path = "~/.cache/go-build"
size = "8G"
`
	e, b := adminEnv(t, cfg, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: e.a.Cfg.Container + "-cache"}}
	disk := e.makeDisk("cache", 8<<30)
	forceEnv(e)
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if !isFile(disk) || !slices.Equal(b.RemovedVolumes, []string{"cache"}) ||
		!strings.Contains(e.errb.String(), "its vm disk, "+disk+", is kept: vm.default still declares the volume") {
		t.Errorf("kept: disk %v, removed %q\n%s", isFile(disk), b.RemovedVolumes, e.errb)
	}

	e, b = adminEnv(t, ctrOnlyConfig, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: e.a.Cfg.Container + "-cache"}}
	disk = e.makeDisk("cache", 8<<30)
	answerTerm(e, "y\n")
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if isFile(disk) || !slices.Equal(b.RemovedVolumes, []string{"cache"}) || !strings.Contains(e.errb.String(), "delete both?") {
		t.Errorf("both: disk %v, removed %q\n%s", isFile(disk), b.RemovedVolumes, e.errb)
	}
}

// Under container, what a restore left beside a vm disk goes with the
// disk, and stays with one a vm profile still declares.
func TestVolumeRmContainerRestoreLeft(t *testing.T) {
	cfg := ctrOnlyConfig + `
[vm.default]

[vm.default.volume.cache]
path = "~/.cache/go-build"
size = "8G"
`
	e, b := adminEnv(t, cfg, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: e.a.Cfg.Container + "-cache"}}
	disk := e.makeDisk("cache", 8<<30)
	before := disk + checkpoint.BeforeRestoreSuffix
	if err := os.WriteFile(before, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	forceEnv(e)
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("kept: %v\n%s", err, e.errb)
	}
	if !isFile(disk) || !isFile(before) || !slices.Equal(b.RemovedVolumes, []string{"cache"}) {
		t.Errorf("kept: disk %v, before-restore %v, removed %q\n%s", isFile(disk), isFile(before), b.RemovedVolumes, e.errb)
	}

	e, b = adminEnv(t, ctrOnlyConfig, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: e.a.Cfg.Container + "-cache"}}
	disk = e.makeDisk("cache", 8<<30)
	before = disk + checkpoint.BeforeRestoreSuffix
	if err := os.WriteFile(before, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	forceEnv(e)
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil {
		t.Fatalf("deleted: %v\n%s", err, e.errb)
	}
	if isFile(disk) || isFile(before) || !slices.Equal(b.RemovedVolumes, []string{"cache"}) || !strings.Contains(e.errb.String(), "deleted "+before) {
		t.Errorf("deleted: disk %v, before-restore %v, removed %q\n%s", isFile(disk), isFile(before), b.RemovedVolumes, e.errb)
	}
}

// Under container, a vm profile's volume removed with --delete-disk
// deletes its disk and keeps the docker volume, which the container
// profile in use declares, and the container has; and a container
// profile's, while another container or gvisor profile declares it,
// keeps the docker volume and deletes the disk no vm profile declares.
func TestVolumeRmContainerKeepsDockerVolume(t *testing.T) {
	cfg := ctrOnlyConfig + `
[vm.default]

[vm.default.volume.cache]
path = "~/.cache/go-build"
size = "8G"
`
	e, b := adminEnv(t, cfg, nil)
	vol := e.a.Cfg.Container + "-cache"
	b.Status, b.SandboxLabels = "running", cacheLabel
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: vol}}
	disk := e.makeDisk("cache", 8<<30)
	forceEnv(e)
	if err := e.a.Volume([]string{"rm", "cache", "--profile", "vm.default", "--delete-disk"}); err != nil {
		t.Fatalf("vm profile: %v\n%s", err, e.errb)
	}
	if isFile(disk) || len(b.RemovedVolumes) != 0 || !strings.Contains(e.readConfig(), "#[vm.default.volume.cache]") ||
		strings.Contains(e.readConfig(), "#[container.default.volume.cache]") ||
		!strings.Contains(e.errb.String(), "its docker volume, "+vol+", is kept: vm.default is a vm profile, whose volume is a disk, not it") {
		t.Errorf("vm profile: disk %v, removed %q\n%s\n%s", isFile(disk), b.RemovedVolumes, e.readConfig(), e.errb)
	}

	e, b = adminEnv(t, ctrConfig, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: vol}}
	disk = e.makeDisk("cache", 8<<30)
	forceEnv(e)
	if err := e.a.Volume([]string{"rm", "cache", "--profile", "gvisor.default", "--delete-disk"}); err != nil {
		t.Fatalf("other container profile: %v\n%s", err, e.errb)
	}
	if isFile(disk) || len(b.RemovedVolumes) != 0 || !strings.Contains(e.readConfig(), "#[gvisor.default.volume.cache]") ||
		!strings.Contains(e.errb.String(), "its docker volume, "+vol+", is kept: container.default still declares the volume") ||
		!strings.Contains(e.errb.String(), "deleted "+disk) {
		t.Errorf("other container profile: disk %v, removed %q\n%s\n%s", isFile(disk), b.RemovedVolumes, e.readConfig(), e.errb)
	}
}

// With "delete both?" answered yes, a docker volume that will not go
// leaves the disk deleted all the same, and the error says what went,
// what did not, that config.toml is edited already, and what finishes
// the job; a disk that will not go is said the same way.
func TestVolumeRmContainerPartialFailure(t *testing.T) {
	e, b := adminEnv(t, ctrOnlyConfig, nil)
	vol := e.a.Cfg.Container + "-cache"
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: vol}}
	b.RemoveVolumeErr = errors.New("volume is in use")
	disk := e.makeDisk("cache", 8<<30)
	answerTerm(e, "y\n")
	err := e.a.Volume([]string{"rm", "cache", "--delete-disk"})
	want := "cannot delete docker volume " + vol + ": volume is in use; [container.default.volume.cache] is commented out in config.toml already: once that is fixed, 'caboose volume rm cache --delete-disk' finishes the job"
	if err == nil || err.Error() != want || isFile(disk) || !strings.Contains(e.errb.String(), "deleted "+disk) ||
		!strings.Contains(e.readConfig(), "#[container.default.volume.cache]") {
		t.Errorf("docker volume:\n got %v\nwant %s\ndisk %v\n%s", err, want, isFile(disk), e.errb)
	}
	// The finishing run: nothing declares it now.
	b.RemoveVolumeErr = nil
	forceEnv(e)
	if err := e.a.Volume([]string{"rm", "cache", "--delete-disk"}); err != nil || len(b.EngineVols) != 0 {
		t.Errorf("again: %v, left %+v\n%s", err, b.EngineVols, e.errb)
	}

	// A disk that will not go, the docker volume deleted.
	e, b = adminEnv(t, ctrOnlyConfig, nil)
	b.EngineVols = []backend.EngineVolume{{Name: "cache", Engine: vol}}
	disk = e.makeDisk("cache", 8<<30)
	forceEnv(e)
	orig := removeDisk
	removeDisk = func(string) error { return errors.New("permission denied") }
	defer func() { removeDisk = orig }()
	err = e.a.Volume([]string{"rm", "cache", "--delete-disk"})
	want = "cannot delete " + disk + ": permission denied; [container.default.volume.cache] is commented out in config.toml already: once that is fixed"
	if err == nil || !strings.HasPrefix(err.Error(), want) || !isFile(disk) || !strings.Contains(e.errb.String(), "deleted docker volume "+vol) {
		t.Errorf("disk:\n got %v\nwant %s\n%s", err, want, e.errb)
	}
}

// answerTerm gives e's commands a terminal that answers s.
func answerTerm(e *setupEnv, s string) {
	e.a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(s)), nil }
}
