package launcher

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/policy"
)

// placeDaemon is a running daemon that answers writable, and nothing
// else.
type placeDaemon struct {
	fakeDaemon
	writable *daemonproto.WritableResult
	err      error
}

func (d *placeDaemon) Writable(context.Context) (*daemonproto.WritableResult, error) {
	return d.writable, d.err
}

// placeRig is an app whose place is judged against base, the default
// environment's App: a container sandbox whose root is root, under a
// CABOOSE_HOME of its own; no other environment.
type placeRig struct {
	app  *App
	base *boxApp
	// safe is a directory no sandbox reaches; root the sandbox's root.
	safe, root string
	// vols are the volumes base's engine has, and volErr, when set, what
	// it answers about any (fakeVolumes).
	vols   map[string]docker.Volume
	volErr error
}

// fakeVolumes is an engine that has vols, or answers err about every
// volume when err is set.
func fakeVolumes(vols *map[string]docker.Volume, err *error) func(string) (docker.Volume, bool, error) {
	return func(name string) (docker.Volume, bool, error) {
		if *err != nil {
			return docker.Volume{}, false, *err
		}
		v, ok := (*vols)[name]
		return v, ok, nil
	}
}

func newPlaceRig(t *testing.T) *placeRig {
	t.Helper()
	return newPlaceRigUnder(t, isolationContainer)
}

// newPlaceRigUnder is newPlaceRig with the default environment's sandbox
// under the isolation iso.
func newPlaceRigUnder(t *testing.T, iso string) *placeRig {
	t.Helper()
	base := newBoxApp(t, iso, &backendtest.Fake{})
	// Every place physical: a temp dir may be reached through a symlink
	// (a Mac's /var is /private/var), which the check refuses on its own.
	root := realTempDir(t, base.tmp)
	base.Cfg.Roots = []config.Root{{Host: root, Container: "/work"}}
	base.Cfg.CabooseHome = filepath.Join(realTempDir(t, ""), "caboose-home")
	if err := os.MkdirAll(base.Cfg.CabooseHome, 0o700); err != nil {
		t.Fatal(err)
	}
	safe := realTempDir(t, "")
	a := appApp(t, "")
	a.appBase = func() (*App, error) { return base.App, nil }
	a.appEnvApps = func(env string) (*App, error) { return nil, errors.New("no environment " + env) }
	r := &placeRig{app: a, base: base, safe: safe, root: root}
	base.inspectVolume = fakeVolumes(&r.vols, &r.volErr)
	return r
}

// realTempDir is dir, or a new temp dir when dir is "", with every
// symlink in it resolved.
func realTempDir(t *testing.T, dir string) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *placeRig) check(t *testing.T, exe string) error {
	t.Helper()
	return r.app.checkAppPlace(context.Background(), exe)
}

// noRunArgs are the labels of a sandbox created with no run_args.
var noRunArgs = map[string]string{assets.LabelRunArgs: ""}

func wantRefused(t *testing.T, err error, says string) {
	t.Helper()
	if err == nil {
		t.Fatal("allowed")
	}
	if !strings.Contains(err.Error(), says) {
		t.Fatalf("refused with %q, want it to say %q", err, says)
	}
}

// A bundle where no sandbox reaches runs; the fixed place make app uses
// is one.
func TestAppPlaceAllowed(t *testing.T) {
	r := newPlaceRig(t)
	if err := r.check(t, bundle(t, r.safe, appbundle.CheckoutID)); err != nil {
		t.Fatalf("a place no sandbox reaches: %v", err)
	}
	fixed := appbundle.CheckoutPath(r.base.Cfg.CabooseHome)
	exe := bundle(t, filepath.Dir(fixed), appbundle.CheckoutID)
	if err := r.check(t, exe); err != nil {
		t.Fatalf("make app's place: %v", err)
	}
}

