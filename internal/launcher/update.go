package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/version"
)

// caboose keeps itself current, as Claude Code does: an install made by
// install.sh (selfupdate.Layout) checks for a newer release at most once a
// day, in a process of its own that a launch never waits for, and the next
// run is the new version. Nothing else updates itself: a build from a
// checkout, go install's, or one a package manager installed.

// updateEvery is how often a launch starts a check.
const updateEvery = 24 * time.Hour

// updateBudget bounds a whole update, the download included.
const updateBudget = 5 * time.Minute

// installScriptURL is the one-line install.
const installScriptURL = selfupdate.DefaultReleases + "/latest/download/install.sh"

// currentVersion is this launcher's version; a variable for the tests.
var currentVersion = func() string { return version.Get().Version }

// installKind is how this caboose was installed.
type installKind int

const (
	installDev     installKind = iota // not a release: a checkout's build, or go's
	installOther                      // a release, but not where install.sh puts one
	installManaged                    // install.sh's, which updates itself
)

// install is where this caboose runs from, and what that means for
// updating it.
type install struct {
	kind installKind
	exe  string // the running executable, symlinks resolved when they can be
}

func (a *App) layout() selfupdate.Layout { return selfupdate.DefaultLayout(a.Cfg.Home) }

func (a *App) releases() selfupdate.Source {
	return selfupdate.Source{Base: or(a.getenv("CABOOSE_RELEASES_URL"), selfupdate.DefaultReleases)}
}

