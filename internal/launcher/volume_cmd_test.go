package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// resizerBox is a sandbox whose volumes resize as the test says.
type resizerBox struct {
	*backendtest.Fake
	state   backend.VolumeState
	res     backend.VolumeResize
	err     error
	resized []int64
	// during, when set, runs inside the resize, with its context.
	during func(ctx context.Context) error
}

func (b *resizerBox) VolumeState(name string) (backend.VolumeState, error) {
	if name != "cache" {
		return backend.VolumeState{Running: b.state.Running}, nil
	}
	return b.state, nil
}

func (b *resizerBox) ResizeVolume(ctx context.Context, name string, size int64, say func(string, ...any)) (backend.VolumeResize, error) {
	b.resized = append(b.resized, size)
	say("resizing %s", name)
	if b.during != nil {
		return b.res, b.during(ctx)
	}
	return b.res, b.err
}

const volumeConfig = `isolation = "vm.default"

[vm.default]
cpus = 2

# the build cache
[vm.default.volume.cache]
path = "~/.cache/go-build"
size = "64G"
`

// volumeEnv is an environment under vm whose profile has volume cache,
// 64G, its disk already made at size.
func volumeEnv(t *testing.T, state backend.VolumeState, disk int64) (*setupEnv, *resizerBox) {
	t.Helper()
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig(volumeConfig)
	cfg, err := config.Load(e.a.Cfg.Getenv, config.OSFS{}, "default")
	if err != nil {
		t.Fatal(err)
	}
	e.a.Cfg.Volumes = cfg.Volumes
	free := vmFreeSpace
	vmFreeSpace = func(string) (int64, error) { return 1 << 50, nil }
	t.Cleanup(func() { vmFreeSpace = free })
	b := &resizerBox{Fake: &backendtest.Fake{}, state: state}
	e.a.Backend = b
	if disk > 0 {
		p := e.a.volumeDisk("cache")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, disk); err != nil {
			t.Fatal(err)
		}
	}
	return e, b
}

// configSize is what config.toml now gives volume cache.
func (e *setupEnv) configSize() string {
	e.t.Helper()
	b, err := os.ReadFile(e.configPath())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "size = ") {
			return strings.Trim(strings.TrimPrefix(l, "size = "), `"`)
		}
	}
	return ""
}

var liveVM = backend.VolumeState{Running: true, Has: true, Live: true}

// While the VM runs with the volume live, the resize happens at once and
// its new size is written once the disk has it, the file's comments
// kept.
func TestVolumeResizeLive(t *testing.T) {
	e, b := volumeEnv(t, liveVM, 64<<30)
	b.res = backend.VolumeResize{From: 64 << 30, Applied: true}
	if err := e.a.Volume([]string{"resize", "cache", "96G"}); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if len(b.resized) != 1 || b.resized[0] != 96<<30 {
		t.Errorf("resized %v", b.resized)
	}
	if got := e.configSize(); got != "96G" {
		t.Errorf("config.toml says %q", got)
	}
	if data, _ := os.ReadFile(e.configPath()); !strings.Contains(string(data), "# the build cache") {
		t.Errorf("the comment went:\n%s", data)
	}
	for _, w := range []string{"resizing cache", `config.toml: [vm.default.volume.cache] size = "96G"`} {
		if !strings.Contains(e.errb.String(), w) {
			t.Errorf("said %q, not %q", e.errb, w)
		}
	}
}

// A resize applied with an error (a grown disk whose filesystem lags) is
// recorded and fails; one not applied is not recorded.
func TestVolumeResizeLiveFails(t *testing.T) {
	e, b := volumeEnv(t, liveVM, 64<<30)
	b.res, b.err = backend.VolumeResize{Applied: true}, errors.New("its filesystem did not grow")
	err := e.a.Volume([]string{"resize", "cache", "96G"})
	if err == nil || !strings.Contains(err.Error(), "did not grow") || e.configSize() != "96G" {
		t.Errorf("applied: %v, config %q", err, e.configSize())
	}
	e, b = volumeEnv(t, liveVM, 64<<30)
	b.err = errors.New("is in use")
	err = e.a.Volume([]string{"resize", "cache", "32G"})
	if err == nil || !strings.Contains(err.Error(), "is in use") || e.configSize() != "64G" {
		t.Errorf("not applied: %v, config %q", err, e.configSize())
	}
}

