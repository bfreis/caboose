package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/policy"
)

// checkAppPlace refuses exe, the menu-bar app's executable, when a sandbox
// reaches it, by name or by identity, or a symlink on its way, or reaches
// into its bundle, by name or by the identity of any entry in it
// (policy.CheckAppPlace on what hostlink.AppPlaceOf finds, against
// appReach): the host runs the app, and at every login once it is a login
// item, so a sandbox must not be able to replace it. Nil is allowed.
func (a *App) checkAppPlace(ctx context.Context, exe string) error {
	pl := hostlink.AppPlaceOf(exe)
	if pl.Bundle = appbundle.BundleOf(exe); pl.Bundle != "" {
		pl.BundleEntries, pl.BundleErr = hostlink.BundleEntriesOf(pl.Bundle)
	}
	base, err := a.appEnvBase()
	if err != nil {
		return policy.CheckAppPlace(pl, policy.AppReach{Err: err})
	}
	return policy.CheckAppPlace(pl, base.appReach(ctx, a.appEnvApps))
}

// appEnvBase is the App appReach runs on: this machine's default
// environment, loaded (envApp), with docker found on the login shell's
// PATH when the app's own has none (started from Finder or at login,
// the app has the system's minimal PATH, as the daemon does). The tests
// stand in for it with appBase.
func (a *App) appEnvBase() (*App, error) {
	if a.appBase != nil {
		return a.appBase()
	}
	if _, err := exec.LookPath("docker"); err != nil {
		captureLoginEnv(context.Background())
	}
	return a.envApp(a.Cfg.Env)
}

// appReach is every path a sandbox under CABOOSE_HOME reaches, or will at
// its next start, for policy.CheckAppPlace: checkpointReach's (every
// environment's roots, read-only or not, its kept home and every mount of
// its sandbox; it fails rather than leave an environment out), every host
// path its run_args mounts (runArgsReach), and, when
// a daemon answers, what it says the sandboxes write and read
// (sandboxReach, on writableOf), physical where they resolve, with the
// identity of each that exists. Anything that cannot be told is Err,
// which refuses every place; a setting that hands a sandbox everything
// (runArgsReach's) is Everything, which refuses every place too. apps
// makes another environment's App (nil: envApp).
func (a *App) appReach(ctx context.Context, apps func(env string) (*App, error)) policy.AppReach {
	reach, err := a.checkpointReach(apps)
	if err != nil {
		return policy.AppReach{Err: err}
	}
	var paths []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, s := range []string{p, physicalOr(p)} {
			if !slices.Contains(paths, s) {
				paths = append(paths, s)
			}
		}
	}
	for _, r := range reach {
		add(r.Path)
	}
	// What run_args mounts at the next start, whether or not the engine
	// answers now about the sandbox it made.
	rpaths, everything, err := a.runArgsReach(apps)
	if err != nil {
		return policy.AppReach{Err: err}
	}
	if everything != "" {
		return policy.AppReach{Everything: everything}
	}
	for _, p := range rpaths {
		add(p)
	}
	dpaths, err := a.daemonReach(ctx)
	if err != nil {
		return policy.AppReach{Err: err}
	}
	for _, p := range dpaths {
		add(p)
	}
	var ids []policy.FileID
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			if id, ok := policy.IDOf(fi); ok && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return policy.AppReach{Paths: paths, IDs: ids}
}

// daemonReach is what a running daemon says the sandboxes write and read:
// none when no daemon answers (it is never started for this), an error
// when one answers but cannot say.
func (a *App) daemonReach(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonWritableBudget)
	defer cancel()
	c, err := a.dialDaemon(ctx, false)
	if err != nil {
		return nil, nil
	}
	defer c.Close()
	wc, ok := c.(writableCaller)
	if !ok {
		return nil, nil
	}
	r, err := wc.Writable(ctx)
	if err == nil {
		err = r.Validate()
	}
	if err != nil {
		return nil, fmt.Errorf("the daemon cannot say what the sandboxes reach: %w", err)
	}
	var out []string
	for _, p := range append(slices.Clone(r.Writable), r.ReadOnly...) {
		out = append(out, p.Path)
	}
	return out, nil
}