// A bundle in a root is refused, read-only or not, configured or of
// another environment.
func TestAppPlaceInARoot(t *testing.T) {
	r := newPlaceRig(t)
	wantRefused(t, r.check(t, bundle(t, filepath.Join(r.root, "checkout"), appbundle.CheckoutID)), "a sandbox reaches it")

	ro := filepath.Join(r.safe, "ro")
	r.base.Cfg.Roots = append(r.base.Cfg.Roots, config.Root{Host: ro, Container: "/work/ro"})
	wantRefused(t, r.check(t, bundle(t, filepath.Join(ro, "x"), appbundle.CheckoutID)), "a sandbox reaches it")

	// Another environment's root.
	other := realTempDir(t, "")
	if err := os.MkdirAll(config.EnvDirFor(r.base.Cfg.CabooseHome, "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	y := newBoxApp(t, isolationContainer, &backendtest.Fake{})
	y.Cfg.Env, y.Cfg.Roots = "y", []config.Root{{Host: other, Container: "/work"}}
	r.app.appEnvApps = func(env string) (*App, error) {
		if env == "y" {
			return y.App, nil
		}
		return nil, errors.New("no environment " + env)
	}
	wantRefused(t, r.check(t, bundle(t, filepath.Join(other, "c"), appbundle.CheckoutID)), "within "+other)
}

// What a running daemon says the sandboxes write (the broker's writable
// set, writableOf) is refused too; a daemon that cannot say refuses
// everything, and one that does not answer is not asked.
func TestAppPlaceUnderAWritablePart(t *testing.T) {
	r := newPlaceRig(t)
	part := filepath.Join(r.safe, "part")
	exe := bundle(t, filepath.Join(part, "x"), appbundle.CheckoutID)
	if err := r.check(t, exe); err != nil {
		t.Fatalf("no daemon, nothing configured there: %v", err)
	}
	// A sandbox that mounts it, writable, with no daemon to ask.
	box := r.base.box
	box.SandboxLabels = noRunArgs
	box.Status, box.SandboxMounts = "exited", []backend.Mount{{Source: part, Target: "/home/agent/part"}}
	wantRefused(t, r.check(t, exe), "within "+part)
	box.Status, box.SandboxMounts = "", nil

	d := &placeDaemon{writable: &daemonproto.WritableResult{
		Writable: []daemonproto.WritablePath{{Path: part, Envs: []string{"default"}}},
		ReadOnly: []daemonproto.WritablePath{}}}
	r.base.DaemonDial = func(context.Context, bool) (DaemonCaller, error) { return d, nil }
	wantRefused(t, r.check(t, exe), "within "+part)

	d.writable = &daemonproto.WritableResult{Writable: []daemonproto.WritablePath{},
		ReadOnly: []daemonproto.WritablePath{{Path: part, Envs: []string{"default"}}}}
	wantRefused(t, r.check(t, exe), "within "+part)

	d.writable, d.err = nil, errors.New("docker did not answer")
	wantRefused(t, r.check(t, bundle(t, r.safe, appbundle.CheckoutID)), "cannot be told")

	d.writable, d.err = &daemonproto.WritableResult{Writable: []daemonproto.WritablePath{{Path: "relative", Envs: []string{"default"}}}}, nil
	wantRefused(t, r.check(t, bundle(t, r.safe, appbundle.CheckoutID)), "cannot be told")
}

// A bundle reached through a symlink is refused, whether the link is a
// directory above it or the bundle itself, and wherever it points.
func TestAppPlaceThroughASymlink(t *testing.T) {
	r := newPlaceRig(t)
	real := filepath.Join(r.safe, "real")
	exe := bundle(t, real, appbundle.CheckoutID)
	link := filepath.Join(r.safe, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(link, appbundle.Name, "Contents", "MacOS", "caboose")
	wantRefused(t, r.check(t, via), "through "+link)

	app := filepath.Join(r.safe, "app", appbundle.Name)
	if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, appbundle.Name), app); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, r.check(t, filepath.Join(app, "Contents", "MacOS", "caboose")), "through "+app)

	// The executable itself.
	solo := filepath.Join(r.safe, "solo")
	if err := os.Symlink(exe, solo); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, r.check(t, solo), "through "+solo)
	if err := r.check(t, exe); err != nil {
		t.Fatalf("the place itself: %v", err)
	}
}

// What the disk says of a bundle's place and of what the sandboxes reach
// lines up by identity: a reached directory spelled another way (a Mac's
// firmlink, which a Linux disk cannot make) still refuses a bundle in it.
func TestAppPlaceByIdentity(t *testing.T) {
	r := newPlaceRig(t)
	part := filepath.Join(r.safe, "part")
	exe := bundle(t, part, appbundle.CheckoutID)
	r.base.Cfg.Roots = append(r.base.Cfg.Roots, config.Root{Host: part, Container: "/work/part"})
	reach := r.base.appReach(context.Background(), r.app.appEnvApps)
	if reach.Err != nil {
		t.Fatal(reach.Err)
	}
	// The same reach, every path spelled as the disk never does.
	var other []string
	for _, p := range reach.Paths {
		other = append(other, "/System/Volumes/Data"+p)
	}
	reach.Paths = other
	wantRefused(t, policy.CheckAppPlace(hostlink.AppPlaceOf(exe), reach), "by another name")
	reach.IDs = nil
	if err := policy.CheckAppPlace(hostlink.AppPlaceOf(exe), reach); err != nil {
		t.Fatalf("with no identities, the spelling alone does not hold it: %v", err)
	}
}