// A stopped VM's volume grows at the next start: config.toml alone
// changes. A shrink needs the VM running, and changes nothing.
func TestVolumeResizeStopped(t *testing.T) {
	e, b := volumeEnv(t, backend.VolumeState{Has: true, Live: true}, 64<<30)
	if err := e.a.Volume([]string{"resize", "cache", "100G"}); err != nil {
		t.Fatal(err)
	}
	if e.configSize() != "100G" || len(b.resized) != 0 || !strings.Contains(e.errb.String(), "grows to 100G at its next start") {
		t.Errorf("config %q, resized %v, said %s", e.configSize(), b.resized, e.errb)
	}
	e, b = volumeEnv(t, backend.VolumeState{Has: true, Live: true}, 64<<30)
	err := e.a.Volume([]string{"resize", "cache", "32G"})
	if err == nil || !strings.Contains(err.Error(), "needs the VM running") || e.configSize() != "64G" || len(b.resized) != 0 {
		t.Errorf("shrink: %v, config %q", err, e.configSize())
	}
	// No disk yet: the size is the config's, and made at it.
	e, _ = volumeEnv(t, backend.VolumeState{}, 0)
	if err := e.a.Volume([]string{"resize", "cache", "8G"}); err != nil || e.configSize() != "8G" {
		t.Errorf("no disk: %v, %q", err, e.configSize())
	}
}

// A running VM whose volume is on the framework's disk grows it at the
// next start, and refuses a shrink, naming macOS 27; one created without
// the volume gets it at a restart.
func TestVolumeResizeFrameworkDisk(t *testing.T) {
	e, b := volumeEnv(t, backend.VolumeState{Running: true, Has: true}, 64<<30)
	if err := e.a.Volume([]string{"resize", "cache", "96G"}); err != nil {
		t.Fatal(err)
	}
	if e.configSize() != "96G" || len(b.resized) != 0 || !strings.Contains(e.errb.String(), "macOS 27") || !strings.Contains(e.errb.String(), "grows at the VM's next start") {
		t.Errorf("grow: config %q, said %s", e.configSize(), e.errb)
	}
	e, b = volumeEnv(t, backend.VolumeState{Running: true, Has: true}, 64<<30)
	err := e.a.Volume([]string{"resize", "cache", "32G"})
	if err == nil || !strings.Contains(err.Error(), "macOS 27") || !strings.Contains(err.Error(), "caboose restart") || e.configSize() != "64G" || len(b.resized) != 0 {
		t.Errorf("shrink: %v, config %q", err, e.configSize())
	}
	e, _ = volumeEnv(t, backend.VolumeState{Running: true}, 0)
	if err := e.a.Volume([]string{"resize", "cache", "96G"}); err != nil || !strings.Contains(e.errb.String(), "created without volume cache") {
		t.Errorf("not in the VM: %v, said %s", err, e.errb)
	}
}

