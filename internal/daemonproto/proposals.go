package daemonproto

import (
	"context"
	"strings"
	"time"
)

// The proposals sessions wrote for an environment, as the host would apply
// them: listed, deleted, and applied once a person confirmed the hash of
// what they were shown. Everything in a proposal's title and reason is
// the sandbox's text, kept apart from the host's parts, refusals and
// warnings; all of it is printable.
const (
	MethodProposals      = "proposals"       // params EnvParams, result ProposalsResult
	MethodProposalDelete = "proposal_delete" // params ProposalDeleteParams, no result
	MethodProposalApply  = "proposal_apply"  // params ProposalApplyParams, result ProposalApplyResult
	// MethodProposalBuild applies one whose image is built first
	// (NeedsBuild), as a job of the environment's (jobs.go), which
	// proposal_apply refuses.
	MethodProposalBuild = "proposal_build" // params ProposalApplyParams, result JobResult
)

// Bounds of the proposal methods.
const (
	// MaxProposals is the most proposals a list holds.
	MaxProposals = 64
	// MaxProposalFile is the longest proposal file name, in bytes.
	MaxProposalFile = 255
	// MaxProposalDiff is the most a proposal's diff lines take in all, in
	// bytes, and MaxProposalDiffLines how many there are: one larger is
	// listed without its diff, and cannot be applied from the page.
	MaxProposalDiff      = 64 << 10
	MaxProposalDiffLines = 2000
	// MaxProposalsBytes is what the diffs of a whole list take, under
	// MaxLine with room for the rest.
	MaxProposalsBytes = 640 << 10
	// MaxProposalParts, MaxProposalNotices and MaxProposalNotes bound a
	// proposal's parts, its refusals and warnings, and an apply's notes.
	MaxProposalParts   = 8
	MaxProposalNotices = 32
	MaxProposalNotes   = 8
	// MaxProposalTyped is the longest name typed to confirm.
	MaxProposalTyped = 64

	maxProposalText = 4096
)

// Kinds of a proposal's part.
const (
	ProposalPartSection  = "section"
	ProposalPartPackages = "packages"
	ProposalPartRoot     = "root"
	ProposalPartAlias    = "alias"
	ProposalPartWritable = "writable"
	ProposalPartSettings = "settings"
)

// Confirmations a proposal takes beyond the confirmation itself.
const (
	ProposalConfirmNone     = "none"
	ProposalConfirmTypeName = "type_name"
)

// ProposalPart is one change of a proposal: its diff's literal lines and
// a summary in the host's words.
type ProposalPart struct {
	Kind    string   `json:"kind"`
	Diff    []string `json:"diff"`
	Summary string   `json:"summary"`
}

// ProposalConfirm is what applying a proposal takes: nothing more, or Name
// typed out.
type ProposalConfirm struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// ProposalInfo is one pending proposal as the host would apply it.
// WebApply says whether the page may apply it; WebApplyWhy why not. Hash
// is what proposal_apply is sent back, of what is shown here.
type ProposalInfo struct {
	File        string          `json:"file"`
	Modified    time.Time       `json:"modified"`
	Title       string          `json:"title"`
	Reason      string          `json:"reason"`
	Parts       []ProposalPart  `json:"parts"`
	Refusals    []string        `json:"refusals"`
	Warnings    []string        `json:"warnings"`
	WebApply    bool            `json:"web_apply"`
	WebApplyWhy string          `json:"web_apply_why,omitempty"`
	Confirm     ProposalConfirm `json:"confirm"`
	NeedsBuild  bool            `json:"needs_build"`
	Hash        string          `json:"hash"`
}

// ProposalsResult is the result of proposals: at most MaxProposals, More
// set when there were others.
type ProposalsResult struct {
	Proposals []ProposalInfo `json:"proposals"`
	More      bool           `json:"more,omitempty"`
}

// ValidProposalFile reports whether f can name a proposal file: a name
// ending in .toml, not hidden, with no path in it.
func ValidProposalFile(f string) bool {
	return f != "" && len(f) <= MaxProposalFile && strings.HasSuffix(f, ".toml") && !strings.HasPrefix(f, ".") &&
		!strings.ContainsAny(f, "/\x00") && printable(f)
}

// ProposalFileParams name one proposal of an environment.
type ProposalFileParams struct {
	Env  string `json:"env"`
	File string `json:"file"`
}

// Validate checks the params.
func (p *ProposalFileParams) Validate() error {
	if !ValidEnv(p.Env) {
		return bad("bad environment name")
	}
	if !ValidProposalFile(p.File) {
		return bad("bad proposal file name")
	}
	return nil
}

// ProposalDeleteParams delete one proposal: Hash is the ProposalInfo's
// the person was shown, so that what is deleted is what they saw.
type ProposalDeleteParams struct {
	Env  string `json:"env"`
	File string `json:"file"`
	Hash string `json:"hash"`
}

// Validate checks the params.
func (p *ProposalDeleteParams) Validate() error {
	if err := (&ProposalFileParams{Env: p.Env, File: p.File}).Validate(); err != nil {
		return err
	}
	if !hashRe.MatchString(p.Hash) {
		return bad("bad proposal hash")
	}
	return nil
}

// ProposalApplyParams apply one proposal: Hash is the ProposalInfo's the
// person was shown, Typed the name they typed when it takes one.
type ProposalApplyParams struct {
	Env   string `json:"env"`
	File  string `json:"file"`
	Hash  string `json:"hash"`
	Typed string `json:"typed,omitempty"`
}