// What the sandboxes reach that cannot be told refuses every place: an
// environment whose configuration does not load, an App that cannot be
// made.
func TestAppPlaceReachUnknown(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, r.safe, appbundle.CheckoutID)
	if err := os.MkdirAll(config.EnvDirFor(r.base.Cfg.CabooseHome, "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, r.check(t, exe), "cannot be told")

	r = newPlaceRig(t)
	r.app.appBase = func() (*App, error) { return nil, errors.New("config.toml: nope") }
	wantRefused(t, r.check(t, bundle(t, r.safe, appbundle.CheckoutID)), "cannot be told")
}

// caboose app refuses to run from a place a sandbox reaches, with exit
// status 1 and the reason, before anything of the app's runs.
func TestRunAppRefusesItsPlace(t *testing.T) {
	r := newPlaceRig(t)
	pinAppSupported(t, true) // after newBoxApp, which pins it off
	exe := bundle(t, filepath.Join(r.root, "checkout"), appbundle.CheckoutID)
	r.app.Executable = func() (string, error) { return exe, nil }
	for _, args := range [][]string{nil, {"--selftest"}, {"-psn_0_42"}} {
		err := r.app.RunApp(args)
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(ee.Msg, "refuses to run from "+exe) || !strings.Contains(ee.Msg, "a sandbox reaches it") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// The app's core asks its place again before Open at login: the place
// the app runs from, judged as at its start, against what the sandboxes
// reach then.
func TestAppOptionsLoginCheck(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "b"), appbundle.CheckoutID)
	o := r.app.appOptions(exe, nil)
	if o.LoginCheck == nil {
		t.Fatal("no LoginCheck")
	}
	if err := o.LoginCheck(); err != nil {
		t.Fatalf("a place no sandbox reaches: %v", err)
	}
	// A root that now holds it.
	r.base.Cfg.Roots = append(r.base.Cfg.Roots, config.Root{Host: filepath.Join(r.safe, "b"), Container: "/work/b"})
	wantRefused(t, o.LoginCheck(), "a sandbox reaches it")
}

// A root or a mount inside the bundle (its Info.plist, its resources) is
// the sandbox's to change what the host reads: refused.
func TestAppPlaceReachedInsideTheBundle(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "b"), appbundle.CheckoutID)
	contents := filepath.Join(r.safe, "b", appbundle.Name, "Contents")
	res := filepath.Join(contents, "Resources") // the icon's, made by Assemble
	r.base.Cfg.Roots = append(r.base.Cfg.Roots, config.Root{Host: res, Container: "/work/c"})
	wantRefused(t, r.check(t, exe), "into its bundle")
	r.base.Cfg.Roots = r.base.Cfg.Roots[:1]
	box := r.base.box
	box.SandboxLabels = noRunArgs
	box.Status, box.SandboxMounts = "exited", []backend.Mount{{Source: filepath.Join(contents, "Info.plist"), Target: "/home/agent/p"}}
	wantRefused(t, r.check(t, exe), "into its bundle")
	box.Status, box.SandboxMounts = "", nil
	if err := r.check(t, exe); err != nil {
		t.Fatal(err)
	}
}

