package daemonproto

import (
	"context"
	"time"
)

// The sandbox's stop and restart, and a proposal applied with its image
// built first (proposal_build), run by the daemon as a job of the
// environment's: the call returns at once, with the job running, and
// status's EnvStatus.Job says how it went. KindBusy while a job already
// runs for the environment.
const (
	MethodSandboxStop    = "sandbox_stop"    // params EnvParams, result JobResult
	MethodSandboxRestart = "sandbox_restart" // params EnvParams, result JobResult
)

// Job kinds and states.
const (
	JobStop    = "stop"
	JobRestart = "restart"
	JobApply   = "apply"

	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Bounds of a job's report.
const (
	// MaxJobError is the longest error a job reports, in bytes.
	MaxJobError = 1000
	// MaxJobLines is the most lines of its output a job reports (its
	// last), and MaxJobLine the longest of them, in bytes.
	MaxJobLines = 20
	MaxJobLine  = 200
)

// JobInfo is one job: what it does, how it is, when it started and, once
// it has ended, when; Error says why a failed one failed. Lines are the
// last lines of what it said, as it goes. An apply job names its
// Proposal, and once done, Notes say what it did and what is left.
type JobInfo struct {
	Kind     string    `json:"kind"`
	State    string    `json:"state"`
	Since    time.Time `json:"since"`
	Finished time.Time `json:"finished,omitzero"`
	Error    string    `json:"error,omitempty"`
	Proposal string    `json:"proposal,omitempty"`
	Lines    []string  `json:"lines,omitempty"`
	Notes    []string  `json:"notes,omitempty"`
}

// Validate checks the job: a known kind and state, a start, an end only
// once it has ended (and not before its start), an error only when it
// failed, a proposal on an apply job only, and always, notes only once it
// is done; every string bounded and printable.
func (j *JobInfo) Validate() error {
	switch j.Kind {
	case JobStop, JobRestart:
		if j.Proposal != "" || len(j.Notes) > 0 {
			return bad("a proposal or notes on a %s job", j.Kind)
		}
	case JobApply:
		if !ValidProposalFile(j.Proposal) {
			return bad("an apply job with no proposal file")
		}
		if len(j.Notes) > 0 && j.State != JobDone {
			return bad("notes on an apply job that is not done")
		}
	default:
		return bad("bad job kind")
	}
	if len(j.Lines) > MaxJobLines {
		return bad("more than %d job lines", MaxJobLines)
	}
	for _, l := range j.Lines {
		if err := checkText("job line", l, MaxJobLine); err != nil {
			return err
		}
	}
	if len(j.Notes) > MaxProposalNotes {
		return bad("more than %d job notes", MaxProposalNotes)
	}
	for _, n := range j.Notes {
		if err := checkText("job note", n, maxProposalText); err != nil {
			return err
		}
	}
	if j.Since.IsZero() {
		return bad("a job with no start")
	}
	switch j.State {
	case JobRunning:
		if !j.Finished.IsZero() {
			return bad("a running job that finished")
		}
	case JobDone, JobFailed:
		if j.Finished.IsZero() || j.Finished.Before(j.Since) {
			return bad("an ended job with no end, or one before its start")
		}
	default:
		return bad("bad job state")
	}
	if j.Error != "" && j.State != JobFailed {
		return bad("an error on a job that did not fail")
	}
	return checkText("job error", j.Error, MaxJobError)
}

// JobResult is the result of sandbox_stop, sandbox_restart and
// proposal_build: the job they started.
type JobResult struct {
	Job JobInfo `json:"job"`
}

// Validate checks the job.
func (r *JobResult) Validate() error { return r.Job.Validate() }

// SandboxStop has the daemon stop env's sandbox, ending every session in
// it, without asking: the caller asked the user.
func (c *Client) SandboxStop(ctx context.Context, env string) (*JobResult, error) {
	return c.jobCall(ctx, MethodSandboxStop, env)
}

// SandboxRestart has the daemon recreate env's sandbox, ending every
// session in it, without asking: the caller asked the user.
func (c *Client) SandboxRestart(ctx context.Context, env string) (*JobResult, error) {
	return c.jobCall(ctx, MethodSandboxRestart, env)
}

func (c *Client) jobCall(ctx context.Context, method, env string) (*JobResult, error) {
	p := &EnvParams{Env: env}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	var r JobResult
	if err := c.Call(ctx, method, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
