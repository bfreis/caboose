package launcher

import (
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// The daemon's jobs, such as a proposal applied with its image built
// first (daemonjob_apply.go), share starting (one per environment, busy
// otherwise, the exec gate, jobs.log) and ending (endJob: the state, the
// notification). A job runs in a goroutine of its own, and the call
// answers at once; any question it would ask fails it, since there is no
// terminal to ask it on (noTerminal). What it would say goes to the
// environment's jobs.log; the end is a host notification.
const (
	// jobsLogFile is an environment's jobs log, in its env dir, beside
	// the data dir and in no mount.
	jobsLogFile = "jobs.log"
	// maxJobsLog is the most jobs.log grows to before it is moved aside.
	maxJobsLog = 1 << 20
)

// envJob is one job of an environment's.
type envJob struct {
	info daemonproto.JobInfo // under the envLink's mu
	done chan struct{}       // closed once it has ended, and said so
}

// running reports whether j runs; false for no job.
func (j *envJob) running() bool { return j != nil && j.info.State == daemonproto.JobRunning }

// shown is j as status reports it at now: while it runs, and for
// daemonproto.JobKept after it ended; nil otherwise.
func (j *envJob) shown(now time.Time) *daemonproto.JobInfo {
	if j == nil || (!j.running() && now.Sub(j.info.Finished) > daemonproto.JobKept) {
		return nil
	}
	info := j.info
	return &info
}

func (l *DaemonLinks) jobClock() func() time.Time { return l.jobNow }

// clock is the time by e's jobs' clock.
func (e *envLink) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// jobEnv is env's link, for a job: a valid name, and a daemon that is not
// stopping.
func (l *DaemonLinks) jobEnv(env string) (*envLink, error) {
	if !config.ValidEnv(env) {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "bad environment name")
	}
	e := l.env(env)
	if e == nil {
		return nil, daemonproto.NewError(daemonproto.KindBusy, "the daemon is stopping")
	}
	return e, nil
}

// startJob starts info's job for e, on app, unless one runs for e: run
// runs it in a goroutine of its own, with jobs.log open, and ends it with
// endJob. It returns the job, running.
func (l *DaemonLinks) startJob(e *envLink, app *App, info daemonproto.JobInfo, run func(j *envJob, out *cappedlog.Log)) (*daemonproto.JobInfo, error) {
	if app.Cfg.EnvDir == "" {
		return nil, daemonproto.NewError(daemonproto.KindInternal, "%s has no environment directory for its jobs log", e.name)
	}
	max := l.jobsLogMax
	if max <= 0 {
		max = maxJobsLog
	}
	out, err := cappedlog.Open(filepath.Join(app.Cfg.EnvDir, jobsLogFile), max, "caboose daemon")
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.job.running() {
		k := e.job.info.Kind
		e.mu.Unlock()
		out.Close()
		return nil, daemonproto.NewError(daemonproto.KindBusy, "a %s job already runs for %s", k, e.name)
	}
	// A replace waits for the job as for a host command, and once it
	// has closed the gate (or holds it) starts none.
	if !l.gate.Enter() {
		e.mu.Unlock()
		out.Close()
		return nil, daemonproto.NewError(daemonproto.KindBusy, "the daemon is being replaced: try again once it is back")
	}
	info.State, info.Since = daemonproto.JobRunning, e.clock().UTC()
	j := &envJob{info: info, done: make(chan struct{})}
	e.job = j
	info = j.info
	e.mu.Unlock()
	go run(j, out)
	return &info, nil
}

// endJob records how j ended (failed with err, else done with notes),
// says so in jobs.log, closes out and tells the user text, followed by
// the error when it failed; then lets a replace waiting on it go ahead.
func (l *DaemonLinks) endJob(e *envLink, j *envJob, out *cappedlog.Log, lg *log.Logger, err error, notes []string, text string) {
	kind, env := j.info.Kind, e.name
	e.mu.Lock()
	j.info.Finished = e.clock().UTC()
	if j.info.Finished.Before(j.info.Since) {
		j.info.Finished = j.info.Since
	}
	if err != nil {
		j.info.State, j.info.Error = daemonproto.JobFailed, daemonproto.Text(oneLine(err.Error()), daemonproto.MaxJobError)
	} else {
		j.info.State = daemonproto.JobDone
		for _, n := range notes {
			if len(j.info.Notes) < daemonproto.MaxProposalNotes {
				j.info.Notes = append(j.info.Notes, daemonproto.Text(proposal.Printable(n), proposalTextMax))
			}
		}
	}
	info := j.info
	e.mu.Unlock()

	if err != nil {
		text += info.Error
	}
	lg.Printf("%s %s: %s", kind, env, info.State)
	if err != nil {
		lg.Printf("%s %s: %s", kind, env, info.Error)
	}
	for _, n := range info.Notes {
		lg.Printf("%s %s: %s", kind, env, n)
	}
	out.Close()
	l.logf("%s job for %s: %s", kind, env, info.State)
	if l.jobNotify != nil {
		if notify := l.jobNotify(env); notify != nil {
			notify("caboose: "+env, text)
		}
	}
	// A replace waiting on the job may exit from here on.
	l.gate.Leave()
	close(j.done)
}

// oneLine is s with its lines joined, for a status field.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