// runArgsReach is every host path the run_args of an environment under
// CABOOSE_HOME (envsWithDir, as checkpointReach lists them) bind-mount
// (runArgsSources), the host paths the named volumes they and the
// sandbox made mount bind, as its engine says (volumeReach), and
// everything, which names the first setting found
// that hands a sandbox what no list of paths holds: engine_socket on, a
// mount (of run_args or of the sandbox made) at or above a place an
// engine's socket is kept (engineSocketPlaces), a run_args flag
// runArgsEverything names, or a socket run_args mounts (whose other end
// may be an engine's: it cannot be told which). The run_args judged are
// the configuration's and, for a sandbox that exists, those it was
// created with (assets.LabelRunArgs). An environment whose configuration
// does not load, whose run_args or sandbox mount what cannot be told
// (a volume included: volumeReach), or whose sandbox cannot say what
// run_args it was created with, is an error. A VM has no run_args and
// no engine's volumes: only its roots and mounts are judged, and no
// engine is asked about it.
func (a *App) runArgsReach(apps func(env string) (*App, error)) (paths []string, everything string, err error) {
	if apps == nil {
		apps = a.envApp
	}
	names, err := a.envsWithDir()
	if err != nil {
		return nil, "", fmt.Errorf("listing the environments: %v", err)
	}
	for _, n := range names {
		b := a
		if n != a.Cfg.Env {
			if b, err = apps(n); err != nil {
				return nil, "", fmt.Errorf("environment %s: %v", n, err)
			}
		}
		if b.Cfg.EngineSocket {
			return nil, fmt.Sprintf("%s of environment %s, which hands it the engine", b.Cfg.ProfileKey("engine_socket"), n), nil
		}
		places := b.engineSocketPlaces()
		// Every root, read-only or not, before the sandbox exists: a
		// read-only mount still reaches a socket.
		for _, r := range b.Cfg.Roots {
			if sock := holdsAny(r.Host, places); sock != "" {
				return nil, fmt.Sprintf("root %s of environment %s, %s, which holds %s, where an engine's socket is kept", r.Container, n, r.Host, sock), nil
			}
		}
		if mounts, err := sandboxMounts(b.box()); err == nil {
			for _, m := range mounts {
				switch m.Type {
				case "", "bind", "tmpfs", "image":
				case "volume":
					if b.isVM() {
						// A VM's mounts are shares, never an engine's
						// volumes: one that says otherwise cannot be told.
						return nil, "", fmt.Errorf("environment %s's VM mounts at %s a volume, which no VM's mount is", n, m.Target)
					}
					dev, err := b.volumeReach(m.Volume, true)
					if err != nil {
						return nil, "", fmt.Errorf("environment %s's sandbox mounts at %s: %v", n, m.Target, err)
					}
					if dev != "" {
						if why := socketReach(dev, fmt.Sprintf("which environment %s's sandbox mounts at %s as volume %s", n, m.Target, m.Volume), places); why != "" {
							return nil, why, nil
						}
						paths = append(paths, dev)
					}
				default:
					return nil, "", fmt.Errorf("environment %s's sandbox mounts at %s a mount of type %q, whose reach cannot be told", n, m.Target, m.Type)
				}
				if m.Target == engineSocketTarget {
					return nil, fmt.Sprintf("the engine's socket, which environment %s's sandbox mounts at %s (engine_socket when it was created; 'caboose restart' undoes it)", n, m.Target), nil
				}
				if sock := holdsAny(m.Source, places); sock != "" {
					return nil, fmt.Sprintf("%s, which environment %s's sandbox mounts at %s and which holds %s, where an engine's socket is kept", m.Source, n, m.Target, sock), nil
				}
			}
		}
		if b.isVM() {
			// run_args is no vm profile's key, and a VM is created with
			// none (createContainer): its reach is its shares, above and
			// in checkpointReach, and no engine is asked about it.
			continue
		}
		sets := []struct {
			args  []string
			where string
		}{{b.Cfg.RunArgs, b.Cfg.ProfileKey("run_args")}}
		created, err := createdRunArgsOf(b)
		if err != nil {
			return nil, "", fmt.Errorf("environment %s's sandbox: %v", n, err)
		}
		if created != nil {
			sets = append(sets, struct {
				args  []string
				where string
			}{created, "the run_args its sandbox was created with ('caboose restart' recreates it)"})
		}
		for _, set := range sets {
			if arg := runArgsEverything(set.args); arg != "" {
				return nil, fmt.Sprintf("%s in %s of environment %s", arg, set.where, n), nil
			}
			src, vols, err := runArgsSources(set.args)
			if err != nil {
				return nil, "", fmt.Errorf("environment %s's %s: %v", n, set.where, err)
			}
			for _, p := range src {
				if why := socketReach(p, fmt.Sprintf("which %s of environment %s mounts", set.where, n), places); why != "" {
					return nil, why, nil
				}
			}
			paths = append(paths, src...)
			for _, v := range vols {
				dev, err := b.volumeReach(v, false)
				if err != nil {
					return nil, "", fmt.Errorf("environment %s's %s: %v", n, set.where, err)
				}
				if dev == "" {
					continue
				}
				if why := socketReach(dev, fmt.Sprintf("which %s of environment %s mounts as volume %s", set.where, n, v), places); why != "" {
					return nil, why, nil
				}
				paths = append(paths, dev)
			}
		}
	}
	return paths, "", nil
}

