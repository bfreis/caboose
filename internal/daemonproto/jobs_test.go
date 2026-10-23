package daemonproto

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestJobInfoValidate(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	good := []JobInfo{
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "dot.toml"},
		{Kind: JobApply, State: JobDone, Since: at, Finished: at.Add(time.Second), Proposal: "dot.toml"},
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "dot.toml", Lines: []string{"Building", "<b>step</b> 1/3"}},
		{Kind: JobApply, State: JobDone, Since: at, Finished: at, Proposal: "dot.toml", Notes: []string{"Wrote it", "restart"}},
		{Kind: JobApply, State: JobFailed, Since: at, Finished: at, Proposal: "dot.toml", Error: "the build failed", Lines: []string{"x"}},
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "dot.toml", Lines: make([]string, MaxJobLines)},
	}
	for _, j := range good {
		if err := j.Validate(); err != nil {
			t.Errorf("%+v: %v", j, err)
		}
		st := StatusResult{Envs: []EnvStatus{{Name: "alpha", Job: &j}}}
		if err := st.Validate(); err != nil {
			t.Errorf("status with %+v: %v", j, err)
		}
	}
	for _, j := range []JobInfo{
		{Kind: "start", State: JobRunning, Since: at},
		{Kind: "stop", State: JobRunning, Since: at},
		{Kind: JobApply, State: "queued", Since: at, Proposal: "dot.toml"},
		{Kind: JobApply, State: JobRunning, Proposal: "dot.toml"},
		{Kind: JobApply, State: JobRunning, Since: at, Finished: at, Proposal: "dot.toml"},
		{Kind: JobApply, State: JobDone, Since: at, Proposal: "dot.toml"},
		{Kind: JobApply, State: JobDone, Since: at, Finished: at.Add(-time.Second), Proposal: "dot.toml"},
		{Kind: JobApply, State: JobDone, Since: at, Finished: at, Proposal: "dot.toml", Error: "x"},
		{Kind: JobApply, State: JobFailed, Since: at, Finished: at, Proposal: "dot.toml", Error: "a\nb"},
		{Kind: JobApply, State: JobFailed, Since: at, Finished: at, Proposal: "dot.toml", Error: "\u202e"},
		{Kind: JobApply, State: JobFailed, Since: at, Finished: at, Proposal: "dot.toml", Error: strings.Repeat("x", MaxJobError+1)},
		{Kind: JobApply, State: JobRunning, Since: at},
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "../config.toml"},
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "dot.toml", Notes: []string{"early"}},
		{Kind: JobApply, State: JobFailed, Since: at, Finished: at, Proposal: "dot.toml", Error: "x", Notes: []string{"n"}},
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "dot.toml", Lines: make([]string, MaxJobLines+1)},
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "dot.toml", Lines: []string{strings.Repeat("x", MaxJobLine+1)}},
		{Kind: JobApply, State: JobRunning, Since: at, Proposal: "dot.toml", Lines: []string{"a\x1b[2J"}},
		{Kind: JobApply, State: JobDone, Since: at, Finished: at, Proposal: "dot.toml", Notes: make([]string, MaxProposalNotes+1)},
		{Kind: JobApply, State: JobDone, Since: at, Finished: at, Proposal: "dot.toml", Notes: []string{"\u202e"}},
	} {
		if err := j.Validate(); err == nil {
			t.Errorf("accepted %+v", j)
		}
		st := StatusResult{Envs: []EnvStatus{{Name: "alpha", Job: &j}}}
		if err := st.Validate(); err == nil {
			t.Errorf("status accepted %+v", j)
		}
		if err := (&JobResult{Job: j}).Validate(); err == nil {
			t.Errorf("result accepted %+v", j)
		}
	}
}

// Whatever validates of a job is of a known kind and state, and its error
// bounded and printable.
func FuzzJobs(f *testing.F) {
	for _, s := range []string{
		`{"job":{"kind":"apply","state":"running","since":"2026-10-18T12:00:00Z","proposal":"a.toml"}}`,
		`{"job":{"kind":"apply","state":"failed","since":"2026-10-18T12:00:00Z","finished":"2026-10-18T12:00:01Z","proposal":"a.toml","error":"x"}}`,
		`{"daemon":{},"envs":[{"name":"a","job":{"kind":"apply","state":"done","since":"2026-10-18T12:00:00Z","finished":"2026-10-18T12:00:00Z","proposal":"a.toml"}}]}`,
		`{"job":{"kind":"apply","state":"failed","proposal":"a.toml","error":"\u0007"}}`,
		`{"job":{"kind":"apply","state":"done","since":"2026-10-18T12:00:00Z","finished":"2026-10-18T12:00:00Z","proposal":"a.toml","lines":["b"],"notes":["c"]}}`, `{"job":null}`, `[]`, `null`,
	} {
		f.Add([]byte(s))
	}
	check := func(t *testing.T, j *JobInfo) {
		switch {
		case j.Kind != JobApply,
			j.State != JobRunning && j.State != JobDone && j.State != JobFailed,
			!printable(j.Error) || len(j.Error) > MaxJobError,
			!ValidProposalFile(j.Proposal),
			len(j.Lines) > MaxJobLines || len(j.Notes) > MaxProposalNotes,
			j.Since.IsZero(), j.State == JobRunning && !j.Finished.IsZero():
			t.Fatalf("accepted %+v", j)
		}
		for _, l := range append(slices.Clone(j.Lines), j.Notes...) {
			if !printable(l) {
				t.Fatalf("accepted %+v", j)
			}
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var r JobResult
		if json.Unmarshal(b, &r) == nil && r.Validate() == nil {
			check(t, &r.Job)
		}
		var st StatusResult
		if json.Unmarshal(b, &st) == nil && st.Validate() == nil {
			for _, e := range st.Envs {
				if e.Job != nil {
					check(t, e.Job)
				}
			}
		}
	})
}
