package launcher

import (
	"fmt"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// ckStale is how old a path's newest checkpoint may be, while the sandbox
// runs, before it counts as failing: two and a half intervals.
const ckStale = 25 * time.Minute

// ckSeams are what doctor and status take from the machine: its
// filesystems, the clock, and the Apps of the other environments (nil:
// envApp). A struct, so that a test gives them what it likes.
type ckSeams struct {
	fs   ckFS
	now  func() time.Time
	apps func(env string) (*App, error)
}

func defaultCkSeams() ckSeams { return ckSeams{fs: hostCkFS, now: time.Now} }

// ckPathHealth is one configured path as the store and the resolution
// show it, with the daemon's account of it when there is one.
type ckPathHealth struct {
	Path, Key string
	Resolved  bool
	Reason    string            // why not, when not resolved
	Source    checkpoint.Source // when resolved
	// Newest is the newest readable checkpoint's time and method.
	Newest    time.Time
	HasNewest bool
	Method    string
	// Unreadable counts checkpoints whose meta.json cannot be used.
	Unreadable int
	// Shrunk is the earlier checkpoint to restore from while a sharp
	// shrink stands.
	Shrunk string
	// Failing is the daemon's account of the last take.
	Failing string
	// Count is how many checkpoints of the path are kept.
	Count int
	// Pinned are the checkpoints kept outside retention after a sharp
	// shrink, each with the later one that shrank from it.
	Pinned []ckPin
	// PinsError says why the store's pins cannot be read.
	PinsError string
}

// ckPin is a pinned checkpoint, and the later one that shrank from it.
type ckPin struct{ ID, By string }

// ckHealth is the checkpoints of an environment as doctor and status see
// them: read from the store (never made for it) and the resolution, not
// from the daemon, which adds only what the store does not hold.
type ckHealth struct {
	Paths   []ckPathHealth
	Orphans []daemonproto.CheckpointListed
	Sched   *daemonproto.CheckpointStatus
	Now     time.Time
}

// checkpointHealth reads the environment's checkpoints. sched is the
// daemon's account, nil when it gave none.
func (a *App) checkpointHealth(sched *daemonproto.CheckpointStatus, now time.Time) (*ckHealth, error) {
	srcs, bad := a.checkpointSources()
	st := &checkpoint.Status{}
	srcOf := map[string]checkpoint.Source{}
	for _, s := range srcs {
		st.Paths = append(st.Paths, checkpoint.PathStatus{Path: s.Path, Checked: true, Resolved: true})
		srcOf[s.Path] = s
	}
	for _, b := range bad {
		st.Paths = append(st.Paths, checkpoint.PathStatus{Path: b.Path, Checked: true, Reason: b.Reason})
	}
	res, err := a.checkpointListing(st)
	if err != nil {
		return nil, err
	}
	h := &ckHealth{Orphans: res.Orphans, Sched: sched, Now: now}
	byPath := map[string]daemonproto.CheckpointPathStatus{}
	if sched != nil {
		for _, p := range sched.Paths {
			byPath[p.Path] = p
		}
	}
	for _, l := range res.Paths {
		p := ckPathHealth{Path: l.Path, Key: l.Key, Resolved: l.Resolved, Reason: l.Reason, Shrunk: l.Shrunk,
			Source: srcOf[l.Path], Count: len(l.Checkpoints), Failing: byPath[l.Path].Failing}
		p.PinsError = l.PinsError
		for _, v := range l.Checkpoints {
			if v.Pinned {
				p.Pinned = append(p.Pinned, ckPin{ID: v.ID, By: v.PinnedBy})
			}
			if v.Error != "" {
				p.Unreadable++
				continue
			}
			if p.HasNewest {
				continue
			}
			t := v.Finished
			if t.IsZero() {
				t = v.Started
			}
			if t.IsZero() {
				if id, err := checkpoint.ParseID(v.ID); err == nil {
					t = id.Stamp
				}
			}
			p.Newest, p.HasNewest, p.Method = t, !t.IsZero(), v.Method
		}
		h.Paths = append(h.Paths, p)
	}
	return h, nil
}

// failures are the reasons checkpoints are failing now, one per cause, the
// worst first: a store a sandbox can reach, a path with nothing to copy, a
// take that failed, a newest checkpoint too old while the sandbox runs.
// Below the soft floor the schedule skips its checkpoints: that is the
// reason, given once, in place of each path's newest growing old. Below
// the hard floor, the take fails, and its path's Failing says so.
func (h *ckHealth) failures(running bool) []string {
	var out []string
	if h.Sched != nil && h.Sched.Exposed != "" {
		out = append(out, "no checkpoint is taken: "+h.Sched.Exposed)
	}
	skipped := h.Sched != nil && h.Sched.SpaceSkipped != ""
	if skipped {
		out = append(out, "scheduled checkpoints are skipped: the disk is below its soft floor ("+
			strings.TrimPrefix(h.Sched.SpaceSkipped, checkpoint.ErrLowSpace.Error()+": ")+")")
	}
	for _, p := range h.Paths {
		switch {
		case !p.Resolved:
			out = append(out, fmt.Sprintf("%s is not copied: %s", p.Path, p.Reason))
		case p.Failing != "":
			out = append(out, fmt.Sprintf("%s: %s", p.Path, p.Failing))
		case running && !skipped && p.HasNewest && h.Now.Sub(p.Newest) > ckStale:
			out = append(out, fmt.Sprintf("%s: the newest checkpoint is %s", p.Path, ckAgo(p.Newest, h.Now)))
		}
	}
	return out
}