// What run_args mounts counts whatever the engine says now: a --volume's
// or a --mount's source; --volumes-from, which cannot be told, refuses.
func TestAppPlaceRunArgs(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "v"), appbundle.CheckoutID)
	for _, args := range [][]string{
		{"--volume=" + filepath.Join(r.safe, "v") + ":/data:ro"},
		{"--mount=type=bind,source=" + r.safe + ",target=/data"},
		{"--mount=type=bind,src=" + filepath.Join(r.safe, "v", appbundle.Name) + ",dst=/data"},
		// docker's CLI reads a --mount as CSV, with keys in any case.
		{"--mount=type=bind,Source=" + r.safe + ",target=/data"},
		{"--mount=type=bind,SRC=" + r.safe + ",Target=/data"},
		{`--mount=type=bind,"source=` + r.safe + `",target=/data`},
	} {
		r.base.Cfg.RunArgs = args
		wantRefused(t, r.check(t, exe), "a sandbox reaches it")
	}
	r.base.Cfg.RunArgs = []string{"--volume=named:/data", "--mount=type=volume,target=/x", "--volume=/anon"}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("named and anonymous volumes: %v", err)
	}
	r.base.Cfg.RunArgs = []string{"--volumes-from=other"}
	wantRefused(t, r.check(t, exe), "another container")
	// What docker reads against the directory it runs in, or would not
	// read at all, cannot be told either.
	for _, args := range [][]string{
		{"--volume=./v:/data"},
		{"--mount=type=bind,source=../v,target=/data"},
		{`--mount=type=bind,"source=/x,target=/data`},
		// A volume's driver or options can bind any path of the
		// engine's host, in any case of the key.
		{"--mount=type=volume,source=v,target=/data,volume-opt=type=none,volume-opt=o=bind,volume-opt=device=" + r.safe},
		{"--mount=type=volume,source=v,target=/data,Volume-Opt=device=" + r.safe},
		{"--mount=type=volume,source=v,target=/data,VOLUME-DRIVER=local"},
		{`--mount=type=volume,source=v,target=/data,"volume-opt=o=bind"`},
		{"--volume-driver=local", "--volume=v:/data"},
	} {
		r.base.Cfg.RunArgs = args
		if err := r.check(t, exe); err == nil || !strings.Contains(err.Error(), "cannot be told") {
			t.Errorf("%v: %v, want it refused as what cannot be told", args, err)
		}
	}
	r.base.Cfg.RunArgs = nil
	if err := r.check(t, exe); err != nil {
		t.Fatalf("no run_args: %v", err)
	}
	// Another environment's run_args count as much as this one's.
	if err := os.MkdirAll(config.EnvDirFor(r.base.Cfg.CabooseHome, "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	y := newBoxApp(t, isolationContainer, &backendtest.Fake{})
	y.Cfg.Env = "y"
	r.app.appEnvApps = func(env string) (*App, error) {
		if env == "y" {
			return y.App, nil
		}
		return nil, errors.New("no environment " + env)
	}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("another environment with no run_args: %v", err)
	}
	y.Cfg.RunArgs = []string{"--mount=type=bind,Source=" + r.safe + ",target=/data"}
	wantRefused(t, r.check(t, exe), "a sandbox reaches it")
	y.Cfg.RunArgs = []string{"--volumes-from=other"}
	wantRefused(t, r.check(t, exe), "environment y's run_args")
}

// An executable with another hard link is the same file under a name the
// checks do not see, which a sandbox might reach: refused, wherever that
// link is.
func TestAppPlaceHardLinked(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "v"), appbundle.CheckoutID)
	if err := r.check(t, exe); err != nil {
		t.Fatalf("one link: %v", err)
	}
	if err := os.Link(exe, filepath.Join(r.safe, "other-name")); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, r.check(t, exe), "2 hard links")
}

// What is in the bundle is judged by identity too: a file of it with a
// hard link in a root, a symlink in it, and a reached directory inside
// it spelled as the disk never does (a Mac's firmlink, which a Linux disk
// cannot make) are each refused.
func TestAppPlaceBundleByIdentity(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "b"), appbundle.CheckoutID)
	contents := filepath.Join(r.safe, "b", appbundle.Name, "Contents")
	if err := r.check(t, exe); err != nil {
		t.Fatalf("a bundle of its own files: %v", err)
	}

	plist := filepath.Join(contents, "Info.plist")
	inRoot := filepath.Join(r.root, "plist")
	if err := os.Link(plist, inRoot); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, r.check(t, exe), "a file with 2 hard links")
	if err := os.Remove(inRoot); err != nil {
		t.Fatal(err)
	}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("the link removed: %v", err)
	}

	// Resources, the icon's, replaced by a link.
	link := filepath.Join(contents, "Resources")
	if err := os.Remove(filepath.Join(link, appbundle.IconFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.root, link); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, r.check(t, exe), "a symlink")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	res := filepath.Join(contents, "Resources")
	if err := os.Mkdir(res, 0o755); err != nil {
		t.Fatal(err)
	}
	r.base.Cfg.Roots = append(r.base.Cfg.Roots, config.Root{Host: res, Container: "/work/res"})
	reach := r.base.appReach(context.Background(), r.app.appEnvApps)
	if reach.Err != nil {
		t.Fatal(reach.Err)
	}
	var other []string
	for _, p := range reach.Paths {
		other = append(other, "/System/Volumes/Data"+p)
	}
	reach.Paths = other
	pl := hostlink.AppPlaceOf(exe)
	pl.Bundle = appbundle.BundleOf(exe)
	pl.BundleEntries, pl.BundleErr = hostlink.BundleEntriesOf(pl.Bundle)
	wantRefused(t, policy.CheckAppPlace(pl, reach), "into its bundle by another name")
}

