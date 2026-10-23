package daemonproto

import "time"

// A proposal applied with its image built first (proposal_build) runs in
// the daemon as a job of the environment's: the call returns at once,
// with the job running, and status's EnvStatus.Job says how it went.
// KindBusy while a job already runs for the environment. Its kind and
// states:
const (
	JobApply = "apply"

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
// failed, a proposal always, notes only once it is done; every string
// bounded and printable.
func (j *JobInfo) Validate() error {
	switch j.Kind {
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

// JobResult is the result of proposal_build: the job it started.
type JobResult struct {
	Job JobInfo `json:"job"`
}

// Validate checks the job.
func (r *JobResult) Validate() error { return r.Job.Validate() }
