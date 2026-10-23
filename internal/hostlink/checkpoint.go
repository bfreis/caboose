package hostlink

// Checkpoints asked for by the sandbox: `caboose-agent checkpoint
// [PATH...]` names container paths, and the host takes a checkpoint of
// each now, as its schedule would. The sandbox's word only picks among
// config.toml's [checkpoints] paths: what it sends is bounded, checked and
// normalised as config.toml's entries are, compared with them exactly,
// and anything else is "unknown path". Which host directory a path copies
// is resolved by the taker from config.toml and the roots the sandbox
// mounts, never from anything the sandbox said. The policy decides it
// (checkpoint_requests); the environment's limits bound how often, and
// what it logs.

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
)

// Checkpointer takes the checkpoints a sandbox asks for: the environment's
// checkpoint schedule, as the daemon runs it. Tests fake it.
type Checkpointer interface {
	// CheckpointPaths are config.toml's [checkpoints] paths as they are
	// now, normalised (config.CheckCheckpoints); none when there are none.
	CheckpointPaths() ([]string, error)
	// TakeCheckpoints takes a checkpoint now of each of paths, every one
	// of them among CheckpointPaths, through the same gate and lock as
	// the schedule's, and says what came of each. It returns once ctx
	// ends, at the latest.
	TakeCheckpoints(ctx context.Context, paths []string) []agentproto.CheckpointTaken
}

// The limits on checkpoints asked for by the sandbox, per environment.
const (
	// CheckpointPathEvery is the least time between two checkpoints of one
	// path asked for by the sandbox.
	CheckpointPathEvery = time.Minute
	// CheckpointRequestsPerHour is how many requests are accepted in any
	// hour: a request counts once it has at least one path to take,
	// however many it has.
	CheckpointRequestsPerHour = 10
	// checkpointLogMax is the most lines of checkpoint requests logged a
	// minute; the rest are counted.
	checkpointLogMax   = 20
	checkpointLogEvery = time.Minute
)

// CheckpointLimits are one environment's limits on the checkpoints its
// sandbox asks for, and on what is logged of them: kept by whoever runs
// the environment's links, so a link that starts again starts no fresh
// count.
type CheckpointLimits struct {
	now func() time.Time

	mu       sync.Mutex
	last     map[string]time.Time // path: when a checkpoint of it was last accepted
	accepted []time.Time          // when each request of the last hour was accepted

	logFrom            time.Time
	logged, suppressed int
}

// NewCheckpointLimits are limits with nothing accepted yet, on the clock
// now (time.Now when nil).
func NewCheckpointLimits(now func() time.Time) *CheckpointLimits {
	if now == nil {
		now = time.Now
	}
	return &CheckpointLimits{now: now, last: map[string]time.Time{}}
}

// within reports whether t is less than d before now. A t after now (the
// clock went back) is within too: limits fail closed.
func within(t, now time.Time, d time.Duration) bool { return now.Sub(t) < d }

// admit is which of paths may be taken now: those none was accepted for
// in the last CheckpointPathEvery. full is set, and none admitted, when
// CheckpointRequestsPerHour requests were accepted in the last hour. A
// request with a path admitted is accepted, and counted.
func (c *CheckpointLimits) admit(paths []string) (ok []string, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.accepted = slices.DeleteFunc(c.accepted, func(t time.Time) bool { return !within(t, now, time.Hour) })
	for p, t := range c.last {
		if !within(t, now, CheckpointPathEvery) {
			delete(c.last, p)
		}
	}
	if len(c.accepted) >= CheckpointRequestsPerHour {
		return nil, true
	}
	for _, p := range paths {
		if _, seen := c.last[p]; !seen {
			ok = append(ok, p)
		}
	}
	if len(ok) == 0 {
		return nil, false
	}
	c.accepted = append(c.accepted, now)
	for _, p := range ok {
		c.last[p] = now
	}
	return ok, false
}

// logf logs to l at most checkpointLogMax lines a minute, saying how many
// it dropped once the minute is over: the sandbox decides how often it
// asks.
func (c *CheckpointLimits) logf(l *log.Logger, format string, args ...any) {
	if l == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.logFrom.IsZero() || !within(c.logFrom, now, checkpointLogEvery) || now.Before(c.logFrom) {
		if c.suppressed > 0 {
			l.Printf("checkpoint: ... and %d more lines not logged (at most %d a minute)", c.suppressed, checkpointLogMax)
		}
		c.logFrom, c.logged, c.suppressed = now, 0, 0
	}
	if c.logged >= checkpointLogMax {
		c.suppressed++
		return
	}
	c.logged++
	l.Printf(format, args...)
}

// checkpointsOn reports whether the link offers OpCheckpoint.
func (h *Host) checkpointsOn() bool { return h.cfg.Checkpoints != nil }

// errNoCheckpoints is a request for a checkpoint on a link that takes none.
var errNoCheckpoints = refuse(agentproto.CheckpointUnavailable,
	"this host takes no checkpoints for the sandbox: only a link the host's daemon runs does")

// unknownCheckpoint is the reason of a path that is no [checkpoints] path.
const unknownCheckpoint = "unknown path: not one of [checkpoints] paths in config.toml on the host"