// A setting that hands a sandbox everything refuses every place, wherever
// the app is, and says which setting: engine_socket on (in this
// environment or another), a sandbox made with the engine's socket, a
// run_args flag that widens it past any list of paths, a socket run_args
// mounts. What tightens, or only names a network, does not.
func TestAppPlaceReachEverything(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "b"), appbundle.CheckoutID)
	r.base.Cfg.EngineSocket = true
	wantRefused(t, r.check(t, exe), "everything on this machine through engine_socket")
	r.base.Cfg.EngineSocket = false

	box := r.base.box
	box.SandboxLabels = noRunArgs
	box.Status, box.SandboxMounts = "exited", []backend.Mount{{Source: filepath.Join(r.safe, "docker.sock"), Target: "/var/run/docker.sock"}}
	wantRefused(t, r.check(t, exe), "the engine's socket, which environment default's sandbox mounts")
	box.Status, box.SandboxMounts = "", nil

	for _, arg := range []string{
		"--privileged", "--use-api-socket", "--cap-add=SYS_ADMIN", "--device=/dev/sda", "--device-cgroup-rule=a *:* rwm",
		"--gpus=all", "--security-opt=apparmor=unconfined", "--security-opt=seccomp=unconfined", "--security-opt=label=disable",
		"--pid=host", "--pid=container:other", "--ipc=host", "--ipc=container:other", "--userns=host", "--cgroupns=host",
		"--network=host", "--net=host", "--network=container:other", "--net=Container:other",
	} {
		r.base.Cfg.RunArgs = []string{"--env=X=1", arg}
		wantRefused(t, r.check(t, exe), "everything on this machine through "+arg+" in run_args")
	}
	for _, arg := range []string{"--security-opt=no-new-privileges", "--security-opt=no-new-privileges:true", "--ipc=private", "--cgroupns=private", "--network=bridge", "--network=mynet", "--uts=host"} {
		r.base.Cfg.RunArgs = []string{arg}
		if err := r.check(t, exe); err != nil {
			t.Errorf("%s: %v", arg, err)
		}
	}

	sock := filepath.Join(shortTempDir(t), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r.base.Cfg.RunArgs = []string{"--volume=" + sock + ":/var/run/x.sock"}
	wantRefused(t, r.check(t, exe), "the socket "+sock)
	r.base.Cfg.RunArgs = nil

	// Another environment's.
	if err := os.MkdirAll(config.EnvDirFor(r.base.Cfg.CabooseHome, "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	y := newBoxApp(t, isolationContainer, &backendtest.Fake{})
	y.Cfg.Env, y.Cfg.EngineSocket = "y", true
	r.app.appEnvApps = func(env string) (*App, error) {
		if env == "y" {
			return y.App, nil
		}
		return nil, errors.New("no environment " + env)
	}
	wantRefused(t, r.check(t, exe), "of environment y, which hands it the engine")
	y.Cfg.EngineSocket = false
	if err := r.check(t, exe); err != nil {
		t.Fatal(err)
	}
}

// A mount at or above a place an engine's socket is kept hands the
// sandbox the engine: refused, whether run_args or the sandbox made mount
// it, and whatever socket is there now.
func TestAppPlaceMountHoldsEngineSocket(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "b"), appbundle.CheckoutID)
	home := realTempDir(t, "")
	r.base.Cfg.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("nothing mounted: %v", err)
	}
	for _, src := range []string{"/run", "/var/run", "/var", "/", filepath.Join(home, ".orbstack"), filepath.Join(home, ".docker", "run"), home,
		filepath.Join(home, ".docker"), filepath.Join(home, ".colima"), filepath.Join(home, ".colima", "work"), filepath.Join(home, ".lima", "vm1", "sock")} {
		r.base.Cfg.RunArgs = []string{"--volume=" + src + ":/x"}
		// The reason is clipped where the temp dir's path is long: what
		// comes first is the mount.
		wantRefused(t, r.check(t, exe), "everything on this machine through "+src+", which run_args")
	}
	r.base.Cfg.RunArgs = []string{"--mount=type=bind,source=/run,target=/x"}
	wantRefused(t, r.check(t, exe), "holds /run/docker.sock")
	r.base.Cfg.RunArgs = []string{"--volume=" + filepath.Join(home, ".orbstack", "config") + ":/x"}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("a directory beside the socket's: %v", err)
	}
	r.base.Cfg.RunArgs = nil

	// A root holds one, read-only or not, with no sandbox made yet.
	for _, ro := range []bool{false, true} {
		root := config.Root{Host: home, Container: "/work/home"}
		if ro {
			root.Writable = nil
		} else {
			root.Writable = []string{"."}
		}
		r.base.Cfg.Roots = append(r.base.Cfg.Roots[:1:1], root)
		wantRefused(t, r.check(t, exe), "root /work/home of environment default")
	}
	r.base.Cfg.Roots = r.base.Cfg.Roots[:1]
	if err := r.check(t, exe); err != nil {
		t.Fatal(err)
	}

	box := r.base.box
	box.SandboxLabels = noRunArgs
	box.Status, box.SandboxMounts = "exited", []backend.Mount{{Source: filepath.Join(home, ".orbstack"), Target: "/home/agent/o"}}
	wantRefused(t, r.check(t, exe), "which environment default's sandbox mounts at /home/agent/o")
}

