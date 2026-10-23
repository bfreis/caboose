package daemonproto

import (
	"context"
	"encoding/json"
	"time"
)

// MethodCheckpoints lists an environment's checkpoints (params EnvParams,
// result CheckpointsResult). It only reads: no request takes, prunes or
// restores a checkpoint, since each can run longer than a request may.
const MethodCheckpoints = "checkpoints"

// Bounds of the checkpoint fields: every list, and every string, which
// may name a file the sandbox made.
const (
	MaxCheckpointPaths = 256 // paths in a status or listing
	MaxCheckpointList  = 256 // checkpoints of one path in a listing
	maxCheckpointText  = 1024
	maxCheckpointShort = 128 // an ID, a key, a method, a reason
)

// CheckpointStatus is an environment's checkpoint schedule, in its
// EnvStatus: absent when the daemon runs no schedule for it.
type CheckpointStatus struct {
	Paths []CheckpointPathStatus `json:"paths"`
	// InProgressSince is when the checkpoint being taken began, of
	// InProgressPath.
	InProgressSince time.Time `json:"in_progress_since,omitzero"`
	InProgressPath  string    `json:"in_progress_path,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	LastErrorAt     time.Time `json:"last_error_at,omitzero"`
	// LowSpace is set while the disk holding the store is below a floor.
	LowSpace bool `json:"low_space,omitempty"`
	// SpaceSkipped says why the scheduled checkpoints are skipped: the
	// disk is below the soft floor.
	SpaceSkipped string `json:"space_skipped,omitempty"`
	// Exposed says why no checkpoint is taken: a sandbox can reach the
	// store.
	Exposed string `json:"exposed,omitempty"`
	// Slow names the last checkpoint that ran past five minutes.
	Slow string `json:"slow,omitempty"`
	// Served is how this daemon serves the environment's schedule:
	// "held" (it runs the link), "last" (the sandbox stopped; only the stop
	// checkpoint and the prunes are made).
	Served string `json:"served,omitempty"`
}

// CheckpointPathStatus is one configured path's schedule.
type CheckpointPathStatus struct {
	Path string `json:"path"`
	Key  string `json:"key"`
	// Resolved and Reason are as the last resolution found them; Checked
	// is false before the first.
	Checked  bool   `json:"checked,omitempty"`
	Resolved bool   `json:"resolved"`
	Reason   string `json:"reason,omitempty"`
	// Newest is the newest complete checkpoint's ID, NewestAgeS its age in
	// seconds when the status was made (negative when the clock was set
	// back), Method how it was copied.
	Newest     string    `json:"newest,omitempty"`
	NewestAt   time.Time `json:"newest_at,omitzero"`
	NewestAgeS int64     `json:"newest_age_s,omitempty"`
	Method     string    `json:"method,omitempty"`
	// Shrunk is the earlier checkpoint to restore from while a sharp
	// shrink stands.
	Shrunk string `json:"shrunk,omitempty"`
	// NextAt is when the next one is due, while the sandbox runs.
	NextAt  time.Time `json:"next_at,omitzero"`
	Failing string    `json:"failing,omitempty"`
}

// CheckpointsResult is the result of checkpoints: each configured path's
// checkpoints, then the keys whose path is no longer configured.
type CheckpointsResult struct {
	Env     string             `json:"env"`
	Paths   []CheckpointListed `json:"paths"`
	Orphans []CheckpointListed `json:"orphans,omitempty"`
	// Truncated is set when checkpoints were left out to fit a frame.
	Truncated bool `json:"truncated,omitempty"`
}

// CheckpointListed is one path's, or key's, checkpoints, newest first.
type CheckpointListed struct {
	Path     string `json:"path"`
	Key      string `json:"key"`
	Checked  bool   `json:"checked,omitempty"`
	Resolved bool   `json:"resolved,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Shrunk is the earlier checkpoint to restore from while a sharp
	// shrink stands.
	Shrunk      string           `json:"shrunk,omitempty"`
	Checkpoints []CheckpointView `json:"checkpoints"`
	// PinsError says why the store's pins cannot be read (no checkpoint
	// is pruned meanwhile).
	PinsError string `json:"pins_error,omitempty"`
}

