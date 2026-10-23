package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/vm/vnodes"
)

var _ daemon.Details = (*DaemonLinks)(nil)

const (
	// linkLogWindow is how much of the end of the link log link_log reads.
	linkLogWindow = 256 << 10
	// envDetailsBudget is how long env_details takes in all; a part that
	// has not answered by then is an error entry.
	envDetailsBudget = 5 * time.Second
	// daemonDoctorBudget is how long the daemon's doctor runs: under the web
	// server's 30 s write timeout, so the page gets an answer, not a cut
	// connection.
	daemonDoctorBudget = 25 * time.Second
	// tmuxSessionFormat makes tmux list sessions by name.
	tmuxSessionFormat = "#{session_name}"
)

// detailText is s made printable and cut to at most n bytes, at a character.
func detailText(s string, n int) string {
	s = proposal.Printable(s)
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// LinkLog is the last lines of env's link log (daemon.Details).
func (l *DaemonLinks) LinkLog(_ context.Context, env string, lines int) (*daemonproto.LinkLogResult, error) {
	app, err := l.newApp(env)
	if err != nil {
		return nil, err
	}
	return app.linkLogTail(lines)
}

// linkLogTail is the last lines lines of the link log, read from at most
// its last linkLogWindow bytes. The file sits at the data dir's top level,
// in no mount; it is still read as a plain file only.
func (a *App) linkLogTail(lines int) (*daemonproto.LinkLogResult, error) {
	res := &daemonproto.LinkLogResult{Lines: []string{}}
	lines = min(max(lines, 1), daemonproto.MaxLinkLogLines)
	if fi, err := nofollow.Dir(a.Cfg.DataDir).Lstat(linkLogFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("the link log is not a plain file")
	}
	f, err := os.Open(filepath.Join(a.Cfg.DataDir, linkLogFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var skipFirst bool
	if fi.Size() > linkLogWindow {
		if _, err := f.Seek(-linkLogWindow, io.SeekEnd); err != nil {
			return nil, err
		}
		// The window starts mid-line, most likely: its first line is
		// partial and goes.
		skipFirst, res.Truncated = true, true
	}
	data, err := io.ReadAll(io.LimitReader(f, linkLogWindow+1))
	if err != nil {
		return nil, err
	}
	if len(data) > linkLogWindow {
		data = data[:linkLogWindow]
	}
	if skipFirst {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	all := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		all = nil
	}
	if len(all) > lines {
		all, res.Truncated = all[len(all)-lines:], true
	}
	for _, s := range all {
		res.Lines = append(res.Lines, detailText(s, daemonproto.MaxLinkLogLine))
	}
	return res, nil
}

// detailPart is one part of env_details: it returns what to add to the
// result, so a part that finishes after the budget adds nothing.
type detailPart struct {
	name string
	run  func() (func(*daemonproto.EnvDetailsResult), error)
}

// EnvDetails is what status shows of env, as data (daemon.Details). A
// call while another for env runs shares its answer, so a page polling
// every few seconds never piles details up behind a slow engine; each
// caller still returns when its own ctx ends.
func (l *DaemonLinks) EnvDetails(ctx context.Context, env string) (*daemonproto.EnvDetailsResult, error) {
	return l.detailsFlights.do(ctx, env, func() (*daemonproto.EnvDetailsResult, error) {
		// The shared run ends with its budget, never with one caller.
		return l.envDetails(context.WithoutCancel(ctx), env)
	})
}

func (l *DaemonLinks) envDetails(ctx context.Context, env string) (*daemonproto.EnvDetailsResult, error) {
	app, err := l.newApp(env)
	if err != nil {
		return nil, err
	}
	res := &daemonproto.EnvDetailsResult{Env: env, Isolation: detailText(isolationSummary(app.Cfg), 1000),
		Sessions: []string{}, Ports: []daemonproto.PortDetail{}, Keep: []daemonproto.KeepDetail{}}
	ctx, cancel := context.WithTimeout(ctx, envDetailsBudget)
	defer cancel()
	type done struct {
		name  string
		apply func(*daemonproto.EnvDetailsResult)
		err   error
	}
	parts := []detailPart{
		{"image", app.detailImage},
		{"sessions", app.detailSessions},
		{"started", app.detailStarted},
		{"resources", app.detailResources},
		{"keep", app.detailKeep},
		{"ports", func() (func(*daemonproto.EnvDetailsResult), error) { return l.detailPorts(env), nil }},
	}
	ch := make(chan done, len(parts))
	for _, p := range parts {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					ch <- done{name: p.name, err: fmt.Errorf("failed: %v", r)}
				}
			}()
			apply, err := p.run()
			ch <- done{p.name, apply, err}
		}()
	}
	pending := map[string]bool{}
	for _, p := range parts {
		pending[p.name] = true
	}
	for len(pending) > 0 {
		select {
		case d := <-ch:
			delete(pending, d.name)
			switch {
			case d.err != nil:
				res.Errors = append(res.Errors, daemonproto.DetailError{Part: d.name, Message: detailText(d.err.Error(), 1000)})
			case d.apply != nil:
				d.apply(res)
			}
		case <-ctx.Done():
			for _, p := range parts {
				if pending[p.name] {
					res.Errors = append(res.Errors, daemonproto.DetailError{Part: p.name, Message: "did not answer in time"})
				}
			}
			clear(pending)
		}
	}
	return res, nil
}