// The run_args a sandbox was created with count as much as the
// configuration's: a sandbox made privileged is refused after the
// configuration drops it, until it is recreated; one that does not say
// what it was created with cannot be told.
func TestAppPlaceCreatedRunArgs(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "b"), appbundle.CheckoutID)
	box := r.base.box
	box.Status = "exited"
	box.SandboxLabels = map[string]string{assets.LabelRunArgs: `["--privileged"]`}
	wantRefused(t, r.check(t, exe), "--privileged in the run_args its sandbox was created with")
	box.SandboxLabels = map[string]string{assets.LabelRunArgs: `["--volume=` + r.safe + `:/x"]`}
	wantRefused(t, r.check(t, exe), "a sandbox reaches it")
	box.SandboxLabels = map[string]string{assets.LabelRunArgs: `["--volumes-from=other"]`}
	wantRefused(t, r.check(t, exe), "cannot be told")
	box.SandboxLabels = map[string]string{}
	wantRefused(t, r.check(t, exe), "does not say what run_args it was created with")
	box.SandboxLabels = map[string]string{assets.LabelRunArgs: "not json"}
	wantRefused(t, r.check(t, exe), "cannot be read")
	box.SandboxLabels = noRunArgs
	if err := r.check(t, exe); err != nil {
		t.Fatalf("created with none: %v", err)
	}
	box.Status = ""
	box.SandboxLabels = nil
	if err := r.check(t, exe); err != nil {
		t.Fatalf("no sandbox: %v", err)
	}
}

// labelsFail is a sandbox whose labels cannot be read.
type labelsFail struct{ *backendtest.Fake }

func (labelsFail) Labels() (map[string]string, error) { return nil, errors.New("inspect failed") }

// A sandbox that exists but whose labels cannot be read cannot say what
// run_args it was created with: refused.
func TestAppPlaceCreatedRunArgsUnreadable(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "b"), appbundle.CheckoutID)
	r.base.box.Status = "exited"
	r.base.Backend = labelsFail{r.base.box}
	wantRefused(t, r.check(t, exe), "its labels cannot be read: inspect failed")
}

// bindVolume is a local volume made with the options that bind dev, as
// `docker volume create -o type=none -o o=bind -o device=DEV` makes it.
func bindVolume(dev string) docker.Volume {
	return docker.Volume{Driver: "local", Options: map[string]string{"type": "none", "o": "bind", "device": dev}}
}