// Validate checks the params.
func (p *ProposalApplyParams) Validate() error {
	if err := (&ProposalFileParams{Env: p.Env, File: p.File}).Validate(); err != nil {
		return err
	}
	if !hashRe.MatchString(p.Hash) {
		return bad("bad proposal hash")
	}
	return checkText("typed name", p.Typed, MaxProposalTyped)
}

// ProposalApplyResult says what applying did, and what is left to do.
type ProposalApplyResult struct {
	Applied bool     `json:"applied"`
	Notes   []string `json:"notes"`
}

// Validate checks the notes.
func (r *ProposalApplyResult) Validate() error {
	if len(r.Notes) > MaxProposalNotes {
		return bad("more than %d notes", MaxProposalNotes)
	}
	for _, n := range r.Notes {
		if err := checkText("note", n, maxProposalText); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks one proposal.
func (p *ProposalInfo) Validate() (diffBytes int, err error) {
	if !ValidProposalFile(p.File) {
		return 0, bad("bad proposal file name")
	}
	for _, f := range []struct {
		what, s string
	}{{"title", p.Title}, {"reason", p.Reason}, {"web_apply_why", p.WebApplyWhy}} {
		if err := checkText(f.what, f.s, maxProposalText); err != nil {
			return 0, err
		}
	}
	if !hashRe.MatchString(p.Hash) {
		return 0, bad("a proposal without its hash")
	}
	if len(p.Parts) > MaxProposalParts {
		return 0, bad("more than %d parts", MaxProposalParts)
	}
	lines := 0
	for _, pt := range p.Parts {
		switch pt.Kind {
		case ProposalPartSection, ProposalPartPackages, ProposalPartRoot, ProposalPartAlias, ProposalPartWritable, ProposalPartSettings:
		default:
			return 0, bad("bad proposal part kind")
		}
		if err := checkText("part summary", pt.Summary, maxProposalText); err != nil {
			return 0, err
		}
		for _, l := range pt.Diff {
			lines++
			diffBytes += len(l) + 1
			if err := checkText("proposal diff", l, MaxProposalDiff); err != nil {
				return 0, err
			}
		}
	}
	if lines > MaxProposalDiffLines || diffBytes > MaxProposalDiff {
		return 0, bad("a proposal's diff over %d lines or %d bytes", MaxProposalDiffLines, MaxProposalDiff)
	}
	if len(p.Refusals)+len(p.Warnings) > MaxProposalNotices {
		return 0, bad("more than %d refusals and warnings", MaxProposalNotices)
	}
	for _, list := range [][]string{p.Refusals, p.Warnings} {
		for _, s := range list {
			if err := checkText("refusal or warning", s, maxProposalText); err != nil {
				return 0, err
			}
		}
	}
	switch p.Confirm.Kind {
	case ProposalConfirmNone:
		if p.Confirm.Name != "" {
			return 0, bad("a name to type with no confirmation to type it")
		}
	case ProposalConfirmTypeName:
		if p.Confirm.Name == "" {
			return 0, bad("no name to type")
		}
		if err := checkText("name to type", p.Confirm.Name, MaxProposalTyped); err != nil {
			return 0, err
		}
	default:
		return 0, bad("bad confirmation kind")
	}
	if p.WebApply && (p.WebApplyWhy != "" || len(p.Refusals) > 0 || len(p.Parts) == 0) {
		return 0, bad("a proposal the page may apply with refusals, no parts or a reason not to")
	}
	return diffBytes, nil
}

// Validate checks the list's bounds and strings.
func (r *ProposalsResult) Validate() error {
	if len(r.Proposals) > MaxProposals {
		return bad("more than %d proposals", MaxProposals)
	}
	total := 0
	for i := range r.Proposals {
		n, err := r.Proposals[i].Validate()
		if err != nil {
			return err
		}
		total += n
	}
	if total > MaxProposalsBytes {
		return bad("proposals' diffs over %d bytes", MaxProposalsBytes)
	}
	return nil
}

// Proposals lists env's pending proposals.
func (c *Client) Proposals(ctx context.Context, env string) (*ProposalsResult, error) {
	p := &EnvParams{Env: env}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	var r ProposalsResult
	if err := c.Call(ctx, MethodProposals, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ProposalDelete deletes one of env's proposals.
func (c *Client) ProposalDelete(ctx context.Context, env, file, hash string) error {
	p := &ProposalDeleteParams{Env: env, File: file, Hash: hash}
	if err := checkParams(p); err != nil {
		return err
	}
	return c.Call(ctx, MethodProposalDelete, p, nil)
}

// ProposalApply applies one of env's proposals as it was shown.
func (c *Client) ProposalApply(ctx context.Context, p ProposalApplyParams) (*ProposalApplyResult, error) {
	if err := checkParams(&p); err != nil {
		return nil, err
	}
	var r ProposalApplyResult
	if err := c.Call(ctx, MethodProposalApply, &p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ProposalBuild applies one of env's proposals that builds the image
// first, as it was shown: the job it started, running.
func (c *Client) ProposalBuild(ctx context.Context, p ProposalApplyParams) (*JobResult, error) {
	if err := checkParams(&p); err != nil {
		return nil, err
	}
	var r JobResult
	if err := c.Call(ctx, MethodProposalBuild, &p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