// detailImage classifies the image the way status's warnings do.
func (a *App) detailImage() (func(*daemonproto.EnvDetailsResult), error) {
	labels, exists, err := a.images().ImageLabels(a.Cfg.Image)
	if err != nil && !exists {
		return nil, fmt.Errorf("cannot inspect '%s': %v", a.Cfg.Image, err)
	}
	st := a.imageStatus(labels, exists)
	d := &daemonproto.ImageDetail{State: daemonproto.ImageUnknown}
	switch st.state {
	case imageCurrent:
		d.State = daemonproto.ImageCurrent
	case imageStale:
		d.State, d.Why = daemonproto.ImageStale, st.reason
		if st.switched() {
			d.State = daemonproto.ImageSwitched
			d.Why = "built on " + st.builtOn + ": " + st.reason
		}
	case imageMissing:
		d.Why = "the image is not built yet"
	case imageUnlabelled:
		d.Why = "the image was not built by caboose build"
	}
	d.Why = detailText(d.Why, 1000)
	return func(r *daemonproto.EnvDetailsResult) { r.Image = d }, nil
}

// detailStarted is when the running sandbox started, from its backend
// (backend.Starter); nothing, and no error, when it does not run or its
// backend cannot tell (a vm with no vmm.pid).
func (a *App) detailStarted() (func(*daemonproto.EnvDetailsResult), error) {
	st, ok := a.box().(backend.Starter)
	if !ok {
		return nil, nil
	}
	at, ok := st.Started()
	if !ok {
		return nil, nil
	}
	return func(r *daemonproto.EnvDetailsResult) { r.Started = at.UTC() }, nil
}

// detailSessions lists the sandbox's tmux sessions by name, at most
// MaxDetailSessions, when the sandbox runs.
func (a *App) detailSessions() (func(*daemonproto.EnvDetailsResult), error) {
	if a.state() != "running" {
		return nil, nil
	}
	out, err := backend.RawOutput(a.box(), "tmux", "list-sessions", "-F", tmuxSessionFormat)
	if err != nil && strings.TrimSpace(out) == "" {
		// tmux with no server is "no sessions", not a failure of this part.
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "no sessions") {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot list tmux sessions: %v", err)
	}
	var names []string
	for _, s := range strings.Split(out, "\n") {
		if s = strings.TrimSpace(s); s != "" && len(names) < daemonproto.MaxDetailSessions {
			names = append(names, detailText(s, 200))
		}
	}
	return func(r *daemonproto.EnvDetailsResult) { r.Sessions = names }, nil
}

// detailResources is what is known: the VM's memory limit from the
// profile, and on a Mac the files its process holds open. What is not
// known is left out.
func (a *App) detailResources() (func(*daemonproto.EnvDetailsResult), error) {
	if !a.isVM() {
		return nil, nil
	}
	x := &daemonproto.ResourcesDetail{}
	if a.Cfg.VMMemory != "" {
		if mib, err := parseMemory(a.Cfg.VMMemory); err == nil && mib > 0 {
			x.MemoryLimitBytes = int64(mib) << 20
		}
	}
	var perr error
	if a.state() == "running" {
		held, maxV, _, err := a.vmFilesHeld()
		switch {
		case err == nil:
			x.Vnodes, x.VnodesMax = held, maxV
		case !errors.Is(err, vnodes.ErrUnsupported):
			perr = fmt.Errorf("cannot count the files the VM holds open: %v", err)
		}
	}
	if *x == (daemonproto.ResourcesDetail{}) {
		return nil, perr
	}
	return func(r *daemonproto.EnvDetailsResult) { r.Resources = x }, perr
}