// A named volume run_args mount is judged by what the engine says of it:
// the engine's own storage is allowed, a volume that binds a directory
// reaches it, and what cannot be told refuses every place.
func TestAppPlaceRunArgsVolumes(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "v"), appbundle.CheckoutID)
	mounts := [][]string{
		{"--volume=named:/data"},
		{"--volume=named:/data:ro"},
		{"--mount=type=volume,source=named,target=/data"},
		// docker's --mount is a volume when it names no type.
		{"--mount=src=named,dst=/data"},
		{"--mount=Type=Volume,source=named,target=/data,readonly"},
	}
	allowed := func(what string) {
		t.Helper()
		for _, args := range mounts {
			r.base.Cfg.RunArgs = args
			if err := r.check(t, exe); err != nil {
				t.Errorf("%s, %v: %v", what, args, err)
			}
		}
	}
	refused := func(what, says string) {
		t.Helper()
		for _, args := range mounts {
			r.base.Cfg.RunArgs = args
			if err := r.check(t, exe); err == nil || !strings.Contains(err.Error(), says) {
				t.Errorf("%s, %v: %v, want it refused saying %q", what, args, err, says)
			}
		}
	}

	// Not made yet: docker makes it at the start, with no options.
	allowed("a volume the engine has not")
	for what, v := range map[string]docker.Volume{
		"the engine's own storage": {Driver: "local"},
		"empty options":            {Driver: "local", Options: map[string]string{}},
		"a size":                   {Driver: "local", Options: map[string]string{"size": "1G"}},
		"a tmpfs":                  {Driver: "local", Options: map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=100m"}},
		"a bind elsewhere":         bindVolume(filepath.Join(r.safe, "elsewhere")),
	} {
		r.vols = map[string]docker.Volume{"named": v}
		allowed(what)
	}

	for what, v := range map[string]docker.Volume{
		"a bind of it":                  {Driver: "local", Options: map[string]string{"type": "none", "o": "bind", "device": filepath.Join(r.safe, "v")}},
		"a bind above it":               bindVolume(r.safe),
		"a bind, uncleaned":             bindVolume(r.safe + "/x/../v/"),
		"o=bind alone":                  {Driver: "local", Options: map[string]string{"o": "ro,BIND", "device": r.safe}},
		"o=rbind":                       {Driver: "local", Options: map[string]string{"type": "ext4", "o": "rbind", "device": r.safe}},
		"type=none alone":               {Driver: "local", Options: map[string]string{"type": "none", "device": r.safe}},
		"a tmpfs that binds, after all": {Driver: "local", Options: map[string]string{"type": "tmpfs", "o": "bind", "device": r.safe}},
	} {
		r.vols = map[string]docker.Volume{"named": v}
		refused(what, "a sandbox reaches it")
	}

	for what, v := range map[string]docker.Volume{
		"a relative device":    bindVolume("v"),
		"no device":            {Driver: "local", Options: map[string]string{"type": "none", "o": "bind"}},
		"another driver":       {Driver: "rclone"},
		"no driver":            {},
		"nfs":                  {Driver: "local", Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.1,rw", "device": ":/export"}},
		"a block device":       {Driver: "local", Options: map[string]string{"type": "ext4", "device": "/dev/sdb1"}},
		"overlay":              {Driver: "local", Options: map[string]string{"type": "overlay", "o": "lowerdir=" + r.safe, "device": "overlay"}},
		"an option not known":  {Driver: "local", Options: map[string]string{"copy": "x"}},
		"options without type": {Driver: "local", Options: map[string]string{"o": "ro"}},
	} {
		r.vols = map[string]docker.Volume{"named": v}
		refused(what, "cannot be told")
	}

	// An engine that cannot say refuses every place.
	r.vols, r.volErr = nil, docker.Unreachable(errors.New("Cannot connect to the Docker daemon"))
	refused("an engine that does not answer", "cannot be told")
	r.volErr = errors.New("permission denied")
	refused("an engine that fails", "cannot be told")
	r.volErr = nil

	// A device that is or holds an engine's socket hands the sandbox
	// everything.
	r.vols = map[string]docker.Volume{"named": bindVolume("/run")}
	refused("a bind of /run", "where an engine's socket is kept")
	sock := filepath.Join(realTempDir(t, ""), "s.sock")
	if l, err := net.Listen("unix", sock); err == nil {
		defer l.Close()
		r.vols = map[string]docker.Volume{"named": bindVolume(sock)}
		refused("a bind of a socket", "the socket "+sock)
	} else {
		t.Logf("no unix socket here: %v", err)
	}

	// An anonymous volume is made with no options, whatever the engine
	// has; a name that is no volume's, or a bind whose source is not a
	// path, cannot be told.
	r.vols = map[string]docker.Volume{"named": bindVolume(r.safe)}
	for _, args := range [][]string{{"--volume=/data"}, {"--mount=type=volume,target=/data"}, {"--mount=target=/data"}, {"--mount=type=tmpfs,target=/data"}} {
		r.base.Cfg.RunArgs = args
		if err := r.check(t, exe); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"--volume=-x:/data"}, {"--mount=source=a/b,target=/data"}, {"--mount=type=bind,source=named,target=/data"}} {
		r.base.Cfg.RunArgs = args
		if err := r.check(t, exe); err == nil || !strings.Contains(err.Error(), "cannot be told") {
			t.Errorf("%v: %v, want it refused as what cannot be told", args, err)
		}
	}

	// Another environment's volumes are asked of its own engine.
	r.base.Cfg.RunArgs = nil
	if err := os.MkdirAll(config.EnvDirFor(r.base.Cfg.CabooseHome, "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	y := newBoxApp(t, isolationContainer, &backendtest.Fake{})
	y.Cfg.Env = "y"
	yvols := map[string]docker.Volume{}
	var yerr error
	y.inspectVolume = fakeVolumes(&yvols, &yerr)
	r.app.appEnvApps = func(env string) (*App, error) {
		if env == "y" {
			return y.App, nil
		}
		return nil, errors.New("no environment " + env)
	}
	y.Cfg.RunArgs = []string{"--volume=named:/data"}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("y's engine has no such volume: %v", err)
	}
	yvols["named"] = bindVolume(r.safe)
	wantRefused(t, r.check(t, exe), "a sandbox reaches it")
	yerr = errors.New("no engine")
	wantRefused(t, r.check(t, exe), "environment y's run_args")
}

// The volumes a sandbox mounts, as its engine reports them, are judged
// as run_args' are, but one the engine no longer has cannot be told; the
// run_args it was created with name their volumes too.
func TestAppPlaceSandboxVolumes(t *testing.T) {
	r := newPlaceRig(t)
	exe := bundle(t, filepath.Join(r.safe, "v"), appbundle.CheckoutID)
	box := r.base.box
	box.Status, box.SandboxLabels = "exited", noRunArgs
	vol := backend.Mount{Source: "/var/lib/docker/volumes/v/_data", Target: "/data", Type: "volume", Volume: "v"}
	box.SandboxMounts = []backend.Mount{vol}

	wantRefused(t, r.check(t, exe), "volume v, which a sandbox mounts, is not the engine's")
	r.vols = map[string]docker.Volume{"v": {Driver: "local"}}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("the engine's own storage: %v", err)
	}
	r.vols["v"] = bindVolume(r.safe)
	wantRefused(t, r.check(t, exe), "a sandbox reaches it")
	r.vols["v"] = bindVolume("/var/run")
	wantRefused(t, r.check(t, exe), "which environment default's sandbox mounts at /data as volume v")
	r.vols["v"] = docker.Volume{Driver: "nfs-plugin"}
	wantRefused(t, r.check(t, exe), "cannot be told")
	r.vols["v"], r.volErr = docker.Volume{Driver: "local"}, errors.New("engine gone")
	wantRefused(t, r.check(t, exe), "cannot be told")
	r.volErr = nil

	// A type of mount whose reach is not known cannot be told; a tmpfs
	// and an image hold nothing of the host's.
	for typ, ok := range map[string]bool{"tmpfs": true, "image": true, "bind": true, "cluster": false, "npipe": false, "something": false} {
		box.SandboxMounts = []backend.Mount{vol, {Target: "/t", Type: typ}}
		if err := r.check(t, exe); (err == nil) != ok || (!ok && !strings.Contains(err.Error(), "cannot be told")) {
			t.Errorf("a mount of type %s: %v", typ, err)
		}
	}

	// The run_args it was created with.
	box.SandboxMounts = nil
	box.SandboxLabels = map[string]string{assets.LabelRunArgs: `["--volume=w:/w"]`}
	if err := r.check(t, exe); err != nil {
		t.Fatalf("a created-with volume the engine has not: %v", err)
	}
	r.vols["w"] = bindVolume(r.safe)
	wantRefused(t, r.check(t, exe), "a sandbox reaches it")
}

