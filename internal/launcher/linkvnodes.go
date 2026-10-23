package launcher

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/vm/vnodes"
)

// Under vm, the Mac's process that serves the VM's shares holds a file
// open for every file and directory the guest has cached from them, and
// enough of those take the Mac's vnodes, after which nothing on the Mac
// can open a file. The link samples what this environment's VM holds
// against kern.maxvnodes and tells whoever is at the host as it crosses
// each level, once: a level is told of again only after the count has
// fallen below vnodeRearmPct.
const (
	// vnodePollEvery is how often the link counts.
	vnodePollEvery = time.Minute
	// vnodeWarnPct and vnodeAlarmPct are the levels told of, in percent of
	// kern.maxvnodes.
	vnodeWarnPct  = 50
	vnodeAlarmPct = 80
	// vnodeRearmPct is where the count has to fall below for the levels to
	// be told of again.
	vnodeRearmPct = 40
	// vnodeErrorEvery is the least time between two log lines for the same
	// failure to count.
	vnodeErrorEvery = time.Hour
)

// vnodeSampler counts what the environment's VM holds; vnodes.Finder is
// the real one.
type vnodeSampler interface {
	Sample() (vnodes.Sample, error)
}

// vnodeWatch is the levels told of, and the failures logged.
type vnodeWatch struct {
	sampler vnodeSampler
	env     string
	// level is the highest level told of since the count was last below
	// vnodeRearmPct: 0, vnodeWarnPct or vnodeAlarmPct.
	level int
	// warning is what the last count says while it is at or past a level,
	// else "": the level's state, not the notification, so it stays up
	// until the count is back under vnodeRearmPct.
	warning string
	// errs are the failures logged, by message, and when.
	errs map[string]time.Time
}

// check samples once, and returns the notification due now, if any, and
// what to log.
func (w *vnodeWatch) check(now time.Time) (text string, notify bool, logs []string) {
	s, err := w.sampler.Sample()
	if err != nil {
		msg := err.Error()
		for m, at := range w.errs {
			if now.Sub(at) >= vnodeErrorEvery {
				delete(w.errs, m)
			}
		}
		if _, logged := w.errs[msg]; !logged {
			if w.errs == nil {
				w.errs = map[string]time.Time{}
			}
			w.errs[msg] = now
			logs = append(logs, fmt.Sprintf("cannot count the files the VM holds open (said at most hourly): %s", msg))
		}
		return "", false, logs
	}
	if s.Max <= 0 {
		return "", false, nil
	}
	held := fmt.Sprintf("the VM (pid %d) holds %s of the Mac's %s vnodes", s.PID, thousands(s.Files), thousands(s.Max))
	at := func(pct int) bool { return int64(s.Files)*100 >= int64(s.Max)*int64(pct) }
	if !at(vnodeRearmPct) {
		if w.level != 0 {
			logs = append(logs, fmt.Sprintf("%s, under %d%% again", held, vnodeRearmPct))
		}
		w.level = 0
		w.warning = ""
		return "", false, logs
	}
	reached := 0
	switch {
	case at(vnodeAlarmPct):
		reached = vnodeAlarmPct
	case at(vnodeWarnPct):
		reached = vnodeWarnPct
	}
	if reached > 0 || w.level > 0 {
		w.warning = fmt.Sprintf("the VM holds %s of the Mac's %s vnodes", thousands(s.Files), thousands(s.Max))
	}
	if reached <= w.level {
		return "", false, nil
	}
	w.level = reached
	logs = append(logs, fmt.Sprintf("%s, past %d%%", held, reached))
	return fmt.Sprintf("the VM holds %s of the Mac's %s vnodes: see %s",
		thousands(s.Files), thousands(s.Max), envCommand(w.env, "doctor")), true, logs
}

// vnodeWarning is what the count last warned of, "" while it is under the
// levels or is not made.
func (r *linkRunner) vnodeWarning() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.vnodes
}

// vnodeTitle is the title of the notification.
func vnodeTitle(env string) string { return "caboose (" + env + ")" }

// thousands is n with its digits grouped by commas.
func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := n < 0
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// vnodesHost is the Mac's processes, which vnodeFinder searches; a
// variable so that tests can play a host with none to scan.
var vnodesHost = vnodes.Host

// vnodeFinder finds this environment's VM process by what it holds under
// the data dir, which no other environment shares, and out of its vm/,
// which holds the builder's disks too.
func (a *App) vnodeFinder() (*vnodes.Finder, error) {
	sys, err := vnodesHost()
	if err != nil {
		return nil, err
	}
	f := &vnodes.Finder{Sys: sys}
	// The kernel names files by their real path.
	dirs := []string{filepath.Clean(a.Cfg.DataDir)}
	if r, err := filepath.EvalSymlinks(a.Cfg.DataDir); err == nil && r != dirs[0] {
		dirs = append(dirs, r)
	}
	for _, d := range dirs {
		f.Dirs = append(f.Dirs, d)
		f.Exclude = append(f.Exclude, filepath.Join(d, vmDirName))
	}
	return f, nil
}

// vmFilesHeld is what this environment's VM holds open on the Mac: the
// files and directories its process has open, kern.maxvnodes, and that
// process's PID. It fails off a Mac (vnodes.ErrUnsupported) and when the
// VM is not running.
func (a *App) vmFilesHeld() (held, maxVnodes, pid int, err error) {
	f, err := a.vnodeFinder()
	if err != nil {
		return 0, 0, 0, err
	}
	s, err := f.Sample()
	if err != nil {
		return 0, 0, 0, err
	}
	return s.Files, s.Max, s.PID, nil
}

// watchVnodes tells of the VM's open files crossing each level, until
// ctx ends. It serves vm alone, and on a Mac only: elsewhere there is nothing
// to count. It first counts a whole vnodePollEvery in, past the VM's boot.
// A failure is logged, and never ends the link.
func (r *linkRunner) watchVnodes(ctx context.Context) {
	if r.settingsNow().Isolation != isolationVM {
		return
	}
	f, err := r.a.vnodeFinder()
	if err != nil {
		if !errors.Is(err, vnodes.ErrUnsupported) {
			r.log.Printf("cannot count the files the VM holds open: %v", err)
		}
		return
	}
	w := &vnodeWatch{sampler: f, env: r.a.Cfg.Env}
	t := time.NewTicker(vnodePollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		text, notify, logs := w.check(time.Now())
		r.mu.Lock()
		r.vnodes = w.warning
		r.mu.Unlock()
		for _, l := range logs {
			r.log.Print(l)
		}
		if notify {
			r.mu.Lock()
			actions := r.cfg.Actions
			r.mu.Unlock()
			if err := hostlink.Notify(actions, vnodeTitle(r.a.Cfg.Env), text); err != nil {
				r.log.Printf("cannot show that the VM holds many files open: %v", err)
			}
		}
	}
}
