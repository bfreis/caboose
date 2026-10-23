package launcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// A proposal as data, and applied without a prompter: what the daemon
// shows the web page, and writes once the page confirms. caboose apply
// plans with the same functions (planProposal, planRootChange,
// configDiff) and writes with the same writers (writeDockerfile,
// writeConfig); what it asks and says stays its own.
//
// A proposal is applied whole, and with one write: one part, or packages
// with a root, which go into config.toml as one edit. Packages build the
// image before anything is written (NeedsBuild), which takes minutes, so
// the page applies such a proposal through proposal_build, as a job of
// the environment's (daemonjob_apply.go), which plans it again once the
// image is built and writes only if it is still what was shown; every
// other proposal is ApplyPlanned's, at once.

// The kinds of a proposal's part.
const (
	PartSection  = daemonproto.ProposalPartSection
	PartPackages = daemonproto.ProposalPartPackages
	PartRoot     = daemonproto.ProposalPartRoot
	PartAlias    = daemonproto.ProposalPartAlias
	PartWritable = daemonproto.ProposalPartWritable
)

// The confirmations a proposal takes beyond the confirmation itself.
const (
	ConfirmNone     = daemonproto.ProposalConfirmNone
	ConfirmTypeName = daemonproto.ProposalConfirmTypeName // the root's or the alias's name typed (a writable part's root's), as caboose apply asks
)

// ProposalPart is one change a proposal makes: the literal lines of its
// diff (lineDiff's, made printable, tabs as spaces) and a summary in the
// host's words.
type ProposalPart struct {
	Kind    string   `json:"kind"`
	Diff    []string `json:"diff"`
	Summary string   `json:"summary"`
}

// ProposalView is a proposal as the host would apply it, as data. Title
// and Reason are the sandbox's text; everything else is the host's.
type ProposalView struct {
	File          string
	Modified      time.Time
	Title, Reason string
	Parts         []ProposalPart
	Refusals      []string
	Warnings      []string
	// WebApply is whether it may be applied from the web page; WebApplyWhy
	// says why not when it may not.
	WebApply    bool
	WebApplyWhy string
	// Confirm is ConfirmNone or ConfirmTypeName, with the name to type.
	Confirm     string
	ConfirmName string
	// NeedsBuild is set when applying it builds the image first.
	NeedsBuild bool
	// Hash is the sha256 of what is shown of it (hashView).
	Hash string

	plan plan
	// edit is the config.toml edit it makes, and diff that edit's diff
	// when planned; nil when it makes none.
	edit     *config.Edit
	editDiff string
}

// viewLines are lineDiff's d as a view's lines: printable, tabs made
// spaces, as daemonproto.DiffLines makes a config.toml diff.
func viewLines(d string) []string {
	var out []string
	if d = strings.TrimSuffix(d, "\n"); d != "" {
		for _, l := range strings.Split(d, "\n") {
			out = append(out, proposal.Printable(strings.ReplaceAll(l, "\t", " ")))
		}
	}
	return out
}

