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
		"which shares this machine's kernel; gVisor, a kernel of its own; or, on a Mac, a VM of caboose's own.")
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
	// vm needs no engine: its files beside the launcher are the question.
	vf, vmErr := a.findVMFiles()
	vmOK := vmErr == nil || a.vmFetchable(vf)
	// Its files being here is not enough: caboose-vmm says whether this
	// Mac, and its own signature, let it run a VM.
	if vmOK {
		if v := a.checkVMM(vf.VMM); v.Problem != "" {
			vmOK, vmErr = false, fmt.Errorf("%s; to fix it: %s", v.Problem, v.Fix)
			if written != isolationVM {
				p.note("vm is not offered: %v", vmErr)
			}
		}
	}
	gv := false
	have, err := a.engineRuntimes()
	switch {
	case err != nil && !vmOK:
		p.fail("Docker did not list its runtimes: %s", firstLine(dockerFailed(err).Error()))
		p.same("Nothing changed")
		return nil
	case err != nil:
		p.note("Docker did not list its runtimes, so gVisor is not offered: %s", firstLine(dockerFailed(err).Error()))
	default:
		gv = slices.Contains(have, runscName)
		if gv {
			if gv, err = a.refreshRunsc(p); err != nil {
				return err
			}
			a.checkRunscFlags(p)
		} else if gv, err = a.offerRunsc(p); err != nil {
			return err
		}
		if gv {
			gv = a.tryRunsc(p)
		}
	}
	if vmOK && vmErr != nil {
		p.ok("This Mac can run the sandbox in a VM of caboose's own (its kernel and builder are fetched from this caboose's release on first use)")
	} else if vmOK {
		p.ok("This Mac can run the sandbox in a VM of caboose's own (caboose-vmm, the kernel and the builder are here)")
	} else if written == isolationVM {
		p.warn("isolation is %s, which cannot work here: %v", isolationVM, vmErr)
	}

	// What works, the strongest first, then docker, then what is written
	// but cannot work now, kept offered. The default is what is written
	// when it works, else the strongest that does.
	type option struct{ iso, text string }
	var opts, kept []option
	switch {
	case vmOK:
		opts = append(opts, option{isolationVM, "vm: a VM of caboose's own, with no Docker (the strongest)"})
	case written == isolationVM:
		kept = append(kept, option{isolationVM, "vm, kept for when its files are here"})
	}
	switch {
	case gv:
		opts = append(opts, option{isolationGVisor, "gvisor: a kernel of its own (the stronger)"})
	case written == isolationGVisor:
		p.warn("isolation is %s, which cannot work here: a launch stops until it does.", isolationGVisor)
		kept = append(kept, option{isolationGVisor, "gvisor, kept for when runsc works"})
	}
	opts = append(opts, option{isolationDocker, "docker: this machine's kernel, shared"})
	works := len(opts)
	opts = append(opts, kept...)
	choice := isolationDocker
	if len(opts) > 1 {
		def := 0
		if len(kept) > 0 {
			def = works - 1
		}
		var texts []string
		for i, o := range opts {
			texts = append(texts, o.text)
			if o.iso == written && i < works {
				def = i
			}
		}
		i, err := p.choose("Isolate the sandbox with", texts, def)
		if err != nil {
			return err
		}
		choice = opts[i].iso
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
		p.warn("The sandbox keeps the isolation it was created with: %s moves it onto %s, and ends running sessions.",
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
		p.note("Docker has no runsc runtime. To use gVisor, install it as %s says, registering it with docker as %s "+
			"(the first flag lets the sandbox reach the SSH agent caboose forwards; the others, run Docker inside it), "+
			"then 'sudo systemctl reload docker' and run %s again.",
			gvisorInstall, p.code("sudo runsc install -- "+strings.Join(runscInstallArgs, " ")), p.code(SetupCommand(a.Cfg.Env, "isolation")))
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
	if err != nil || !ok {
		return true, nil
	}
	dir := a.cabooseRunscDir(loaded.Path)
	if dir == "" {
		return true, nil
	}
	if err := a.offerRunscUpdate(p, dir); err != nil {
		return false, err
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

// checkRunscFlags says what to do when docker's runsc, registered by
// anyone but caboose (gVisor's own install on Linux, say), runs without
// --host-uds=open: the forwarded SSH agent is a host Unix socket, which
// gVisor refuses otherwise. That runsc is not caboose's to change, so it
// is only said; caboose's own is refreshRunsc's.
func (a *App) checkRunscFlags(p *prompter) {
	loaded, ok, err := a.engineRuntime(runscName)
	if err != nil || !ok || a.cabooseRunscDir(loaded.Path) != "" || runscHostUDS(loaded.RuntimeArgs) {
		return
	}
	p.warn("Docker's runsc runs without --host-uds=open, so under gvisor the sandbox cannot reach the SSH agent caboose forwards. To fix it, %s.",
		hostUDSFix())
}

// offerRunscUpdate checks the gVisor release caboose downloaded into dir
// against gVisor's latest, by the checksum published beside it, and
// downloads the latest over it when asked: checked as the first download
// is, and swapped in whole, so a failure leaves the old one. The path and
// the engine's runtimes entry stay as they are, so the engine needs no
// restart; a container already running under runsc goes on in the old
// release until it is recreated, which is said. Not being able to tell is
// said too, and fails nothing.
func (a *App) offerRunscUpdate(p *prompter, dir string) error {
	c := a.Cfg
	arch := filepath.Base(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	latest, err := latestRunscSum(ctx, nil, runscReleases, arch)
	cancel()
	if err != nil {
		p.note("Could not tell whether gVisor has a newer release than the one in %s: %v", a.short(dir), err)
		return nil
	}
	rec, known := readRunscRelease(dir)
	switch {
	case known && rec.SHA512 == latest:
		p.ok("gVisor in %s is its latest release (downloaded %s)", a.short(dir), rec.Downloaded.Local().Format(time.DateOnly))
		return nil
	case known:
		p.say("gVisor has a newer release than the one caboose downloaded into %s on %s.",
			a.short(dir), rec.Downloaded.Local().Format(time.DateOnly))
	default:
		p.say("caboose cannot tell which gVisor release is in %s (it was downloaded before caboose recorded that), "+
			"and gVisor's latest may be newer.", a.short(dir))
	}
	iso, _, _ := a.createdIsolation()
	running := a.state() == "running" && iso == isolationGVisor
	if running {
		p.say("The sandbox is running under it: it goes on in the old release until %s, which ends its sessions, "+
			"and docker drives it with the new runsc meanwhile.", p.code("caboose restart"))
	}
	ok, err := p.yesNo("Download gVisor's latest release (some 150MB) in its place?", !running)
	if err != nil {
		return err
	}
	if !ok {
		p.same("Not updated; %s offers it again", p.code(SetupCommand(c.Env, "isolation")))
		return nil
	}
	p.note("Downloading gVisor (%s) from %s...", arch, runscReleases)
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := downloadRunsc(ctx, nil, runscReleases, arch, dir); err != nil {
		p.fail("Not updated, the old release kept: %v", err)
		return nil
	}
	p.ok("Updated gVisor in %s, its checksum verified", a.short(dir))
	if running {
		p.warn("The sandbox runs on in the release it started with: %s moves it onto the new one, and ends its sessions.",
			p.code("caboose restart"))
	}
	return nil
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