// detailKeep lists the sandbox config's keep entries, read through
// nofollow.
func (a *App) detailKeep() (func(*daemonproto.EnvDetailsResult), error) {
	sb, err := a.sandboxConfig()
	if err != nil {
		return nil, err
	}
	var keep []daemonproto.KeepDetail
	for _, k := range sb.Keep {
		if len(keep) >= daemonproto.MaxDetailKeep {
			break
		}
		keep = append(keep, daemonproto.KeepDetail{Path: detailText("~/"+k.Rel, 1000), Sync: len(k.Rules) > 0})
	}
	return func(r *daemonproto.EnvDetailsResult) { r.Keep = keep }, nil
}

// detailPorts is the ports env's link forwards now.
func (l *DaemonLinks) detailPorts(env string) func(*daemonproto.EnvDetailsResult) {
	l.mu.Lock()
	e := l.envs[env]
	l.mu.Unlock()
	if e == nil {
		return nil
	}
	e.mu.Lock()
	cur := e.cur
	e.mu.Unlock()
	if cur == nil {
		return nil
	}
	var ports []daemonproto.PortDetail
	for _, p := range cur.runner.forwarded() {
		if len(ports) >= daemonproto.MaxDetailPorts {
			break
		}
		ports = append(ports, daemonproto.PortDetail{Port: p, URL: "http://localhost:" + strconv.Itoa(p)})
	}
	return func(r *daemonproto.EnvDetailsResult) { r.Ports = ports }
}

// Doctor runs doctor's checks for env here and returns the findings
// (daemon.Details). The checks run in the daemon's environment, whose PATH
// and SSH agent can differ from a terminal's; they run offline, since the
// page asks for no sync.
//
// One run at a time for env: a call while one runs waits for that run's
// findings, within its own budget, and a run that outlives every caller's
// budget is not started again until it ends.
func (l *DaemonLinks) Doctor(ctx context.Context, env string) (*daemonproto.DoctorResult, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonDoctorBudget)
	defer cancel()
	res, err := l.doctorFlights.do(ctx, env, func() (*daemonproto.DoctorResult, error) {
		app, err := l.newApp(env)
		if err != nil {
			return nil, err
		}
		c := &checkup{}
		func() {
			defer func() { _ = recover() }()
			app.runCheckup(c, true)
		}()
		return c.result(time.Now()), nil
	})
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil, daemonproto.NewError(daemonproto.KindTimeout, "doctor did not finish in %s", daemonDoctorBudget)
	}
	return res, err
}

// flights runs one call of a piece of work per key at a time: a call
// while one runs for its key shares that run's answer. The run goes on
// when its callers give up, and the next call after it ends runs afresh,
// so a run that hangs (a doctor check waiting on a stuck engine) answers
// every later call for its key with that call's own deadline until it
// ends: no pile-up, and no fresh run either. The zero value is ready.
type flights[T any] struct {
	mu  sync.Mutex
	run map[string]*flight[T]
}

type flight[T any] struct {
	done chan struct{}
	res  T
	err  error
	// callers is how many calls this run answers; under flights.mu.
	callers int
}

// do is work's answer for key, from the run under way or a new one; ctx
// ending first is ctx's error, and the run goes on without this caller.
func (f *flights[T]) do(ctx context.Context, key string, work func() (T, error)) (T, error) {
	f.mu.Lock()
	if f.run == nil {
		f.run = map[string]*flight[T]{}
	}
	fl := f.run[key]
	if fl == nil {
		fl = &flight[T]{done: make(chan struct{})}
		f.run[key] = fl
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fl.err = fmt.Errorf("failed: %v", r)
				}
				f.mu.Lock()
				delete(f.run, key)
				f.mu.Unlock()
				close(fl.done)
			}()
			fl.res, fl.err = work()
		}()
	}
	fl.callers++
	f.mu.Unlock()
	select {
	case <-fl.done:
		return fl.res, fl.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// result is c's findings as data, at most MaxDoctorFindings, each printable.
func (c *checkup) result(at time.Time) *daemonproto.DoctorResult {
	res := &daemonproto.DoctorResult{Findings: []daemonproto.DoctorFinding{}, RanAt: at, As: daemonproto.DoctorAsDaemon}
	for _, r := range c.rows {
		if len(res.Findings) >= daemonproto.MaxDoctorFindings {
			res.Truncated = true
			break
		}
		res.Findings = append(res.Findings, daemonproto.DoctorFinding{Level: levelName(r.level),
			Label: detailText(r.label, 60), Message: detailText(r.text, 1000), Fix: detailText(r.fix, 1000)})
	}
	return res
}

// levelName is a level as daemonproto spells it.
func levelName(l level) string {
	switch l {
	case levelOK:
		return daemonproto.LevelOK
	case levelNote:
		return daemonproto.LevelNote
	case levelProblem:
		return daemonproto.LevelProblem
	}
	return daemonproto.LevelUnchecked
}