// socketReach says how p, a host path a sandbox mounts (what: "which
// ... mounts"), hands it an engine: a socket, whose other end may be an
// engine's, or a path at or above a place an engine's socket is kept
// (holdsAny); "" for neither.
func socketReach(p, what string, places []string) string {
	if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return fmt.Sprintf("the socket %s, %s, and which may be an engine's", p, what)
	}
	if sock := holdsAny(p, places); sock != "" {
		return fmt.Sprintf("%s, %s and which holds %s, where an engine's socket is kept", p, what, sock)
	}
	return ""
}

// createdRunArgsOf is the run_args b's sandbox was created with: nil
// when it does not exist, and an error when it exists but its label
// (assets.LabelRunArgs) is missing or cannot be read, since what it was
// given cannot be told then. An empty label is no run_args.
func createdRunArgsOf(b *App) ([]string, error) {
	box := b.box()
	if box.State() == "absent" {
		return nil, nil
	}
	labels, err := box.Labels()
	if err != nil {
		return nil, fmt.Errorf("its labels cannot be read: %v", err)
	}
	v, ok := labels[assets.LabelRunArgs]
	if !ok {
		return nil, fmt.Errorf("it does not say what run_args it was created with")
	}
	args := []string{}
	if v != "" {
		if err := json.Unmarshal([]byte(v), &args); err != nil {
			return nil, fmt.Errorf("the run_args it was created with cannot be read: %v", err)
		}
	}
	return args, nil
}

