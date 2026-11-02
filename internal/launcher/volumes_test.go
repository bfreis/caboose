package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/vm"
)

// A VM is created with dockerd's disk and the profile's volumes, each on
// caboose-vmm's device on macOS 27 and on the framework's before (said
// as it is created), and labelled with them; a container with the
// profile's volumes alone, as docker volumes, given to the agent user,
// labelled with them, with nothing said and macOS never asked.
func TestCreateVolumes(t *testing.T) {
	vols := []config.Volume{{Name: "cache", Path: "/home/agent/.cache/go-build", Size: 8 << 30}, {Name: "data", Path: "/data", Size: 64 << 30}}
	for _, tc := range []struct {
		iso, macOS string
		live       bool
		said       string
	}{
		{isolationVM, "27.0.1", true, ""},
		{isolationVM, "27", true, ""},
		{isolationVM, "26.4", false, "cache, data go on Virtualization.framework's own disks, as this Mac runs macOS 26.4: caboose-vmm serves volumes on a disk of its own, whose size can change while the VM runs, from macOS 27"},
		{isolationVM, "13.0", false, "as this Mac runs macOS 13.0"},
		{isolationVM, "", false, "as caboose-vmm did not say which macOS this Mac runs"},
		{isolationContainer, "27.0", false, ""},
		{isolationGVisor, "27.0", false, ""},
	} {
		t.Run(tc.iso+" "+tc.macOS, func(t *testing.T) {
			b := creating(t, tc.iso, currentLabels("linux-x64"))
			b.Cfg.Volumes = vols
			b.Cfg.Profile = tc.iso + ".a"
			asked := 0
			b.macOS = func() string { asked++; return tc.macOS }
			if err := b.createContainer(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			if tc.said == "" && strings.Contains(b.errb.String(), "volume") || !strings.Contains(b.errb.String(), tc.said) {
				t.Errorf("said:\n%s", b.errb)
			}
			spec, _ := b.box.Spec()
			if tc.iso != isolationVM {
				want := []backend.Volume{{Name: "cache", Target: "/home/agent/.cache/go-build"}, {Name: "data", Target: "/data"}}
				if !reflect.DeepEqual(spec.Volumes, want) || !reflect.DeepEqual(b.sandboxVolumes(), want) || asked != 0 {
					t.Errorf("volumes %+v, sandbox volumes %+v, macOS asked %d times", spec.Volumes, b.sandboxVolumes(), asked)
				}
				if !hasSeq(spec.Labels, assets.LabelVolumes+`={"cache":"/home/agent/.cache/go-build","data":"/data"}`) {
					t.Errorf("labels %q", spec.Labels)
				}
				if owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()); spec.VolumeOwner != owner || spec.User != "" {
					t.Errorf("volume owner %q, user %q; want %q, the agent", spec.VolumeOwner, spec.User, owner)
				}
				return
			}
			if spec.VolumeOwner != "" {
				t.Errorf("a VM's volume owner %q", spec.VolumeOwner)
			}
			want := []backend.Volume{{Name: "docker", Target: "/var/lib/docker"},
				{Name: "cache", Target: "/home/agent/.cache/go-build", Live: tc.live}, {Name: "data", Target: "/data", Live: tc.live}}
			if !reflect.DeepEqual(spec.Volumes, want) {
				t.Errorf("volumes %+v", spec.Volumes)
			}
			if !hasSeq(spec.Labels, assets.LabelVolumes+`={"cache":"/home/agent/.cache/go-build","data":"/data","docker":"/var/lib/docker"}`) {
				t.Errorf("labels %q", spec.Labels)
			}
		})
	}
}