func (a *App) executable() (string, error) {
	if a.Executable != nil {
		return a.Executable()
	}
	return os.Executable()
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// installed says how this caboose was installed. A release build is one
// whose version is a release tag, and which is not next to a checkout (a
// make build of a tagged commit is stamped with the tag).
func (a *App) installed() install {
	exe, err := a.executable()
	if err != nil {
		return install{kind: installDev}
	}
	in := install{exe: exe}
	switch {
	case !release(currentVersion()) || a.Checkout != "":
		in.kind = installDev
	case a.Cfg.Home == "":
		in.kind = installOther
	default:
		if _, ok := a.layout().Managed(exe); ok {
			in.kind = installManaged
		} else {
			in.kind = installOther
		}
	}
	return in
}

// describe is what `git describe --dirty` adds to a tag between tags, or on
// an edited tree: what make stamps into a build that is no release.
var describe = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+(-dirty)?$|-dirty$`)

// release reports whether v is a release's version: a tag, and not one
// git describe made up from the last one.
func release(v string) bool { return selfupdate.Valid(v) && !describe.MatchString(v) }

// autoUpdateOff reports whether CABOOSE_NO_AUTO_UPDATE turns automatic
// updates off: set to anything but "" or "0". Machine-wide, not a key in an
// environment's config.toml: the binary serves every environment.
func (a *App) autoUpdateOff() bool {
	v := a.getenv("CABOOSE_NO_AUTO_UPDATE")
	return v != "" && v != "0"
}

// AutoUpdate runs before every command but update and help. It says, once,
// that caboose was updated since the last run; and on an install that
// updates itself, at most once every updateEvery, it starts `caboose update
// --background` and goes on without waiting for it. Nothing here can fail
// a command: the state file is best effort.
func (a *App) AutoUpdate() {
	home := a.Cfg.CabooseHome
	if home == "" {
		return
	}
	st := selfupdate.ReadState(home)
	if st.To != "" && !st.Announced && st.To == currentVersion() {
		a.Note("updated to %s (from %s)", st.To, st.From)
		st.Announced = true
		_ = selfupdate.WriteState(home, st)
	}
	in := a.installed()
	if in.kind != installManaged || a.autoUpdateOff() {
		return
	}
	now := a.now()
	if since := now.Sub(st.Attempted); since >= 0 && since < updateEvery {
		return
	}
	st.Attempted = now
	if selfupdate.WriteState(home, st) != nil {
		return // no state, no telling when the last one was: not every run
	}
	_ = a.spawn(in.exe, "update", "--background")
}

// spawn starts exe with args in a session of its own, with no terminal and
// nothing to write to, and leaves it: it outlives the command that started
// it, and a launch handing the terminal to tmux.
func (a *App) spawn(exe string, args ...string) error {
	if a.Spawn != nil {
		return a.Spawn(exe, args...)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Update is `caboose update`: install the latest release now, if it is
// newer, saying what it does. With --background (what AutoUpdate starts)
// it says nothing and records what happened in the state file instead.
func (a *App) Update(args []string) error {
	background := false
	for _, arg := range args {
		if arg != "--background" {
			return Die("usage: caboose update (got %q)", arg)
		}
		background = true
	}
	if background {
		a.updateInBackground()
		return nil
	}
	return a.updateNow()
}

// updateInBackground is one automatic update, silent: its outcome goes into
// the state file, for doctor and the next run.
func (a *App) updateInBackground() {
	home, in := a.Cfg.CabooseHome, a.installed()
	if home == "" || in.kind != installManaged {
		return
	}
	unlock, err := selfupdate.Lock(home)
	if err != nil {
		return // another one is at it
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), updateBudget)
	defer cancel()
	cur := currentVersion()
	tag, err := a.releases().Latest(ctx)
	st := selfupdate.ReadState(home)
	st.Checked, st.Error = a.now(), ""
	switch {
	case err != nil:
		st.Error = err.Error()
	default:
		st.Latest = tag
		if selfupdate.Compare(tag, cur) > 0 {
			if err := a.layout().Install(ctx, a.releases(), tag, runtime.GOOS, runtime.GOARCH, in.exe); err != nil {
				st.Error = err.Error()
			} else {
				st.From, st.To, st.Announced = cur, tag, false
			}
		}
	}
	_ = selfupdate.WriteState(home, st)
}

// updateNow is `caboose update`, in the foreground.
func (a *App) updateNow() error {
	in, cur := a.installed(), currentVersion()
	switch in.kind {
	case installDev:
		from := "a checkout"
		if a.Checkout != "" {
			from = a.Checkout
		}
		return Die("this caboose (%s) is a development build: update %s instead (git pull, then make)", cur, from)
	case installOther:
		return Die("this caboose (%s, at %s) was not installed by install.sh, so it does not update itself;\n"+
			"       an install that does: curl -fsSL %s | sh", cur, in.exe, installScriptURL)
	}
	home := a.Cfg.CabooseHome
	unlock, err := selfupdate.Lock(home)
	if errors.Is(err, selfupdate.ErrLocked) {
		return Die("an update is already running (a launch started one); 'caboose update' again in a minute says how it went")
	}
	if err != nil {
		return Die("%v", err)
	}
	defer unlock()

	u := newUI(a.Stdout, termWidth(a.Stdout, 100))
	var sp *spinner
	say := func(what string) {
		if u.width > 0 {
			if sp == nil {
				sp = startSpinner(u, what)
			} else {
				sp.set(what)
			}
		}
	}
	stop := func() {
		if sp != nil {
			sp.end()
			sp = nil
		}
	}
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), updateBudget)
	defer cancel()
	say("checking for a newer caboose")
	st := selfupdate.ReadState(home)
	st.Checked, st.Error = a.now(), ""
	tag, err := a.releases().Latest(ctx)
	if err != nil {
		stop()
		st.Error = err.Error()
		_ = selfupdate.WriteState(home, st)
		return Die("%v", err)
	}
	st.Latest = tag
	if selfupdate.Compare(tag, cur) <= 0 {
		stop()
		_ = selfupdate.WriteState(home, st)
		if tag == cur {
			u.ok("caboose %s is the latest release.", cur)
		} else {
			u.ok("caboose %s is newer than the latest release, %s: nothing to do.", cur, tag)
		}
		return nil
	}
	say(fmt.Sprintf("downloading caboose %s", tag))
	if err := a.layout().Install(ctx, a.releases(), tag, runtime.GOOS, runtime.GOARCH, in.exe); err != nil {
		stop()
		st.Error = err.Error()
		_ = selfupdate.WriteState(home, st)
		return Die("updating to %s: %v", tag, err)
	}
	stop()
	st.From, st.To, st.Announced = cur, tag, true
	_ = selfupdate.WriteState(home, st)
	u.ok("Updated caboose: %s → %s", cur, tag)
	u.note("The next caboose you run is %s. When it builds the sandbox's image differently, the next 'caboose restart' rebuilds it.", tag)
	return nil
}

// updateSummary says how this caboose updates, for version and doctor:
// the level is doctor's.
func (a *App) updateSummary() (level, string) {
	in, cur := a.installed(), currentVersion()
	switch in.kind {
	case installDev:
		return levelOK, cur + ", a development build; it does not update itself"
	case installOther:
		return levelNote, fmt.Sprintf("%s, not installed by install.sh (%s), so it does not update itself", cur, in.exe)
	}
	if a.autoUpdateOff() {
		return levelNote, cur + "; automatic updates are off (CABOOSE_NO_AUTO_UPDATE)"
	}
	st := selfupdate.ReadState(a.Cfg.CabooseHome)
	switch {
	case st.Latest != "" && selfupdate.Valid(st.Latest) && selfupdate.Compare(st.Latest, cur) > 0:
		return levelNote, fmt.Sprintf("%s; %s is out, and the next update check installs it ('caboose update' does now)", cur, st.Latest)
	case st.Error != "":
		return levelNote, fmt.Sprintf("%s; the last update check (%s) failed: %s", cur, ago(a.now(), st.Checked), firstLine(st.Error))
	case st.Checked.IsZero():
		return levelOK, cur + ", updates itself (not checked yet)"
	}
	return levelOK, fmt.Sprintf("%s, updates itself (last checked %s)", cur, ago(a.now(), st.Checked))
}

// ago says how long before now t was, roughly.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return "never"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