// engineSocketPlaces are where an engine's socket is kept on this
// machine, as named and physical: DOCKER_HOST's, the system's, and those
// of Docker Desktop, OrbStack, Rancher Desktop and Podman under the
// home; and the directories under the home where any number of them may
// be (~/.docker for contexts' sockets, every Colima profile, every Lima
// instance), which end in "/" (socketDir). A mount at or above a socket
// hands the sandbox the engine, and so does one at, above or within such
// a directory (holdsAny).
func (a *App) engineSocketPlaces() []string {
	places := []string{DockerSockPath(true, a.getenv("DOCKER_HOST")), "/var/run/docker.sock", "/run/docker.sock",
		"/var/run/containerd/containerd.sock", "/run/containerd/containerd.sock", "/run/podman/podman.sock"}
	if home := a.getenv("HOME"); path.IsAbs(home) {
		for _, rel := range []string{".orbstack/run/docker.sock", ".rd/docker.sock", ".local/share/containers/podman/machine/podman.sock"} {
			places = append(places, path.Join(home, rel))
		}
		for _, rel := range []string{".docker", ".colima", ".lima"} {
			places = append(places, path.Join(home, rel)+socketDir)
		}
	}
	if x := a.getenv("XDG_RUNTIME_DIR"); path.IsAbs(x) {
		places = append(places, path.Join(x, "docker.sock"), path.Join(x, "podman", "podman.sock"))
	}
	var out []string
	for _, p := range places {
		dir := strings.HasSuffix(p, socketDir)
		p = strings.TrimSuffix(p, socketDir)
		for _, s := range []string{p, physicalOr(p)} {
			if dir && s != "" {
				s += socketDir
			}
			if s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// socketDir ends an engineSocketPlaces entry that is a directory where
// sockets are kept, not a socket.
const socketDir = "/"

// holdsAny is the first of places (engineSocketPlaces) that src, a
// mount's source, is or holds, or for a directory of sockets is also
// within, as named or physical; "" for none.
func holdsAny(src string, places []string) string {
	if !path.IsAbs(src) {
		return ""
	}
	for _, s := range []string{path.Clean(src), physicalOr(src)} {
		if s == "" {
			continue
		}
		for _, p := range places {
			if d, ok := strings.CutSuffix(p, socketDir); ok && d != "" {
				if config.Within(d, s) || config.Within(s, d) {
					return d
				}
			} else if config.Within(p, s) {
				return p
			}
		}
	}
	return ""
}

// runArgsEverything is the first of args, the user's docker run
// arguments, that gives a sandbox what no list of paths holds, or "": a
// privileged container, an added capability, a device, a loosened
// security option (any but no-new-privileges), the host's PID, IPC,
// user, cgroup or network namespace (the host's network holds its
// abstract Unix sockets: X11's, D-Bus's), another container's PID, IPC
// or network namespace, or the engine's API socket (--use-api-socket).
// A mount is runArgsSources'.
func runArgsEverything(args []string) string {
	for _, arg := range args {
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		switch name {
		case "privileged", "use-api-socket", "cap-add", "device", "device-cgroup-rule", "gpus":
			return arg
		case "security-opt":
			if v := strings.ToLower(strings.TrimSpace(value)); v != "no-new-privileges" && v != "no-new-privileges=true" && v != "no-new-privileges:true" {
				return arg
			}
		case "pid", "ipc", "userns", "cgroupns", "network", "net":
			v := strings.ToLower(strings.TrimSpace(value))
			if v == "host" || (name != "userns" && name != "cgroupns" && strings.HasPrefix(v, "container:")) {
				return arg
			}
		}
	}
	return ""
}

// runArgsSources are the host paths run_args (each --flag=value, as
// checkRunArgs holds them) bind-mount, a --volume's or a --mount's
// absolute source, and the named volumes they mount, whose host paths
// only the engine can say (volumeReach). An anonymous volume, which
// docker creates at the start with the local driver and no options, is
// the engine's own storage, no host path of ours. What cannot be told
// from here is an error: a relative source, which docker reads against
// the directory it runs in, a bind's source that is not absolute, a
// volume's name that is no volume's, a --mount docker would not read or
// that names a volume's driver or options (volume-driver, volume-opt:
// the local driver binds any path of the engine's host),
// --volume-driver, and --volumes-from, which takes what another
// container mounts.
func runArgsSources(args []string) (paths, volumes []string, err error) {
	for _, arg := range args {
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		var src string
		named := false
		switch name {
		case "volume":
			if parts := strings.Split(value, ":"); len(parts) > 1 {
				src, named = parts[0], true
			}
		case "mount":
			m, err := mountFields(value)
			if err != nil {
				return nil, nil, err
			}
			if m.volumeOpts {
				return nil, nil, fmt.Errorf("%s: a volume's driver or its options, with which it can be any directory of the engine's host", arg)
			}
			src = m.source
			switch m.typ {
			case "", "volume":
				named = true
			case "bind":
				if !path.IsAbs(src) && !strings.HasPrefix(src, ".") {
					return nil, nil, fmt.Errorf("%s: a bind whose source is not an absolute path", arg)
				}
			}
		case "volumes-from":
			return nil, nil, fmt.Errorf("--volumes-from mounts what another container does")
		case "volume-driver":
			return nil, nil, fmt.Errorf("%s: a volume's driver, with which a volume can be any directory of the engine's host", arg)
		}
		switch {
		case strings.HasPrefix(src, "."):
			// docker's CLI makes it absolute against the directory it
			// runs in, which is not known here.
			return nil, nil, fmt.Errorf("%s: a relative source, which docker reads against the directory it runs in", arg)
		case path.IsAbs(src):
			paths = append(paths, path.Clean(src))
		case named && src != "":
			if !docker.ValidVolumeName(src) {
				return nil, nil, fmt.Errorf("%s: %q, which is no volume's name", arg, src)
			}
			if !slices.Contains(volumes, src) {
				volumes = append(volumes, src)
			}
		}
	}
	return paths, volumes, nil
}

// volumeReach is the host path the volume name reaches, as b's engine
// says it (inspectVolume): "" for the engine's own storage, a local
// volume with no options (or only a size), or a tmpfs; the device of a
// local volume that binds it (o holding bind or rbind, or type none),
// absolute and cleaned. A volume the engine has not is "" unless
// mustExist: one run_args names is created at the sandbox's start, by
// docker, with the local driver and no options. Anything else is an
// error, which fails the check closed: an engine that does not answer
// or cannot say, a volume a sandbox mounts that is gone, another driver,
// whose volumes may be anywhere, a device that is not absolute, an
// option the local driver is not known to take, or a type of mount
// (nfs, cifs, a block device, overlay with its directories in o) whose
// reach cannot be told.
func (b *App) volumeReach(name string, mustExist bool) (string, error) {
	inspect := b.inspectVolume
	if inspect == nil {
		if b.Docker == nil {
			return "", fmt.Errorf("volume %s: no engine to ask what it is", name)
		}
		inspect = b.Docker.InspectVolume
	}
	v, exists, err := inspect(name)
	if err != nil {
		return "", fmt.Errorf("volume %s: %v", name, err)
	}
	if !exists {
		if mustExist {
			return "", fmt.Errorf("volume %s, which a sandbox mounts, is not the engine's: what it held cannot be told", name)
		}
		return "", nil
	}
	return volumeDevice(name, v)
}

// volumeDevice is volumeReach's judgement of what the engine said of
// the volume name.
func volumeDevice(name string, v docker.Volume) (string, error) {
	if v.Driver != "local" {
		return "", fmt.Errorf("volume %s has the driver %q, whose volumes may be any directory", name, v.Driver)
	}
	var typ, o, device string
	for k, val := range v.Options {
		switch k {
		case "type":
			typ = strings.ToLower(strings.TrimSpace(val))
		case "o":
			o = val
		case "device":
			device = val
		case "size":
		default:
			return "", fmt.Errorf("volume %s has the option %q, which the local driver is not known to take", name, k)
		}
	}
	bind := typ == "none"
	for _, f := range strings.Split(o, ",") {
		if f = strings.ToLower(strings.TrimSpace(f)); f == "bind" || f == "rbind" {
			bind = true
		}
	}
	switch {
	case bind:
		device = docker.HostSource(device)
		if !path.IsAbs(device) {
			return "", fmt.Errorf("volume %s binds %q, which is not an absolute path", name, device)
		}
		return path.Clean(device), nil
	case typ == "" && o == "" && device == "":
		return "", nil
	case typ == "tmpfs":
		return "", nil
	}
	return "", fmt.Errorf("volume %s mounts %q of type %q with %q, whose reach cannot be told", name, device, typ, o)
}