// A VM's reach is its shares: its roots, its kept home and its mounts,
// a file's whole directory with it, since a virtio-fs share is a
// directory. No engine is asked anything of it (newBoxApp's fails the
// test on any question under vm): no volume, and no run_args, which no
// VM is created with.
func TestAppPlaceVM(t *testing.T) {
	r := newPlaceRigUnder(t, isolationVM)
	r.base.inspectVolume = func(name string) (docker.Volume, bool, error) {
		t.Errorf("the engine was asked about volume %s for a VM", name)
		return docker.Volume{}, false, errors.New("no engine")
	}
	box := r.base.box
	box.Status, box.SandboxLabels = "exited", noRunArgs
	shared := filepath.Join(r.safe, "shared")
	file := filepath.Join(shared, "settings.json")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	box.SandboxMounts = []backend.Mount{{Source: file, Target: "/home/agent/settings.json"}}
	// Whatever a config or a label might say of run_args, a VM has none.
	r.base.Cfg.RunArgs = []string{"--volume=v:/v", "--privileged"}

	if err := r.check(t, bundle(t, filepath.Join(r.safe, "elsewhere"), appbundle.CheckoutID)); err != nil {
		t.Fatalf("a place no share reaches: %v", err)
	}
	wantRefused(t, r.check(t, bundle(t, filepath.Join(shared, "app"), appbundle.CheckoutID)), "a sandbox reaches it")
	wantRefused(t, r.check(t, bundle(t, filepath.Join(r.root, "app"), appbundle.CheckoutID)), "a sandbox reaches it")

	// A VM's mount is never a volume: one that says so cannot be told.
	box.SandboxMounts = []backend.Mount{{Source: "/x", Target: "/data", Type: "volume", Volume: "v"}}
	wantRefused(t, r.check(t, bundle(t, filepath.Join(r.safe, "elsewhere2"), appbundle.CheckoutID)), "a volume, which no VM's mount is")
}
