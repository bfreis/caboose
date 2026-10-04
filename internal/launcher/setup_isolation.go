package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/config"
)

// Where setup gets runsc -- gVisor's latest release, one directory per
// architecture, as its install guide downloads it -- and how it restarts
// OrbStack's engine: variables so that tests can serve a fake release and
// play the engine.
var (
	runscReleases = "https://storage.googleapis.com/gvisor/releases/release/latest"
	orbCommand    = "orb"
	// engineWait is how long a restarted engine has to list runsc, polled
	// every enginePoll.
	engineWait, enginePoll = 60 * time.Second, time.Second
)

// gvisorInstall is gVisor's own install guide, for an engine caboose does
// not register runsc with.
const gvisorInstall = "https://gvisor.dev/docs/user_guide/install/"

// setupIsolation asks what isolates the sandbox (config.toml's isolation)
// and writes the answer explicitly, the strongest that works as the
// default where nothing is written yet. gvisor works when docker lists
// runsc and, with an image to try, a container runs under it. Where docker
// has no runsc and the engine is OrbStack, it offers to download runsc
// into CABOOSE_HOME and register it in OrbStack's daemon config: shown as
// a difference and asked for, since that file is the user's and the
// engine's restart stops every container. Anything else is said, with how
// to get runsc there. Nothing here fails setup.
func (a *App) setupIsolation(p *prompter) error {
	c := a.Cfg
	p.heading("Isolation", "What stands between the sandbox and this machine: docker's own runtime, "+
		"which shares this machine's kernel, or gVisor, a kernel of its own.")
	if v := c.Getenv("CABOOSE_ISOLATION"); v != "" {
		p.warn("CABOOSE_ISOLATION is set in this shell (%s), and wins over config.toml; unset it to choose here.", v)
		p.same("Nothing changed")
		return nil
	}
	written := ""
	if c.File != nil {
		written = c.File.Vals["ISOLATION"]
	}
	if written != "" {
		p.say("Now: %s.", written)
	} else {
		p.say("Now: %s, the default (config.toml does not say).", isolationDocker)
	}
	have, err := a.engineRuntimes()
	if err != nil {
		p.fail("Docker did not list its runtimes: %s", firstLine(dockerFailed(err).Error()))
		p.same("Nothing changed")
		return nil
	}
	gv := slices.Contains(have, runscName)
	if gv {
		if gv, err = a.refreshRunsc(p); err != nil {
			return err
		}
	} else if gv, err = a.offerRunsc(p); err != nil {
		return err
	}
	if gv {
		gv = a.tryRunsc(p)
	}

	choice := isolationDocker
	switch {
	case gv:
		def := 0
		if written == isolationDocker {
			def = 1
		}
		i, err := p.choose("Isolate the sandbox with", []string{
			"gvisor: a kernel of its own (the stronger)",
			"docker: this machine's kernel, shared",
		}, def)
		if err != nil {
			return err
		}
		if i == 0 {
			choice = isolationGVisor
		}
	case written == isolationGVisor:
		p.warn("isolation is %s, which cannot work here: a launch stops until it does.", isolationGVisor)
		i, err := p.choose("Isolate the sandbox with", []string{
			"docker: this machine's kernel, shared",
			"gvisor, kept for when runsc works",
		}, 0)
		if err != nil {
			return err
		}
		if i == 1 {
			choice = isolationGVisor
		}
	}
	if choice == written {
		p.same("Nothing changed")
	} else {
		if _, err := a.writeConfig(config.Edit{Set: map[string]any{"isolation": choice}}); err != nil {
			return err
		}
		p.ok("Wrote isolation = %q to %s", choice, a.short(filepath.Join(c.EnvDir, config.FileName)))
	}
	if rt := a.runtimeGone(); rt != "" {
		if choice == isolationGVisor && !gv {
			p.warn("The container was created under %s, which docker no longer has, so it cannot start; "+
				"with isolation %s, no new one can be created until docker has runsc. %s registers it, or choose %s here.",
				rt, choice, p.code(SetupCommand(c.Env, "isolation")), isolationDocker)
		} else {
			p.note("The container was created under %s, which docker no longer has, so it cannot start: "+
				"the next launch recreates it with isolation %s (it is stopped, so no sessions are lost).", rt, choice)
		}
		return nil
	}
	if choice == written {
		return nil
	}
	if iso, _, ok := a.createdIsolation(); ok && iso != choice {
		p.warn("The container keeps the isolation it was created with: %s moves it onto %s, and ends running sessions.",
			p.code("caboose restart"), choice)
	}
	return nil
}