// CheckCheckpointPaths is paths, what the sandbox sent, checked and
// normalised as config.toml's [checkpoints] entries are, duplicates
// dropped; or a refusal (CheckpointBad) saying why, quoting nothing
// unprintable.
func CheckCheckpointPaths(paths []string) ([]string, error) {
	if len(paths) > agentproto.MaxCheckpointPaths {
		return nil, refuse(agentproto.CheckpointBad, "%d paths asked for, at most %d", len(paths), agentproto.MaxCheckpointPaths)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if len(p) > agentproto.MaxCheckpointPath {
			return nil, refuse(agentproto.CheckpointBad, "a path of %d bytes is too long (at most %d)", len(p), agentproto.MaxCheckpointPath)
		}
		n, err := config.NormalizeCheckpointPath(p)
		if err != nil {
			return nil, refuse(agentproto.CheckpointBad, "%s", clip(proposal.Printable(err.Error()), 300))
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// checkpoint serves OpCheckpoint into r: the paths checked, decided,
// matched with [checkpoints] paths, limited, and taken; or a refusal of
// the whole, whose reason is one of the Checkpoint* codes.
func (h *Host) checkpoint(paths []string, r *agentproto.Message) error {
	if !h.checkpointsOn() {
		return errNoCheckpoints
	}
	lim := h.cpLimits()
	asked, err := CheckCheckpointPaths(paths)
	if err != nil {
		lim.logf(h.cfg.Log, "checkpoint: refused: %s", clip(err.Error(), 300))
		return err
	}
	o, _ := h.decider().Decide(policy.Request{Kind: policy.KindCheckpoint})
	if o.Verdict != broker.Allowed {
		lim.logf(h.cfg.Log, "checkpoint: refused: %s", clip(o.Reason, 300))
		return refuse(agentproto.CheckpointOff, "%s", o.Reason)
	}
	configured, err := h.cfg.Checkpoints.CheckpointPaths()
	if err != nil {
		lim.logf(h.cfg.Log, "checkpoint: refused: %s", clip(proposal.Printable(err.Error()), 300))
		return refuse(agentproto.CheckpointUnavailable, "the host cannot read its [checkpoints] paths now: %s", clip(proposal.Printable(err.Error()), 200))
	}
	if len(configured) == 0 {
		return refuse(agentproto.CheckpointUnavailable, "the host takes no checkpoints of this sandbox: config.toml on the host lists no [checkpoints] paths")
	}
	if len(asked) == 0 {
		asked = configured
	}
	reasons := map[string]string{}
	var known []string
	for _, p := range asked {
		if slices.Contains(configured, p) {
			known = append(known, p)
		} else {
			reasons[p] = unknownCheckpoint
		}
	}
	if !h.takeCheckpointSlot() {
		return refuse(agentproto.CheckpointLimit, "a checkpoint asked for earlier is still being taken; try again once it is done")
	}
	defer h.releaseCheckpointSlot()
	ok, full := lim.admit(known)
	if full {
		lim.logf(h.cfg.Log, "checkpoint: refused: %d requests in the last hour", CheckpointRequestsPerHour)
		return refuse(agentproto.CheckpointLimit, "at most %d checkpoint requests an hour are taken; try again later", CheckpointRequestsPerHour)
	}
	for _, p := range known {
		if !slices.Contains(ok, p) {
			reasons[p] = fmt.Sprintf("a checkpoint of it was asked for less than %s ago", CheckpointPathEvery)
		}
	}
	taken := map[string]agentproto.CheckpointTaken{}
	if len(ok) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), agentproto.CheckpointWait)
		defer cancel()
		go func() {
			select {
			case <-h.sess.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		for _, t := range h.cfg.Checkpoints.TakeCheckpoints(ctx, ok) {
			if slices.Contains(ok, t.Path) {
				taken[t.Path] = t
			}
		}
	}
	for _, p := range asked {
		t, ok := taken[p]
		switch {
		case ok && t.ID != "":
			t.Reason = ""
		case ok:
			t.Reason = clip(proposal.Printable(t.Reason), 500)
			if t.Reason == "" {
				t.Reason = "not taken"
			}
		case reasons[p] != "":
			t = agentproto.CheckpointTaken{Path: p, Reason: reasons[p]}
		default:
			t = agentproto.CheckpointTaken{Path: p, Reason: "the host did not take it in time"}
		}
		t.Path, t.ID = p, clip(proposal.Printable(t.ID), 200)
		r.Checkpoints = append(r.Checkpoints, t)
		if t.ID != "" {
			lim.logf(h.cfg.Log, "checkpoint %s: taken, %s", clip(p, 200), t.ID)
		} else {
			lim.logf(h.cfg.Log, "checkpoint %s: not taken: %s", clip(p, 200), clip(t.Reason, 300))
		}
	}
	return nil
}

// cpLimits are the link's checkpoint limits: the environment's, or the
// Host's own when its Config has none.
func (h *Host) cpLimits() *CheckpointLimits {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.CheckpointLimits == nil {
		h.cfg.CheckpointLimits = NewCheckpointLimits(nil)
	}
	return h.cfg.CheckpointLimits
}

// takeCheckpointSlot takes the link's one slot for a checkpoint request
// in progress.
func (h *Host) takeCheckpointSlot() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cpBusy {
		return false
	}
	h.cpBusy = true
	return true
}

func (h *Host) releaseCheckpointSlot() {
	h.mu.Lock()
	h.cpBusy = false
	h.mu.Unlock()
}