// hashView is the sha256 of what a person is shown of v: its title,
// reason and parts, each part's kind, diff lines and summary, in JSON,
// whose quoting leaves no two of them able to run together.
func hashView(v *ProposalView) string {
	data, _ := json.Marshal(struct {
		Title  string         `json:"title"`
		Reason string         `json:"reason"`
		Parts  []ProposalPart `json:"parts"`
	}{v.Title, v.Reason, v.Parts})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ProposalView plans e as caboose apply would, and says it as data. An
// entry that is no proposal, and one with refusals, come back with them
// and WebApply false.
func (a *App) ProposalView(e proposal.Entry) (v ProposalView, err error) {
	v = ProposalView{File: proposal.Printable(e.File), Modified: e.Modified, Confirm: ConfirmNone}
	defer func() { v.Hash = hashView(&v) }()
	if e.Err != nil || e.Proposal == nil {
		why := "unreadable"
		if e.Err != nil {
			why = e.Err.Error()
		}
		v.Refusals = []string{"This is no proposal caboose can apply: " + proposal.Printable(why)}
		v.WebApplyWhy = "caboose cannot apply it: delete it, or ask the session to propose it again"
		return v, nil
	}
	pr := e.Proposal
	// A reason may run over lines, which the page shows as one.
	oneLine := strings.NewReplacer("\r\n", " ", "\n", " ", "\t", " ")
	v.Title, v.Reason = proposal.Printable(oneLine.Replace(pr.Title)), proposal.Printable(oneLine.Replace(pr.Reason))
	pl, refusals, warnings := a.planProposal(pr)
	v.plan, v.Refusals, v.Warnings = pl, refusals, warnings
	if len(refusals) == 0 {
		a.viewParts(&v)
	}
	switch {
	case len(v.Refusals) > 0:
		v.WebApplyWhy = "caboose refuses it as it is: delete it, or ask the session for another"
	case pl.empty():
		v.WebApplyWhy = "there is nothing left in it to apply"
	case len(v.Parts) > 1 && !packagesWithRoot(pl, v.Parts):
		// Its writes could not all be checked, and made, as one: a file
		// changed between them would leave it half applied.
		v.WebApplyWhy = "it changes more than one thing: " + ApplyCommand(a.Cfg.Env) + " in a terminal applies it"
	default:
		v.WebApply = true
	}
	return v, nil
}

// packagesWithRoot reports whether parts are pl's packages and its root,
// and nothing else: the one pair config.toml takes as one edit.
func packagesWithRoot(pl plan, parts []ProposalPart) bool {
	return len(parts) == 2 && pl.packages != nil && pl.root != nil && pl.dockerfile == nil && pl.alias == nil && pl.writable == nil
}

// viewParts fills v's parts from its plan, and what applying them takes.
func (a *App) viewParts(v *ProposalView) {
	c, pl := a.Cfg, v.plan
	if pl.dockerfile != nil {
		path := a.short(filepath.Join(c.ImageProfile.Dir, "Dockerfile"))
		sum := fmt.Sprintf("the section %s in %s", proposal.Printable(v.sectionName()), path)
		if pl.source == proposal.SourceSeed {
			sum = fmt.Sprintf("caboose's Dockerfile with the section %s, written as %s", proposal.Printable(v.sectionName()), path)
		}
		v.Parts = append(v.Parts, ProposalPart{Kind: PartSection, Diff: viewLines(lineDiff(string(pl.oldDockerfile), string(pl.dockerfile))), Summary: sum})
	}
	if pk := pl.packages; pk != nil {
		var lines []string
		for _, n := range pk.add {
			lines = append(lines, "+ "+proposal.Printable(n))
		}
		for _, n := range pk.drop {
			lines = append(lines, "- "+proposal.Printable(n))
		}
		v.Parts = append(v.Parts, ProposalPart{Kind: PartPackages, Diff: lines,
			Summary: fmt.Sprintf("the packages of [%s] in %s", pk.profile, a.short(filepath.Join(c.EnvDir, config.FileName)))})
		v.NeedsBuild = true
	}
	if al := pl.alias; al != nil {
		var words []string
		for _, w := range al.alias.Command {
			words = append(words, proposal.Printable(w))
		}
		sum := fmt.Sprintf("a host command the sandbox may run with caboose-agent run %s: %s", al.alias.Name, strings.Join(words, " "))
		if al.command != al.alias.Command[0] {
			sum += ", which is " + proposal.Printable(al.command) + " now"
		}
		dir := "a fresh empty directory"
		if al.alias.Cwd != "" {
			dir = proposal.Printable(al.alias.Cwd)
		}
		sum += ", in " + dir
		for _, k := range slices.Sorted(maps.Keys(al.alias.Params)) {
			sum += fmt.Sprintf("; {%s} is one of %s", k, proposal.Printable(strings.Join(al.alias.Params[k], ", ")))
		}
		edit := al.edit
		v.edit, v.editDiff = &edit, al.diff
		v.Parts = append(v.Parts, ProposalPart{Kind: PartAlias, Diff: viewLines(al.diff), Summary: sum})
		v.Confirm, v.ConfirmName = ConfirmTypeName, al.alias.Name
	}
	if wp := pl.writable; wp != nil {
		edit := wp.edit
		v.edit, v.editDiff = &edit, wp.diff
		v.Parts = append(v.Parts, ProposalPart{Kind: PartWritable, Diff: viewLines(wp.diff), Summary: a.writableSummary(wp)})
		v.Confirm, v.ConfirmName = ConfirmTypeName, wp.root.Name
	}
	if pl.root != nil {
		rc := a.planRootChange(pl)
		if rc.err != nil {
			v.Refusals = append(v.Refusals, fmt.Sprintf("The roots cannot take it: %v", rc.err))
			return
		}
		edit := rootsEdit(rc.roots)
		d, err := a.configDiff(edit)
		if err != nil {
			v.Refusals = append(v.Refusals, fmt.Sprintf("It cannot be written into config.toml safely: %s.", proposal.Printable(err.Error())))
			return
		}
		v.edit, v.editDiff = &edit, d
		hostShown := proposal.Printable(pl.host)
		sum := fmt.Sprintf("mount %s read-only at %s/%s", hostShown, config.WorkDir, pl.root.Name)
		if proposal.Printable(a.short(pl.host)) != proposal.Printable(pl.root.Path) && hostShown != proposal.Printable(pl.root.Path) {
			sum += ", proposed as " + proposal.Printable(pl.root.Path)
		}
		v.Parts = append(v.Parts, ProposalPart{Kind: PartRoot, Diff: viewLines(d), Summary: sum})
		v.Warnings = append(v.Warnings, fmt.Sprintf("Everything under %s becomes readable from the sandbox. It is mounted read-only: "+
			"what the sandbox may write of it is writable in config.toml, which %s asks for.", hostShown, SetupCommand(c.Env, "roots")))
		if rc.sole != nil {
			v.Warnings = append(v.Warnings, fmt.Sprintf("The root there is now, %s, is mounted at %s itself. With two, each is mounted at %s/NAME, so it moves to %s/%s.",
				rc.sole.Path, config.WorkDir, config.WorkDir, config.WorkDir, rc.sole.Name))
		}
		for _, m := range rc.moves {
			v.Warnings = append(v.Warnings, capFirst(m)+". Claude Code keeps each project's state under its path in the sandbox, "+
				"so it will not find what it has under the old one. Nothing is moved or deleted.")
		}
		v.Confirm, v.ConfirmName = ConfirmTypeName, pl.root.Name
	}
}

// sectionName is the name of the section v's proposal puts in the
// Dockerfile.
func (v *ProposalView) sectionName() string {
	if v.plan.section != nil {
		return v.plan.section.Name
	}
	return ""
}

// rootChange is what adding a plan's root does to the roots: the roots as
// they would be, the sole root at /work that moves to its own /work/NAME
// (nil when none does), the projects whose paths move, or why the roots
// cannot take it.
type rootChange struct {
	roots []setupRoot
	sole  *setupRoot
	moves []string
	err   error
}

// planRootChange works out what adding pl's root does to the roots
// config.toml has.
func (a *App) planRootChange(pl plan) rootChange {
	cur := a.fileRoots()
	roots := append([]setupRoot(nil), cur...)
	var rc rootChange
	if len(roots) == 1 && roots[0].Container == config.WorkDir {
		sole := roots[0]
		rc.sole = &sole
		roots[0].Container = ""
	}
	roots = append(roots, setupRoot{Name: pl.root.Name, Path: a.rootSpelling(pl.host)})
	rc.roots = roots
	if _, err := config.FileRoots(rootsEdit(roots).Roots, a.Cfg.Home); err != nil {
		rc.err = err
		return rc
	}
	rc.moves = a.rootMoves(cur, roots)
	return rc
}

// ApplyPlanned applies e as the web page confirmed it: planned again now,
// and written only when what would be shown hashes to hash, it may be
// applied from the page, and typed is the name it takes typed. The
// refusals are daemonproto errors: KindStaleDiff when the plan is not
// the one confirmed, KindBadRequest otherwise. Just before each write
// what it would write is checked again against the plan. notes say what
// was done and what is left (a restart).
func (a *App) ApplyPlanned(e proposal.Entry, hash, typed string) (notes []string, err error) {
	c := a.Cfg
	if c.EnvDir == "" {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "this environment has no dir of its own, so no config.toml to apply a proposal to")
	}
	v, err := a.ProposalView(e)
	if err != nil {
		return nil, err
	}
	if v.Hash != hash {
		return nil, daemonproto.NewError(daemonproto.KindStaleDiff, "the proposal, or what it applies to, changed since it was shown: nothing was applied")
	}
	if !v.WebApply {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "not applied from the page: %s", v.WebApplyWhy)
	}
	if v.NeedsBuild {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "not applied at once: it builds the image first, which %s does as a job", daemonproto.MethodProposalBuild)
	}
	if err := v.checkTyped(typed); err != nil {
		return nil, err
	}
	pl := v.plan
	if a.beforeProposalWrite != nil {
		a.beforeProposalWrite()
	}
	// The view allows one part only, so one write: checked again against
	// the plan just before it is made.
	restart := envCommand(c.Env, "restart")
	switch {
	case pl.dockerfile != nil:
		if source, cur, err := a.dockerfileBase(); err != nil || source != pl.source || !bytes.Equal(cur, pl.oldDockerfile) {
			return nil, daemonproto.NewError(daemonproto.KindStaleDiff, "the Dockerfile changed since it was shown: nothing was applied")
		}
		path := filepath.Join(c.ImageProfile.Dir, "Dockerfile")
		if err := writeDockerfile(c.ImageProfile.Dir, pl.dockerfile); err != nil {
			return nil, err
		}
		notes = append(notes, fmt.Sprintf("Wrote %s; it is yours to edit from here on. The image is rebuilt at the next %s, which ends running sessions.",
			a.short(path), restart))
	case v.edit != nil:
		if d, err := a.configDiff(*v.edit); err != nil || d != v.editDiff {
			return nil, daemonproto.NewError(daemonproto.KindStaleDiff, "config.toml changed since it was shown: nothing was applied")
		}
		if _, err := a.writeConfig(*v.edit); err != nil {
			return nil, err
		}
		if err := a.rereadConfigFile(); err != nil {
			return nil, err
		}
		cfgPath := a.short(filepath.Join(c.EnvDir, config.FileName))
		switch {
		case pl.alias != nil:
			notes = append(notes, fmt.Sprintf("Wrote [alias.%s] to %s: the link reads it within seconds, with no restart.", pl.alias.alias.Name, cfgPath))
		case pl.root != nil:
			notes = append(notes, a.rootNote(pl))
		case pl.writable != nil:
			notes = append(notes, fmt.Sprintf("Wrote the roots to %s: making %s writable takes effect at the %s's next restart (%s, which ends running sessions).",
				cfgPath, proposal.Printable(pl.writable.container), a.noun(), restart))
		}
	default:
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "not applied from the page: there is nothing in it to write")
	}
	return append(notes, a.removeApplied(e)...), nil
}