// tryRunsc runs a container under runsc, from the image, when there is
// one, and says whether the sandbox would run as the agent or as root; it
// reports whether runsc works, or is taken to, with no image to try.
func (a *App) tryRunsc(p *prompter) bool {
	_, exists, err := a.Docker.ImageLabels(a.Cfg.Image)
	if err != nil || !exists {
		p.ok("Docker has runsc; it is tried when the container is created (there is no image to try it with yet)")
		return true
	}
	writes, err := a.agentCanWrite(runscName)
	switch {
	case err != nil:
		p.fail("Docker has runsc, but a container under it did not run: %s", firstLine(err.Error()))
		return false
	case writes:
		p.ok("A container runs under runsc, as the agent user")
	default:
		p.ok("A container runs under runsc. On this engine the agent user cannot write its mounts under it, " +
			"so the sandbox runs as root inside gVisor (files it writes are still yours here).")
	}
	return true
}

// offerRunsc gets runsc registered where caboose can do it, OrbStack on a
// Mac, and otherwise says how; it reports whether docker now has it.
func (a *App) offerRunsc(p *prompter) (bool, error) {
	engine := ""
	if goos == "darwin" {
		engine = a.macEngine()
	}
	switch {
	case engine == "OrbStack":
		return a.registerOrbStack(p)
	case engine == "Docker Desktop":
		p.note("Docker has no runsc runtime, and caboose cannot register one with Docker Desktop yet: gVisor is not available here.")
	case goos == "linux":
		p.note("Docker has no runsc runtime. To use gVisor, install it as %s says (its 'runsc install' registers it with docker), then run %s again.",
			gvisorInstall, p.code(SetupCommand(a.Cfg.Env, "isolation")))
	default:
		p.note("Docker has no runsc runtime, and caboose cannot register one with this engine: gVisor is not available here.")
	}
	return false, nil
}

// refreshRunsc keeps the runsc caboose registered with OrbStack as this
// caboose would register it, judged by what the engine has loaded (docker
// info), not by docker.json alone: an entry docker.json has lost goes at
// the engine's next restart, and one changed there is not in effect until
// then. Each is said, and fixed only after asking, as registerOrbStack
// does. A runsc registered by anyone else is left as it is. It reports
// whether docker has runsc after, which it does unless the engine did not
// come back.
func (a *App) refreshRunsc(p *prompter) (bool, error) {
	if goos != "darwin" || a.macEngine() != "OrbStack" {
		return true, nil
	}
	loaded, ok, err := a.engineRuntime(runscName)
	if err != nil || !ok || !strings.HasPrefix(loaded.Path, filepath.Join(a.Cfg.CabooseHome, "runsc")+string(filepath.Separator)) {
		return true, nil
	}
	daemon := filepath.Join(a.Cfg.Home, ".orbstack", "config", "docker.json")
	cur, err := os.ReadFile(daemon)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.fail("Cannot read %s: %v", a.short(daemon), err)
		return true, nil
	}
	next, changed, err := withRuntime(cur, runscName, runscEntry(loaded.Path))
	if err != nil {
		p.fail("Cannot edit %s (%v). Check its runsc entry by hand.", a.short(daemon), err)
		return true, nil
	}
	if !changed && slices.Equal(loaded.RuntimeArgs, runscArgs) {
		return true, nil
	}
	var file struct {
		Runtimes map[string]json.RawMessage `json:"runtimes"`
	}
	_ = json.Unmarshal(cur, &file)
	what := orbFlags
	switch _, listed := file.Runtimes[runscName]; {
	case !listed:
		what = orbRegister
		p.say("Docker runs caboose's runsc, but %s no longer registers it: the engine drops it at its next restart, "+
			"and a container under gvisor cannot start without it.", a.short(daemon))
	case changed:
		p.say("The runsc caboose registered in %s runs without flags this caboose gives it. %s", a.short(daemon), dcacheWhy)
	default:
		p.say("Docker runs caboose's runsc without flags this caboose gives it. %s", dcacheWhy)
	}
	if _, err := a.applyOrbStack(p, daemon, cur, next, changed, what); err != nil {
		return false, err
	}
	have, err := a.engineRuntimes()
	return err == nil && slices.Contains(have, runscName), nil
}

// dcacheWhy is what a runsc without runscArgs' --dcache=0 costs.
const dcacheWhy = "Without --dcache=0, a directory on your repos listed while empty can go on listing empty in the sandbox after files are added."