// Every resize that writes config.toml tells sessions the new size
// (state.toml): live and applied, a stopped VM's, one on the framework's
// disk; a live one not applied tells them nothing.
func TestVolumeResizeExports(t *testing.T) {
	stateSays := func(e *setupEnv, size string) bool {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
		if err != nil {
			return false
		}
		return strings.Contains(string(b), `size = "`+size+`"`)
	}
	e, b := volumeEnv(t, liveVM, 64<<30)
	b.err = errors.New("is in use")
	err := e.a.Volume([]string{"resize", "cache", "32G"})
	if _, serr := os.Stat(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml")); err == nil || !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("not applied: %v, exported (%v)", err, serr)
	}
	b.res, b.err = backend.VolumeResize{From: 64 << 30, Applied: true}, nil
	if err := e.a.Volume([]string{"resize", "cache", "96G"}); err != nil || !stateSays(e, "96G") {
		t.Errorf("live: %v, exported %v\n%s", err, stateSays(e, "96G"), e.errb)
	}
	e, _ = volumeEnv(t, backend.VolumeState{Has: true, Live: true}, 64<<30)
	if err := e.a.Volume([]string{"resize", "cache", "100G"}); err != nil || !stateSays(e, "100G") {
		t.Errorf("stopped: %v, exported %v\n%s", err, stateSays(e, "100G"), e.errb)
	}
	e, _ = volumeEnv(t, backend.VolumeState{Running: true, Has: true}, 64<<30)
	if err := e.a.Volume([]string{"resize", "cache", "112G"}); err != nil || !stateSays(e, "112G") {
		t.Errorf("framework's disk: %v, exported %v\n%s", err, stateSays(e, "112G"), e.errb)
	}
}

// What the command refuses before touching anything: another isolation,
// dockerd's disk, a volume the profile lacks, a size config.toml would
// refuse, a usage it does not know.
func TestVolumeResizeRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		code int
	}{
		{[]string{"resize", "docker", "96G"}, "docker_size", 1},
		{[]string{"resize", "data", "96G"}, "profile vm.default has no volume data (its volumes: cache)", 1},
		{[]string{"resize", "cache", "2G"}, "outside 4G to 1024G", 1},
		{[]string{"resize", "cache", "2T"}, "size:", 1},
		{[]string{"resize", "cache"}, "usage: caboose volume resize NAME SIZE", 2},
		{[]string{"grow"}, "usage: caboose volume create NAME PATH", 2},
		{nil, "| resize NAME SIZE (got nothing)", 2},
	} {
		e, b := volumeEnv(t, liveVM, 64<<30)
		err := e.a.Volume(tc.args)
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.args, err)
		}
		if len(b.resized) != 0 || e.configSize() != "64G" {
			t.Errorf("%q: resized %v, config %q", tc.args, b.resized, e.configSize())
		}
	}
	e, b := volumeEnv(t, liveVM, 64<<30)
	e.a.Cfg.Isolation = isolationContainer
	err := e.a.Volume([]string{"resize", "cache", "96G"})
	if err == nil || !strings.Contains(err.Error(), "under container a volume has no size") || len(b.resized) != 0 {
		t.Errorf("container: %v", err)
	}
}

// A config.toml that cannot be edited stops the resize before the VM is
// touched.
func TestVolumeResizeConfigUneditable(t *testing.T) {
	e, b := volumeEnv(t, liveVM, 64<<30)
	if err := os.Remove(e.configPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.configPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	err := e.a.Volume([]string{"resize", "cache", "96G"})
	if err == nil || !strings.Contains(err.Error(), "reading") || len(b.resized) != 0 {
		t.Errorf("%v, resized %v", err, b.resized)
	}
}

func TestSizeValue(t *testing.T) {
	for n, want := range map[int64]string{64 << 30: "64G", 4 << 30: "4G", 4608 << 20: "4608M"} {
		if got := sizeValue(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}

// An interrupt during a live resize is the resize's to handle: its
// context ends, the launcher lives on to record what was applied, and
// exits as interrupted.
func TestVolumeResizeInterrupted(t *testing.T) {
	e, b := volumeEnv(t, liveVM, 64<<30)
	b.res = backend.VolumeResize{From: 64 << 30}
	b.during = func(ctx context.Context) error {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.New("interrupted before anything was shrunk; it is mounted again")
		case <-time.After(5 * time.Second):
			return errors.New("the context never ended")
		}
	}
	err := e.a.Volume([]string{"resize", "cache", "32G"})
	var x *ExitError
	if !errors.As(err, &x) || x.Code != 130 || !strings.Contains(x.Msg, "interrupted before") {
		t.Fatalf("%v", err)
	}
	if got := e.configSize(); got != "64G" {
		t.Errorf("config.toml says %q", got)
	}
}
