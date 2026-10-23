package launcher

import (
	"fmt"
	"strings"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// checkpointLine is status's line for the checkpoints, "" when no paths
// are set: how many, and the newest's age and method, or how many fail and
// the first reason (the schedule skipping them below the soft floor
// among them), a disk low on space, and the checkpoints pinned after a
// sharp shrink. It reads the store and the daemon's status as lv
// already holds it; it starts nothing and makes no store.
func (a *App) checkpointLine(lv linkView, running bool, s ckSeams) string {
	n := len(a.Cfg.Checkpoints)
	if n == 0 {
		return ""
	}
	var sched *daemonproto.CheckpointStatus
	if lv.Env != nil {
		sched = lv.Env.Checkpoints
	}
	now := s.now()
	head := fmt.Sprintf("%d %s", n, plural(n, "path", "paths"))
	h, err := a.checkpointHealth(sched, now)
	if err != nil {
		return head + ", 1 failing: cannot read the store: " + shortReason(err.Error())
	}
	if f := h.failures(running); len(f) > 0 {
		return fmt.Sprintf("%s, %d failing: %s", head, len(f), shortReason(f[0]))
	}
	low := ""
	if sched != nil && sched.LowSpace {
		low = "; the disk is low on space"
	}
	pinned := 0
	for _, p := range h.Paths {
		pinned += len(p.Pinned)
	}
	if pinned > 0 {
		low += fmt.Sprintf("; %d pinned after a sharp shrink (caboose checkpoint list)", pinned)
	}
	best := -1
	for i, p := range h.Paths {
		if p.HasNewest && (best < 0 || p.Newest.After(h.Paths[best].Newest)) {
			best = i
		}
	}
	if best < 0 {
		return head + ", none taken yet" + low
	}
	p := h.Paths[best]
	return fmt.Sprintf("%s, newest %s (%s)%s", head, ckAgo(p.Newest, now), proposal.Printable(p.Method), low)
}

// shortReason is a failure's reason for a line of status: printable, on
// one line, cut short.
func shortReason(s string) string {
	s = strings.Join(strings.Fields(proposal.Printable(s)), " ")
	if len(s) > 160 {
		s = strings.ToValidUTF8(s[:157], "") + "..."
	}
	return s
}