// checkTyped is the refusal of typed when v takes a name typed and typed
// is not it.
func (v *ProposalView) checkTyped(typed string) error {
	if v.Confirm == ConfirmTypeName && typed != v.ConfirmName {
		return daemonproto.NewError(daemonproto.KindBadRequest, "that is not %s: nothing was applied, and the proposal is left pending", v.ConfirmName)
	}
	return nil
}

// rootNote says what is left once pl's root is written to config.toml.
func (a *App) rootNote(pl plan) string {
	return fmt.Sprintf("Wrote the roots to %s: %s mounts %s at %s/%s, and ends running sessions.",
		a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), envCommand(a.Cfg.Env, "restart"), proposal.Printable(pl.host), config.WorkDir, pl.root.Name)
}

// removeApplied removes e once it is applied, and tells sessions what a
// proposal is made against now; the note says when it could not be
// removed.
func (a *App) removeApplied(e proposal.Entry) (notes []string) {
	c := a.Cfg
	if err := proposal.Remove(c.DataDir, e.File); err != nil {
		notes = append(notes, fmt.Sprintf("Applied, but it could not be removed (%s): delete %s by hand, or it is offered again.",
			proposal.Printable(err.Error()), a.short(filepath.Join(c.DataDir, proposal.Dir, proposal.Printable(e.File)))))
	}
	a.exportProposals()
	return notes
}

// writeDockerfile writes data as dir's Dockerfile, by a temp file and a
// rename.
func writeDockerfile(dir string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Die("%v", err)
	}
	f, err := os.CreateTemp(dir, ".Dockerfile-*")
	if err != nil {
		return Die("%v", err)
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, "Dockerfile"))
	}
	if err != nil {
		return Die("writing %s: %v", filepath.Join(dir, "Dockerfile"), err)
	}
	return nil
}