// The sandbox's label against the profile's volumes, under every
// isolation: another set, or another path, is drift that a restart
// fixes; a sandbox with no label is not this caboose's.
func TestVolumesDrift(t *testing.T) {
	cache := config.Volume{Name: "cache", Path: "/c", Size: 4 << 30}
	for _, tc := range []struct {
		name   string
		iso    string
		labels map[string]string
		vols   []config.Volume
		drift  string
	}{
		{"same", isolationVM, map[string]string{assets.LabelVolumes: `{"cache":"/c","docker":"/var/lib/docker"}`}, []config.Volume{cache}, ""},
		{"none", isolationVM, map[string]string{assets.LabelVolumes: `{"docker":"/var/lib/docker"}`}, nil, ""},
		{"a size alone changed", isolationVM, map[string]string{assets.LabelVolumes: `{"cache":"/c","docker":"/var/lib/docker"}`},
			[]config.Volume{{Name: "cache", Path: "/c", Size: 9 << 30}}, ""},
		{"added", isolationVM, map[string]string{assets.LabelVolumes: `{"docker":"/var/lib/docker"}`}, []config.Volume{cache},
			"the VM was created with volumes none; the configuration says cache at /c"},
		{"moved", isolationVM, map[string]string{assets.LabelVolumes: `{"cache":"/old","docker":"/var/lib/docker"}`}, []config.Volume{cache},
			"created with volumes cache at /old; the configuration says cache at /c"},
		{"no label", isolationVM, map[string]string{}, nil, "records no volumes"},
		{"unreadable", isolationVM, map[string]string{assets.LabelVolumes: "["}, nil, "cannot be read"},
		{"a container, same", isolationContainer, map[string]string{assets.LabelVolumes: `{"cache":"/c"}`}, []config.Volume{cache}, ""},
		{"a container, none", isolationContainer, map[string]string{assets.LabelVolumes: `{}`}, nil, ""},
		{"a container, added", isolationContainer, map[string]string{assets.LabelVolumes: `{}`}, []config.Volume{cache},
			"the container was created with volumes none; the configuration says cache at /c"},
		{"a container, moved", isolationContainer, map[string]string{assets.LabelVolumes: `{"cache":"/old"}`}, []config.Volume{cache},
			"the container was created with volumes cache at /old; the configuration says cache at /c"},
		{"a container, removed", isolationContainer, map[string]string{assets.LabelVolumes: `{"cache":"/c"}`}, nil,
			"the container was created with volumes cache at /c; the configuration says none"},
		{"a container, no label", isolationContainer, map[string]string{}, nil, "the container records no volumes"},
		{"gvisor, added", isolationGVisor, map[string]string{assets.LabelVolumes: `{}`}, []config.Volume{cache},
			"the container was created with volumes none; the configuration says cache at /c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBoxApp(t, tc.iso, &backendtest.Fake{Status: "running", SandboxLabels: tc.labels})
			b.Cfg.Volumes = tc.vols
			got := b.volumesDrift()
			if (tc.drift == "") != (got == "") || !strings.Contains(got, tc.drift) {
				t.Errorf("drift %q, want %q", got, tc.drift)
			}
		})
	}
}