// CheckpointView is one checkpoint as listed. Error is set when its
// meta.json cannot be used (it is then never pruned).
type CheckpointView struct {
	ID       string    `json:"id"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	Reason   string    `json:"reason,omitempty"`
	Method   string    `json:"method,omitempty"`
	Entries  int64     `json:"entries"`
	Bytes    int64     `json:"bytes"`
	Linked   int64     `json:"linked"`
	Skipped  int64     `json:"skipped"`
	Shrunk   string    `json:"shrunk,omitempty"` // the earlier checkpoint this one shrank from
	// Pinned is set while the checkpoint is kept outside retention, since
	// PinnedBy, a later one, held half or less of it.
	Pinned   bool      `json:"pinned,omitempty"`
	PinnedBy string    `json:"pinned_by,omitempty"`
	PinnedAt time.Time `json:"pinned_at,omitzero"`
	Error    string    `json:"error,omitempty"`
}

func (c *CheckpointStatus) validate() error {
	if len(c.Paths) > MaxCheckpointPaths {
		return bad("more than %d checkpoint paths", MaxCheckpointPaths)
	}
	for i := range c.Paths {
		if err := c.Paths[i].validate(); err != nil {
			return err
		}
	}
	for _, s := range []struct{ what, v string }{
		{"checkpoint in progress", c.InProgressPath}, {"checkpoint error", c.LastError},
		{"checkpoint exposure", c.Exposed}, {"slow checkpoint", c.Slow}, {"checkpoint space", c.SpaceSkipped},
	} {
		if err := checkText(s.what, s.v, maxCheckpointText); err != nil {
			return err
		}
	}
	return checkText("checkpoint serving", c.Served, maxName)
}

func (p *CheckpointPathStatus) validate() error {
	for _, s := range []struct {
		what, v string
		max     int
	}{
		{"checkpoint path", p.Path, maxCheckpointText}, {"checkpoint key", p.Key, maxCheckpointShort},
		{"checkpoint reason", p.Reason, maxCheckpointText}, {"checkpoint ID", p.Newest, maxCheckpointShort},
		{"checkpoint method", p.Method, maxCheckpointShort}, {"checkpoint ID", p.Shrunk, maxCheckpointShort},
		{"checkpoint error", p.Failing, maxCheckpointText},
	} {
		if err := checkText(s.what, s.v, s.max); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks a listing.
func (r *CheckpointsResult) Validate() error {
	if !ValidEnv(r.Env) {
		return bad("bad environment name in a checkpoint listing")
	}
	if len(r.Paths) > MaxCheckpointPaths || len(r.Orphans) > MaxCheckpointPaths {
		return bad("more than %d checkpoint paths", MaxCheckpointPaths)
	}
	for _, l := range [][]CheckpointListed{r.Paths, r.Orphans} {
		for i := range l {
			if err := l[i].validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *CheckpointListed) validate() error {
	for _, s := range []struct {
		what, v string
		max     int
	}{
		{"checkpoint path", l.Path, maxCheckpointText}, {"checkpoint key", l.Key, maxCheckpointShort},
		{"checkpoint reason", l.Reason, maxCheckpointText}, {"checkpoint ID", l.Shrunk, maxCheckpointShort},
		{"checkpoint pins", l.PinsError, maxCheckpointText},
	} {
		if err := checkText(s.what, s.v, s.max); err != nil {
			return err
		}
	}
	if len(l.Checkpoints) > MaxCheckpointList {
		return bad("more than %d checkpoints of a path", MaxCheckpointList)
	}
	for i := range l.Checkpoints {
		if err := l.Checkpoints[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

func (v *CheckpointView) validate() error {
	for _, s := range []struct {
		what, v string
		max     int
	}{
		{"checkpoint ID", v.ID, maxCheckpointShort}, {"checkpoint reason", v.Reason, maxCheckpointShort},
		{"checkpoint method", v.Method, maxCheckpointShort}, {"checkpoint ID", v.Shrunk, maxCheckpointShort},
		{"checkpoint ID", v.PinnedBy, maxCheckpointShort}, {"checkpoint error", v.Error, maxCheckpointText},
	} {
		if err := checkText(s.what, s.v, s.max); err != nil {
			return err
		}
	}
	if v.Entries < 0 || v.Bytes < 0 || v.Linked < 0 || v.Skipped < 0 {
		return bad("negative checkpoint counts")
	}
	return nil
}

// FitCheckpoints keeps, of r's checkpoints, as many as fit a frame with
// room to spare: the oldest of each path go first, and Truncated says so.
func FitCheckpoints(r *CheckpointsResult) {
	size := func() int {
		b, err := json.Marshal(r)
		if err != nil {
			return resultBudget + 1
		}
		return len(b)
	}
	for size() > resultBudget {
		// Drop the oldest checkpoint of the longest list.
		var longest *CheckpointListed
		for _, l := range [][]CheckpointListed{r.Paths, r.Orphans} {
			for i := range l {
				if longest == nil || len(l[i].Checkpoints) > len(longest.Checkpoints) {
					longest = &l[i]
				}
			}
		}
		if longest == nil || len(longest.Checkpoints) == 0 {
			return
		}
		n := max(1, len(longest.Checkpoints)/8)
		longest.Checkpoints = longest.Checkpoints[:len(longest.Checkpoints)-n]
		r.Truncated = true
	}
}

// Checkpoints lists env's checkpoints.
func (c *Client) Checkpoints(ctx context.Context, env string) (*CheckpointsResult, error) {
	p := &EnvParams{Env: env}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	var r CheckpointsResult
	if err := c.Call(ctx, MethodCheckpoints, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
