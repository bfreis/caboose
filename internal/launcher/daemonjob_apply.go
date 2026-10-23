package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// A proposal whose image is built first (ProposalView's NeedsBuild: its
// packages, alone or with a root), applied from the page. proposal_build
// checks it as proposal_apply does, under the environment's proposal
// lock: the hash of what was shown, that the list lets the page apply it,
// the name typed. A stale hash or a wrong name is answered at once, and
// no job starts. Then a job of the environment's (daemonjobs.go: one at a
// time, busy otherwise) does what caboose apply does: the image is built
// on a copy of the configuration before anything is written; then, under
// the lock again, the proposal is planned on a fresh App and written only
// when it still hashes as shown, config.toml's profile is what the build
// took and the root's edit diffs as it did, in one config.toml write; the
// lock the build resolved after it. A build that fails, or anything that
// changed meanwhile, writes nothing and leaves the proposal pending. The
// sandbox keeps the image it runs until its next restart.

// ProposalBuild starts the job that applies one of p.Env's proposals
// that builds the image first, as it was shown (daemon.Proposals).
func (l *DaemonLinks) ProposalBuild(_ context.Context, p daemonproto.ProposalApplyParams) (*daemonproto.JobInfo, error) {
	e, err := l.jobEnv(p.Env)
	if err != nil {
		return nil, err
	}
	app, err := l.newApp(p.Env)
	if err != nil {
		return nil, err
	}
	mu := l.proposalLock(p.Env)
	mu.Lock()
	defer mu.Unlock()
	lp, v, err := buildable(app, p)
	if err != nil {
		return nil, err
	}
	info := daemonproto.JobInfo{Kind: daemonproto.JobApply, Proposal: lp.info.File}
	return l.startJob(e, app, info, func(j *envJob, out *cappedlog.Log) { l.runApplyJob(e, j, app, out, p, v) })
}

// buildable is p's proposal as the list shows it and as app plans it,
// when proposal_build may apply it: what was shown (its hash), one the
// list lets the page apply, one that builds the image, the name typed
// when it takes one.
func buildable(app *App, p daemonproto.ProposalApplyParams) (listedProposal, ProposalView, error) {
	lp, err := shownProposal(app, p.File, p.Hash)
	if err != nil {
		return listedProposal{}, ProposalView{}, err
	}
	if !lp.info.WebApply {
		return listedProposal{}, ProposalView{}, daemonproto.NewError(daemonproto.KindBadRequest, "not applied from the page: %s", lp.info.WebApplyWhy)
	}
	if !lp.info.NeedsBuild {
		return listedProposal{}, ProposalView{}, daemonproto.NewError(daemonproto.KindBadRequest, "it builds no image: %s applies it", daemonproto.MethodProposalApply)
	}
	v, err := app.ProposalView(lp.entry)
	if err != nil {
		return listedProposal{}, ProposalView{}, err
	}
	if v.Hash != p.Hash {
		return listedProposal{}, ProposalView{}, daemonproto.NewError(daemonproto.KindStaleDiff, "the proposal, or what it applies to, changed since it was shown: nothing was done")
	}
	if !v.WebApply || !v.NeedsBuild || v.plan.packages == nil {
		return listedProposal{}, ProposalView{}, daemonproto.NewError(daemonproto.KindBadRequest, "not applied from the page: %s", v.WebApplyWhy)
	}
	if err := v.checkTyped(p.Typed); err != nil {
		return listedProposal{}, ProposalView{}, err
	}
	return lp, v, nil
}

// runApplyJob runs the apply job j on app: v's image built, then v
// written as above; then records how it ended and tells the user.
func (l *DaemonLinks) runApplyJob(e *envLink, j *envJob, app *App, out *cappedlog.Log, p daemonproto.ProposalApplyParams, v ProposalView) {
	env, file := e.name, j.info.Proposal
	lg := log.New(out, "", log.LstdFlags)
	lg.Printf("apply %s: started, the proposal %s", env, file)
	lines := &jobLines{e: e, j: j}
	w := io.MultiWriter(out, lines)
	jobOutput(app, w)
	app.noTerminal = fmt.Errorf("this needs a terminal: run `%s`", ApplyCommand(env))
	notes, err := l.applyBuilt(app, p, v, w)
	lines.flush()
	text := fmt.Sprintf("%s: applied the proposal %s. The image is built and config.toml written; the sandbox keeps its current image until its next restart (%s)",
		env, file, envCommand(env, "restart"))
	if err != nil {
		text = fmt.Sprintf("applying the proposal %s to %s failed: ", file, env)
	} else {
		l.logf("environment %s: applied the proposal %s, its image built", env, file)
	}
	l.endJob(e, j, out, lg, err, notes, text)
}

// jobOutput has what app says, and what docker says under it, go to w.
func jobOutput(app *App, w io.Writer) {
	app.Stdout, app.Stderr = w, w
	if app.Docker != nil {
		d := *app.Docker
		d.Stderr = w
		app.Docker = &d
	}
}