// registerOrbStack downloads runsc for the engine's architecture into
// CABOOSE_HOME/runsc, adds it to OrbStack's docker.json (after showing the
// difference and asking; the file as it was is kept beside it), and
// restarts OrbStack's engine. OrbStack's VM sees the Mac's /Users at the
// same path, so the engine runs the binary where it was downloaded.
func (a *App) registerOrbStack(p *prompter) (bool, error) {
	c := a.Cfg
	daemon := filepath.Join(c.Home, ".orbstack", "config", "docker.json")
	arch, err := a.Docker.Output("info", "--format", "{{.Architecture}}")
	if err != nil {
		p.fail("Docker did not say its architecture: %s", firstLine(dockerFailed(err).Error()))
		return false, nil
	}
	garch := runscArch(arch)
	if garch == "" {
		p.note("Docker has no runsc runtime, and gVisor has no build for this engine's architecture (%s).", arch)
		return false, nil
	}
	dir := filepath.Join(c.CabooseHome, "runsc", garch)
	p.say("Docker has no runsc runtime. caboose can download gVisor into %s and register its runsc with OrbStack's Docker engine, in %s.",
		a.short(dir), a.short(daemon))
	ok, err := p.yesNo("Download gVisor (some 150MB)?", true)
	if err != nil {
		return false, err
	}
	if !ok {
		p.same("Not downloaded")
		return false, nil
	}
	p.note("Downloading gVisor (%s) from %s...", garch, runscReleases)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	bin, err := downloadRunsc(ctx, nil, runscReleases, garch, dir)
	if err != nil {
		p.fail("Not downloaded: %v", err)
		return false, nil
	}
	p.ok("Downloaded gVisor into %s, its checksum verified", a.short(dir))

	cur, err := os.ReadFile(daemon)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.fail("Cannot read %s: %v", a.short(daemon), err)
		return false, nil
	}
	next, changed, err := withRuntime(cur, runscName, runscEntry(bin))
	if err != nil {
		p.fail("Cannot edit %s (%v). Register runsc there by hand, then restart OrbStack's Docker engine.", a.short(daemon), err)
		return false, nil
	}
	return a.applyOrbStack(p, daemon, cur, next, changed, orbRegister)
}

// orbChange is what applyOrbStack says of a docker.json change.
type orbChange struct {
	diff, done, notDone string
}

var (
	orbRegister = orbChange{"with runsc", "Registered runsc", "Not registered"}
	orbFlags    = orbChange{"with runsc's flags updated", "Updated runsc's flags", "Not updated"}
)

// applyOrbStack writes next over OrbStack's docker.json at daemon, which
// holds cur, after showing the difference and asking (unless changed is
// false: next is already there, and only the engine has not loaded it),
// then restarts OrbStack's engine and waits for it to list runsc. It
// reports whether docker has runsc then.
func (a *App) applyOrbStack(p *prompter, daemon string, cur, next []byte, changed bool, what orbChange) (bool, error) {
	c := a.Cfg
	running := a.state() == "running"
	stops := "That stops every running container."
	if running {
		stops = "That stops every running container, this sandbox's too, ending its sessions."
	}
	var ok bool
	var err error
	if changed {
		p.blank()
		p.diff(a.short(daemon), what.diff, lineDiff(string(cur), string(next)))
		p.blank()
		ok, err = p.yesNo("Write it, and restart OrbStack's Docker engine? "+stops, !running)
	} else {
		p.say("%s registers this runsc already; the engine has not loaded it.", a.short(daemon))
		ok, err = p.yesNo("Restart OrbStack's Docker engine? "+stops, !running)
	}
	if err != nil {
		return false, err
	}
	if !ok {
		p.same("%s; %s offers it again", what.notDone, p.code(SetupCommand(c.Env, "isolation")))
		return false, nil
	}
	if changed {
		backup, err := writeDaemonConfig(daemon, cur, next)
		if err != nil {
			p.fail("%s: %v", what.notDone, err)
			return false, nil
		}
		if backup != "" {
			p.ok("%s in %s (as it was: %s)", what.done, a.short(daemon), a.short(backup))
		} else {
			p.ok("%s in %s", what.done, a.short(daemon))
		}
	}
	p.note("Restarting OrbStack's Docker engine...")
	if out, err := exec.Command(orbCommand, "restart", "docker").CombinedOutput(); err != nil {
		p.fail("'%s restart docker' failed: %s", orbCommand, firstLine(or(string(out), err.Error())))
		p.warn("Restart Docker from OrbStack's menu, then run %s again.", p.code(SetupCommand(c.Env, "isolation")))
		return false, nil
	}
	for end := time.Now().Add(engineWait); ; time.Sleep(enginePoll) {
		if have, err := a.engineRuntimes(); err == nil && slices.Contains(have, runscName) {
			p.ok("OrbStack's Docker engine restarted, with runsc")
			return true, nil
		}
		if time.Now().After(end) {
			break
		}
	}
	p.fail("OrbStack's Docker engine did not list runsc within %s of its restart.", engineWait)
	return false, nil
}

// writeDaemonConfig replaces the engine's daemon config at path, which
// held cur (nil: none), with next: by rename, keeping its mode, through a
// symlink to the file it names. The file as it was is kept once, at
// path.before-caboose, the first time caboose changes it; that path is
// returned, or "" when there was no file to keep.
func writeDaemonConfig(path string, cur, next []byte) (backup string, err error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := os.FileMode(0o644)
	if cur != nil {
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
		backup = path + ".before-caboose"
		f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		switch {
		case err == nil:
			_, err = f.Write(cur)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", fmt.Errorf("keeping %s: %v", backup, err)
			}
		case !errors.Is(err, fs.ErrExist):
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(next)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), mode)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return "", fmt.Errorf("writing %s: %v", path, err)
	}
	return backup, nil
}