// Under every isolation, a keep entry at, inside or around a volume's
// path is skipped, as a problem of the sandbox config naming the volume;
// the others stay.
func TestDropKeepsUnderVolumes(t *testing.T) {
	parse := func() *datadir.Sandbox {
		c, err := sandboxcfg.Parse([]byte("[[keep]]\npath = \"~/.cache/go-build\"\n[[keep]]\npath = \"~/.local/share/x\"\n[[keep]]\npath = \"~/.npm\"\n[[keep]]\npath = \"~/.cargo\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		return &datadir.Sandbox{Config: c}
	}
	// A volume at a keep entry, one around another, one inside a third.
	vols := []config.Volume{{Name: "gobuild", Path: "/home/agent/.cache/go-build"}, {Name: "share", Path: "/home/agent/.local/share"},
		{Name: "npm", Path: "/home/agent/.npm/_cacache"}}
	rels := func(sb *datadir.Sandbox) []string {
		var out []string
		for _, k := range sb.Keep {
			if !k.Required {
				out = append(out, k.Rel)
			}
		}
		return out
	}
	for _, iso := range isolations {
		b := newBoxApp(t, iso, &backendtest.Fake{})
		b.Cfg.Volumes = vols
		b.Cfg.Profile = iso + ".a"
		sb := parse()
		n := len(sb.Problems)
		b.dropKeepsUnderVolumes(sb)
		added := sb.Problems[n:]
		if got := rels(sb); !reflect.DeepEqual(got, []string{".cargo"}) {
			t.Errorf("%s: kept %q", iso, got)
		}
		if len(added) != 3 || added[0] != "keep ~/.cache/go-build is at, inside or around volume gobuild (/home/agent/.cache/go-build): skipped, the volume wins (["+iso+".a.volume.gobuild])" ||
			!strings.Contains(added[1], "keep ~/.local/share/x is at, inside or around volume share") ||
			!strings.Contains(added[2], "keep ~/.npm is at, inside or around volume npm (/home/agent/.npm/_cacache)") {
			t.Errorf("%s: problems %q", iso, added)
		}
	}
}

// doctor lists a VM's volumes, says when a disk is larger than its size
// (kept), dockerd's too, and lists a container's with their docker
// volumes.
func TestDoctorVolumes(t *testing.T) {
	b := newBoxApp(t, isolationVM, &backendtest.Fake{})
	b.Cfg.Profile = "vm.a"
	b.Cfg.Volumes = []config.Volume{{Name: "cache", Path: "/c", Size: 8 << 30}, {Name: "data", Path: "/d", Size: 16 << 30}}
	doctor := func(a *App) *checkup {
		c := &checkup{}
		a.doctorVolumes(c)
		return c
	}
	b.macOS = func() string { return "27.0" }
	if c := doctor(b.App); c.row(levelOK, "cache at /c, 8G (made at the next start); data at /d, 16G (made at the next start); each on caboose-vmm's own disk") == nil ||
		c.row(levelNote, "framework") != nil {
		t.Errorf("new:\n%s", c)
	}
	b.macOS = func() string { return "15.2" }
	if c := doctor(b.App); c.row(levelOK, "data at /d, 16G (made at the next start)") == nil || c.row(levelOK, "caboose-vmm's own disk") != nil ||
		c.row(levelNote, "cache, data go on Virtualization.framework's own disks, as this Mac runs macOS 15.2") == nil {
		t.Errorf("new, before macOS 27:\n%s", c)
	}
	b.macOS = func() string { return "27.0" }
	if err := os.MkdirAll(b.volumesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int64{"cache": 12 << 30, "data": 16 << 30, "docker": 100 << 30} {
		if err := os.WriteFile(b.volumeDisk(name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(b.volumeDisk(name), size); err != nil {
			t.Fatal(err)
		}
	}
	c := doctor(b.App)
	if c.row(levelNote, "cache's disk is 12G, more than its size, 8G ([vm.a.volume.cache]): kept, as a start never shrinks a disk") == nil ||
		c.row(levelNote, "dockerd's disk is 100G, more than docker_size, 64G: kept") == nil {
		t.Errorf("larger:\n%s", c)
	}

	k := newBoxApp(t, isolationContainer, &backendtest.Fake{})
	k.Cfg.Volumes = b.Cfg.Volumes
	k.Cfg.Profile = "container.a"
	k.macOS = func() string { t.Error("macOS asked under container"); return "" }
	k.box.EngineVols = []backend.EngineVolume{{Name: "data", Engine: "caboose-default-data", Size: "24.08kB"}}
	if c := doctor(k.App); c.row(levelOK, "cache at /c (its docker volume, "+k.Cfg.Container+"-cache, is made as the container is created); data at /d, docker volume caboose-default-data, 24.08kB") == nil {
		t.Errorf("container:\n%s", c)
	}
	k.box.ListVolumesErr = errors.New("engine down")
	if c := doctor(k.App); c.row(levelNote, "cannot list the docker volumes of "+k.Cfg.Container+": engine down") == nil {
		t.Errorf("unlisted:\n%s", c)
	}
	k.Cfg.Volumes = nil
	if c := doctor(k.App); len(c.rows) != 0 {
		t.Errorf("no volumes:\n%s", c)
	}
}

// A profile can declare no more volumes than a VM takes on caboose-vmm's
// device, where they all go.
func TestMaxVMVolumes(t *testing.T) {
	if config.MaxVMVolumes != vm.MaxBlockDevices {
		t.Errorf("config takes %d volumes, a VM %d", config.MaxVMVolumes, vm.MaxBlockDevices)
	}
}

// A VM created before a volume was added says so at a launch, in status
// and in doctor, naming the restart that fixes it.
func TestVolumesDriftSaid(t *testing.T) {
	b := newBoxApp(t, isolationVM, runningBox(isolationVM))
	b.Cfg.VMCPUs, b.Cfg.VMMemory = 4, "6G"
	b.mountLocal("local/linux-arm64")
	b.Cfg.Volumes = []config.Volume{{Name: "cache", Path: "/c", Size: 4 << 30}}
	const drift = "the VM was created with volumes none; the configuration says cache at /c"
	if err := b.Status(); err != nil {
		t.Fatal(err)
	}
	if _, errs := b.said(); !strings.Contains(errs, drift) || !strings.Contains(errs, "run 'caboose restart' to recreate it with them") {
		t.Errorf("status said:\n%s", errs)
	}
	_ = b.Doctor([]string{"--offline"})
	if out, _ := b.said(); !strings.Contains(out, drift) {
		t.Errorf("doctor said:\n%s", out)
	}
	_ = b.ensureRunning(false)
	if !strings.Contains(b.errb.String(), drift) {
		t.Errorf("the launch said:\n%s", b.errb)
	}
}

// The sandbox config a launch uses has the keep entries a volume covers
// skipped.
func TestSandboxConfigDropsKeepsUnderVolumes(t *testing.T) {
	b := newBoxApp(t, isolationVM, &backendtest.Fake{})
	b.Cfg.Profile = "vm.a"
	b.Cfg.Volumes = []config.Volume{{Name: "gobuild", Path: "/home/agent/.cache/go-build"}}
	cfg := filepath.Join(b.Cfg.DataDir, datadir.SandboxConfig)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[[keep]]\npath = \"~/.cache/go-build\"\n[[keep]]\npath = \"~/.npm\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := b.sandboxConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range sb.Keep {
		if k.Rel == ".cache/go-build" {
			t.Error("the entry under the volume is kept")
		}
	}
	if !slices.ContainsFunc(sb.Keep, func(k sandboxcfg.Keep) bool { return k.Rel == ".npm" }) {
		t.Error("the other entry is gone")
	}
}

// The volumes' owner is the image's agent user, by the IDs its labels
// record (this process's when it has none); none under vm, with no
// volumes, or for a container that runs as root.
func TestVolumeOwner(t *testing.T) {
	vols := backend.Spec{Volumes: []backend.Volume{{Name: "cache", Target: "/c"}}}
	img := map[string]string{assets.LabelUID: "1234", assets.LabelGID: "99"}
	me := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	root := vols
	root.User = rootUser
	for _, tc := range []struct {
		name  string
		vm    bool
		spec  backend.Spec
		image map[string]string
		want  string
	}{
		{"the agent", false, vols, img, "1234:99"},
		{"no labels", false, vols, nil, me},
		{"a uid alone", false, vols, map[string]string{assets.LabelUID: "1234"}, me},
		{"root", false, root, img, ""},
		{"vm", true, vols, img, ""},
		{"no volumes", false, backend.Spec{}, img, ""},
	} {
		if got := volumeOwner(tc.vm, tc.spec, tc.image); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Under gVisor where the agent cannot write its mounts, the container
// runs as root, which owns a new volume already: no owner is asked for.
// Where it can, the agent is.
func TestCreateVolumesGVisorUser(t *testing.T) {
	for _, tc := range []struct{ probe, owner string }{
		{"no", ""},
		{"yes", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())},
	} {
		b := creating(t, isolationGVisor, currentLabels("linux-x64"))
		b.write(t, "probe-answer", tc.probe)
		b.Cfg.Volumes = []config.Volume{{Name: "cache", Path: "/c"}}
		b.Cfg.Profile = "gvisor.a"
		if err := b.createContainer(false); err != nil {
			t.Fatalf("%v\n%s", err, b.errb)
		}
		spec, _ := b.box.Spec()
		if spec.VolumeOwner != tc.owner || len(spec.Volumes) != 1 {
			t.Errorf("probe %s: owner %q, want %q; user %q", tc.probe, spec.VolumeOwner, tc.owner, spec.User)
		}
	}
}

// A container whose volumes could not be readied fails the launch with
// the backend's message, docker's captured stderr in it, though the error
// unwraps to docker's own exit status: docker showed nothing, so a bare
// status would end the launch saying nothing.
func TestCreateVolumeFailureSaid(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 3").Run()
	var ee *exec.ExitError
	if !errors.As(exitErr, &ee) {
		t.Fatalf("no exit error: %v", exitErr)
	}
	msg := "creating docker volume caboose-default-cache: Error: disk full; the container was not created: fix what docker says, then run caboose again"
	for _, iso := range []string{isolationContainer, isolationGVisor} {
		b := creating(t, iso, currentLabels("linux-x64"))
		b.Cfg.Volumes = []config.Volume{{Name: "cache", Path: "/c"}}
		b.box.CreateErr = &backend.VolumeError{Msg: msg, Err: &docker.Failure{Err: exitErr, Stderr: "Error: disk full"}}
		err := b.createContainer(false)
		var got *ExitError
		if !errors.As(err, &got) || got.Code != 1 || got.Msg != msg {
			t.Errorf("%s: err = %#v", iso, err)
		}
	}
}

// The user's docker run arguments cannot mount over a volume: caboose's
// own -v for it is checked against, as for its other mounts.
func TestRunArgsOverVolume(t *testing.T) {
	b := creating(t, isolationContainer, currentLabels("linux-x64"))
	b.Cfg.Volumes = []config.Volume{{Name: "cache", Path: "/srv/cache"}}
	b.Cfg.Profile = "container.a"
	b.Cfg.RunArgs = []string{"--tmpfs=/srv/cache"}
	err := b.createContainer(false)
	if err == nil || !strings.Contains(err.Error(), "/srv/cache") {
		t.Errorf("err = %v\n%s", err, b.errb)
	}
	if len(b.box.Specs) != 0 {
		t.Error("created")
	}
}