// applyBuilt builds v's image on app, then, nothing having changed, writes
// it; notes say what was done and what is left. A panic is the job's
// failure, not the daemon's.
func (l *DaemonLinks) applyBuilt(app *App, p daemonproto.ProposalApplyParams, v ProposalView, w io.Writer) (notes []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			notes, err = nil, fmt.Errorf("internal error: %v", r)
		}
	}()
	pl := v.plan.packages
	fmt.Fprintf(w, "Building the image with the packages of [%s] first: nothing is written unless it builds.\n", pl.profile)
	writeLock, err := app.buildPackages(pl)
	if err != nil {
		return nil, fmt.Errorf("the build failed (%v): nothing was written, and the proposal is left pending", err)
	}
	fmt.Fprintf(w, "Built image '%s'.\n", app.Cfg.Image)

	mu := l.proposalLock(p.Env)
	mu.Lock()
	defer mu.Unlock()
	const unchanged = "nothing was written, and the proposal is left pending"
	now, err := l.newApp(p.Env)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read again (%v): %s", p.Env, err, unchanged)
	}
	jobOutput(now, w)
	now.noTerminal = app.noTerminal
	lp, cur, err := buildable(now, p)
	switch {
	case daemonproto.Kind(err) == daemonproto.KindStaleDiff:
		return nil, fmt.Errorf("the proposal, or what it applies to, changed while the image was built: %s", unchanged)
	case daemonproto.Kind(err) == daemonproto.KindNotFound:
		return nil, errors.New("the proposal was deleted while the image was built: nothing was written")
	case err != nil:
		return nil, fmt.Errorf("%s: %s", err.Error(), unchanged)
	}
	if why := now.packagesChanged(pl); why != "" {
		return nil, fmt.Errorf("%s: %s", why, unchanged)
	}
	var edit config.Edit
	if cur.edit != nil {
		if d, err := now.configDiff(*cur.edit); err != nil || d != cur.editDiff {
			return nil, fmt.Errorf("config.toml changed while the image was built: %s", unchanged)
		}
		edit = *cur.edit
	}
	addPackages(pl, &edit)
	if now.beforeProposalWrite != nil {
		now.beforeProposalWrite()
	}
	if _, err := now.writeConfig(edit); err != nil {
		return nil, fmt.Errorf("%v: %s", err, unchanged)
	}
	fmt.Fprintln(w, "Wrote config.toml.")
	c := now.Cfg
	cfgPath := now.short(filepath.Join(c.EnvDir, config.FileName))
	// The lock only now: it is of the packages config.toml has.
	if writeLock != nil {
		if err := writeLock(); err != nil {
			notes = append(notes, fmt.Sprintf("The lock could not be written (%v): the next build resolves the packages again.", err))
		}
	}
	notes = append(notes, fmt.Sprintf("Built the image with the packages of [%s] and wrote them to %s. The %s keeps its current image until its next restart (%s, which ends running sessions).",
		pl.profile, cfgPath, now.noun(), envCommand(c.Env, "restart")))
	if cur.plan.root != nil {
		notes = append(notes, now.rootNote(cur.plan))
	}
	// What a proposal is made against now is what was written.
	if err := now.rereadConfigFile(); err != nil {
		notes = append(notes, fmt.Sprintf("config.toml could not be read back (%s): sessions are told what a proposal is made against at the next launch.", proposal.Printable(err.Error())))
	} else if err := now.Cfg.ReadImage(); err != nil {
		notes = append(notes, fmt.Sprintf("The image profile could not be read back (%s): sessions are told what a proposal is made against at the next launch.", proposal.Printable(err.Error())))
	}
	notes = append(notes, now.removeApplied(lp.entry)...)
	return append(notes, l.reloadBroker(p.Env)...), nil
}

// reloadBroker has env's broker read config.toml again when this daemon
// runs it; the note says when it could not.
func (l *DaemonLinks) reloadBroker(env string) []string {
	h, err := l.brokerOf(env)
	if err != nil {
		return nil
	}
	if err := h.b.Reload(); err != nil {
		return []string{"The link's policy could not read config.toml again (" + proposal.Printable(err.Error()) + "): it does within a minute."}
	}
	return nil
}

// ansiRe is a terminal's escape sequence, which a build's output may hold.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// jobLines keeps the last lines j's output said, in j's Lines, each cut
// short and printable, for status to show as it goes.
type jobLines struct {
	e *envLink
	j *envJob

	mu   sync.Mutex
	part []byte // the line being written, at most maxJobPart bytes of it
}

// maxJobPart is the most of one line that is kept before its end.
const maxJobPart = 4 * daemonproto.MaxJobLine

func (w *jobLines) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(b)
	var done []string
	for len(b) > 0 {
		i := bytes.IndexAny(b, "\r\n")
		if i < 0 {
			w.keep(b)
			break
		}
		w.keep(b[:i])
		done = append(done, string(w.part))
		w.part = w.part[:0]
		b = b[i+1:]
	}
	w.add(done)
	return n, nil
}

func (w *jobLines) keep(b []byte) {
	if room := maxJobPart - len(w.part); room > 0 {
		w.part = append(w.part, b[:min(room, len(b))]...)
	}
}

// flush adds the line being written, once nothing more is.
func (w *jobLines) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.part) > 0 {
		w.add([]string{string(w.part)})
		w.part = w.part[:0]
	}
}

// add adds lines to j's, each made one printable line, the blank ones
// dropped, keeping the last daemonproto.MaxJobLines. Lines is replaced,
// never changed in place: status holds copies of it.
func (w *jobLines) add(lines []string) {
	var keep []string
	for _, l := range lines {
		l = strings.TrimSpace(proposal.Printable(strings.ReplaceAll(ansiRe.ReplaceAllString(l, ""), "\t", " ")))
		if l != "" {
			keep = append(keep, daemonproto.Text(l, daemonproto.MaxJobLine))
		}
	}
	if len(keep) == 0 {
		return
	}
	w.e.mu.Lock()
	defer w.e.mu.Unlock()
	all := append(slices.Clone(w.j.info.Lines), keep...)
	if len(all) > daemonproto.MaxJobLines {
		all = all[len(all)-daemonproto.MaxJobLines:]
	}
	w.j.info.Lines = all
}
