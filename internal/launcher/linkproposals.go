package launcher

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/proposal"
)

// While the container runs, the link tells whoever is at the host when a
// session writes a proposal, with a notification naming the apply command:
// a session can only ask the user to run it, and the user may not be
// looking at that session. The notification is all it does. It never
// applies anything, nor runs or opens anything a proposal holds; what it
// shows of one -- its title, else its file's name -- is the sandbox's,
// and goes through hostlink.Notify, which makes it printable and cuts it
// short. The proposals dir is the container's to write, so it is read
// through nofollow only (proposal.Stamps, proposal.Read).
const (
	// proposalPollEvery is how often the link lists the proposals dir:
	// names and modification times, nothing read.
	proposalPollEvery = 3 * time.Second
	// proposalNotifyEvery is the least time between two notifications,
	// however many proposals arrive: those that come in between are named
	// in the next one.
	proposalNotifyEvery = 30 * time.Second
	// proposalNotifyNames is the most proposals one notification names.
	proposalNotifyNames = 3
)

// proposalWatch notices proposals written since the link started.
type proposalWatch struct {
	dataDir, env string
	// seen are the files as last told of, or as found at the start, which
	// the launch that started the link has already told of; prev are the
	// files as the last poll found them.
	seen, prev map[string]time.Time
	last       time.Time // the last notification
}

func newProposalWatch(dataDir, env string) *proposalWatch {
	w := &proposalWatch{dataDir: dataDir, env: env}
	w.seen = w.stamps()
	w.prev = w.seen
	return w
}

func (w *proposalWatch) stamps() map[string]time.Time {
	m := map[string]time.Time{}
	stamps, _ := proposal.Stamps(w.dataDir)
	for _, s := range stamps {
		m[s.File] = s.Modified
	}
	return m
}

// check polls the dir once and returns the notification due now, if any.
// A file counts once two polls in a row find it unchanged, so one still
// being written is not read half done; one rewritten since it was told of
// counts again.
func (w *proposalWatch) check(now time.Time) (title, text string, ok bool) {
	cur := w.stamps()
	var fresh []string
	for f, m := range cur {
		if s, told := w.seen[f]; told && s.Equal(m) {
			continue
		}
		if p, polled := w.prev[f]; polled && p.Equal(m) {
			fresh = append(fresh, f)
		}
	}
	for f := range w.seen {
		if _, ok := cur[f]; !ok {
			delete(w.seen, f) // applied or deleted: a new one of that name is new
		}
	}
	w.prev = cur
	if len(fresh) == 0 || !w.last.IsZero() && now.Sub(w.last) < proposalNotifyEvery {
		return "", "", false
	}
	for _, f := range fresh {
		w.seen[f] = cur[f]
	}
	w.last = now
	return proposalNotice(w.dataDir, w.env, fresh)
}

// proposalNotice is the notification for the proposal files fresh.
func proposalNotice(dataDir, env string, fresh []string) (title, text string, ok bool) {
	slices.Sort(fresh)
	var names []string
	for i, f := range fresh {
		if i == proposalNotifyNames {
			names = append(names, fmt.Sprintf("and %d more", len(fresh)-i))
			break
		}
		if p, err := proposal.Read(dataDir, f); err == nil {
			names = append(names, p.Title)
		} else {
			names = append(names, strings.TrimSuffix(f, proposal.Ext)+" (not one caboose can apply)")
		}
	}
	title = "caboose: a session proposes a change"
	if len(fresh) > 1 {
		title = fmt.Sprintf("caboose: sessions propose %d changes", len(fresh))
	}
	text = fmt.Sprintf("%s. Run '%s' in a terminal to review %s.", strings.Join(names, "; "),
		ApplyCommand(env), plural(len(fresh), "it", "them"))
	return title, text, true
}

// watchProposals notifies of new proposals until stop.
func (r *linkRunner) watchProposals(stop <-chan struct{}) {
	w := newProposalWatch(r.a.Cfg.DataDir, r.a.Cfg.Env)
	t := time.NewTicker(proposalPollEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		title, text, ok := w.check(time.Now())
		if !ok {
			continue
		}
		r.mu.Lock()
		actions := r.cfg.Actions
		r.mu.Unlock()
		if err := hostlink.Notify(actions, title, text); err != nil {
			r.log.Printf("cannot show that a session proposed a change: %v", err)
			continue
		}
		r.log.Printf("told of new proposals")
	}
}
