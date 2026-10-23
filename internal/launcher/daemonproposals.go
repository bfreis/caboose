package launcher

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

var _ daemon.Proposals = (*DaemonLinks)(nil)

// proposalTextMax is the longest title, reason, summary, refusal or
// warning a proposal is listed with.
const proposalTextMax = 4096

// The daemon serves an environment's proposals on its App: listed and
// planned as caboose apply plans them (ProposalView), deleted and applied
// (ApplyPlanned; one that builds the image first by a job,
// daemonjob_apply.go) one at a time per environment. The files are read and
// removed through proposal.List and proposal.Remove, never following a
// link: the sandbox writes that directory.

// proposalLock is env's lock for deleting and applying its proposals.
func (l *DaemonLinks) proposalLock(env string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.proposalMu == nil {
		l.proposalMu = map[string]*sync.Mutex{}
	}
	m := l.proposalMu[env]
	if m == nil {
		m = &sync.Mutex{}
		l.proposalMu[env] = m
	}
	return m
}

// Proposals lists env's pending proposals (daemon.Proposals).
func (l *DaemonLinks) Proposals(_ context.Context, env string) (*daemonproto.ProposalsResult, error) {
	app, err := l.newApp(env)
	if err != nil {
		return nil, err
	}
	listed, more, err := listProposals(app)
	if err != nil {
		return nil, err
	}
	res := &daemonproto.ProposalsResult{Proposals: []daemonproto.ProposalInfo{}, More: more}
	for _, lp := range listed {
		res.Proposals = append(res.Proposals, lp.info)
	}
	return res, nil
}

// listedProposal is one proposal file and what the list says of it.
type listedProposal struct {
	entry proposal.Entry
	info  daemonproto.ProposalInfo
}

// listProposals is app's proposals as the list shows them, each with its
// file; more is set when there were more than the list holds. Deleting and
// applying go by it too, so that what they act on is what was shown,
// whether it may be applied from the page included.
func listProposals(app *App) (listed []listedProposal, more bool, err error) {
	entries, err := proposal.List(app.Cfg.DataDir)
	if err != nil {
		return nil, false, daemonproto.NewError(daemonproto.KindInternal, "reading the proposals: %s", proposal.Printable(err.Error()))
	}
	budget := daemonproto.MaxProposalsBytes
	for _, e := range entries {
		if len(listed) == daemonproto.MaxProposals {
			return listed, true, nil
		}
		v, err := app.ProposalView(e)
		if err != nil {
			return nil, false, err
		}
		info := proposalInfo(v, app.Cfg.Env, budget)
		for _, p := range info.Parts {
			for _, d := range p.Diff {
				budget -= len(d) + 1
			}
		}
		listed = append(listed, listedProposal{entry: e, info: info})
	}
	return listed, false, nil
}

// proposalInfo is v as the protocol carries it, its diff within budget
// bytes. A proposal whose diff the protocol cannot carry whole, within its
// own bounds or what is left of the list's, is listed without it, and
// cannot be applied from the page: nobody would have seen what it writes.
func proposalInfo(v ProposalView, env string, budget int) daemonproto.ProposalInfo {
	// cut is set when a text had to be cut short: what was shown is then
	// not all of it.
	cut := false
	txt := func(s string) string {
		if len(s) > proposalTextMax {
			cut = true
		}
		return daemonproto.Text(s, proposalTextMax)
	}
	info := daemonproto.ProposalInfo{
		File: daemonproto.Text(v.File, daemonproto.MaxProposalFile), Modified: v.Modified,
		Title: txt(v.Title), Reason: txt(v.Reason),
		Parts: []daemonproto.ProposalPart{}, Refusals: []string{}, Warnings: []string{},
		WebApply: v.WebApply, WebApplyWhy: daemonproto.Text(v.WebApplyWhy, proposalTextMax),
		Confirm:    daemonproto.ProposalConfirm{Kind: v.Confirm, Name: v.ConfirmName},
		NeedsBuild: v.NeedsBuild, Hash: v.Hash,
	}
	if len(v.ConfirmName) > daemonproto.MaxProposalTyped {
		info.Confirm.Name, cut = daemonproto.Text(v.ConfirmName, daemonproto.MaxProposalTyped), true
	}
	if !daemonproto.ValidProposalFile(info.File) {
		// A name the protocol cannot carry: shown as one it can, and
		// neither deleted nor applied by it, since it names no file.
		info.File = "unnamed.toml"
		info.WebApply, info.WebApplyWhy = false, "its file name cannot be shown here: "+ApplyCommand(env)+" in a terminal shows it"
	}
	bytes, lines, fits := 0, 0, len(v.Parts) <= daemonproto.MaxProposalParts
	for _, p := range v.Parts {
		for _, d := range p.Diff {
			bytes += len(d) + 1
			lines++
			if len(d) > daemonproto.MaxProposalDiff {
				fits = false
			}
		}
	}
	if bytes > daemonproto.MaxProposalDiff || bytes > budget || lines > daemonproto.MaxProposalDiffLines {
		fits = false
	}
	for i, p := range v.Parts {
		if i == daemonproto.MaxProposalParts {
			break
		}
		pp := daemonproto.ProposalPart{Kind: p.Kind, Summary: txt(p.Summary), Diff: []string{}}
		if fits {
			pp.Diff = append(pp.Diff, p.Diff...)
		}
		info.Parts = append(info.Parts, pp)
	}
	if !fits {
		info.WebApply = false
		info.WebApplyWhy = "it is too long to show here: " + ApplyCommand(env) + " in a terminal shows it whole"
	}
	notices := 0
	for _, s := range v.Refusals {
		if notices < daemonproto.MaxProposalNotices {
			info.Refusals = append(info.Refusals, txt(s))
			notices++
		}
	}
	for _, s := range v.Warnings {
		if notices < daemonproto.MaxProposalNotices {
			info.Warnings = append(info.Warnings, txt(s))
			notices++
		} else {
			cut = true
		}
	}
	if cut && info.WebApply {
		info.WebApply, info.WebApplyWhy = false, "some of it is too long to show here: "+ApplyCommand(env)+" in a terminal shows it whole"
	}
	if len(info.Parts) == 0 && info.WebApply {
		info.WebApply, info.WebApplyWhy = false, "there is nothing in it to apply"
	}
	return info
}

// shownProposal is env's proposal file as the list shows it: the one of
// that name, or else the one whose name, made printable, is file (as the
// list showed it), when only one is; and the list's hash of it must be
// hash, or the file, or what it applies to, changed since it was shown.
func shownProposal(app *App, file, hash string) (listedProposal, error) {
	listed, _, err := listProposals(app)
	if err != nil {
		return listedProposal{}, err
	}
	var shown []listedProposal
	found := false
	var exact listedProposal
	for _, lp := range listed {
		if lp.entry.File == file {
			exact, found = lp, true
			break
		}
		if proposal.Printable(lp.entry.File) == file {
			shown = append(shown, lp)
		}
	}
	if !found {
		if len(shown) != 1 {
			return listedProposal{}, daemonproto.NewError(daemonproto.KindNotFound, "no pending proposal %s in environment %s", file, app.Cfg.Env)
		}
		exact = shown[0]
	}
	if exact.info.Hash != hash {
		return listedProposal{}, daemonproto.NewError(daemonproto.KindStaleDiff, "the proposal, or what it applies to, changed since it was shown: nothing was done")
	}
	return exact, nil
}

// ProposalDelete deletes one of env's proposals (daemon.Proposals).
func (l *DaemonLinks) ProposalDelete(_ context.Context, env, file, hash string) error {
	app, err := l.newApp(env)
	if err != nil {
		return err
	}
	mu := l.proposalLock(env)
	mu.Lock()
	defer mu.Unlock()
	lp, err := shownProposal(app, file, hash)
	if err != nil {
		return err
	}
	if err := proposal.Remove(app.Cfg.DataDir, lp.entry.File); err != nil {
		return daemonproto.NewError(daemonproto.KindInternal, "deleting %s: %s", file, proposal.Printable(err.Error()))
	}
	l.logf("environment %s: deleted the proposal %s", env, file)
	return nil
}

// ProposalApply applies one of env's proposals as it was shown
// (daemon.Proposals), then has env's broker read config.toml again when
// this daemon runs it.
func (l *DaemonLinks) ProposalApply(_ context.Context, p daemonproto.ProposalApplyParams) (*daemonproto.ProposalApplyResult, error) {
	app, err := l.newApp(p.Env)
	if err != nil {
		return nil, err
	}
	mu := l.proposalLock(p.Env)
	mu.Lock()
	defer mu.Unlock()
	lp, err := shownProposal(app, p.File, p.Hash)
	if err != nil {
		return nil, err
	}
	// What the list says may not be applied from the page is not, though
	// the plan alone would allow it: a diff too long to show, a text cut
	// short or a name it cannot carry was never seen whole.
	if !lp.info.WebApply {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "not applied from the page: %s", lp.info.WebApplyWhy)
	}
	notes, err := app.ApplyPlanned(lp.entry, p.Hash, p.Typed)
	if err != nil {
		return nil, err
	}
	l.logf("environment %s: applied the proposal %s from %s", p.Env, p.File, filepath.Join(app.Cfg.DataDir, proposal.Dir))
	notes = append(notes, l.reloadBroker(p.Env)...)
	res := &daemonproto.ProposalApplyResult{Applied: true, Notes: []string{}}
	for _, n := range notes {
		if len(res.Notes) < daemonproto.MaxProposalNotes {
			res.Notes = append(res.Notes, daemonproto.Text(n, proposalTextMax))
		}
	}
	return res, nil
}
