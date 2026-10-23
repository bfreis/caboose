package daemonproto

import (
	"context"
	"strings"
)

// MethodEditEgress edits the egress_allow and egress_deny of the vm
// profile an environment runs under: without a confirmation it returns
// the change to config.toml, and with the hash of that change it writes
// it. A broker method, served by the daemon that runs the environment's
// link.
const MethodEditEgress = "edit_egress" // params EditEgressParams, result EditEgressResult

// Bounds of edit_egress.
const (
	// MaxEgressEditEntries is the most entries one edit adds and removes
	// in all.
	MaxEgressEditEntries = 32
	// MaxEgressEntry is the longest entry, in bytes.
	MaxEgressEntry = 260
)

// EditEgressParams are the params of edit_egress. Each added entry is one
// egress_allow takes (a name, *.name, a CIDR or an address), which the
// daemon checks; each removed one must be in the list now.
type EditEgressParams struct {
	Env         string   `json:"env"`
	AddAllow    []string `json:"add_allow,omitempty"`
	RemoveAllow []string `json:"remove_allow,omitempty"`
	AddDeny     []string `json:"add_deny,omitempty"`
	RemoveDeny  []string `json:"remove_deny,omitempty"`
	// ConfirmDiff is the Hash of the diff the person was shown: the
	// daemon writes only when it equals the hash of the diff it would
	// make now, and answers KindStaleDiff otherwise. Without it nothing
	// is written.
	ConfirmDiff string `json:"confirm_diff,omitempty"`
}

// Validate checks the params.
func (p *EditEgressParams) Validate() error {
	if !ValidEnv(p.Env) {
		return bad("bad environment name")
	}
	n := len(p.AddAllow) + len(p.RemoveAllow) + len(p.AddDeny) + len(p.RemoveDeny)
	if n == 0 {
		return bad("an egress edit adds or removes at least one entry")
	}
	if n > MaxEgressEditEntries {
		return bad("an egress edit adds and removes at most %d entries", MaxEgressEditEntries)
	}
	for _, list := range [][]string{p.AddAllow, p.RemoveAllow, p.AddDeny, p.RemoveDeny} {
		for _, e := range list {
			if e == "" {
				return bad("an empty egress entry")
			}
			if err := checkText("egress entry", e, MaxEgressEntry); err != nil {
				return err
			}
		}
	}
	if p.ConfirmDiff != "" && !hashRe.MatchString(p.ConfirmDiff) {
		return bad("bad confirmation hash")
	}
	return nil
}

// EditEgressResult is what an egress edit does, or did: the change to
// config.toml as lines, its hash, the effect in the host's words, and
// whether it was written.
type EditEgressResult struct {
	Diff    []string `json:"diff"`
	Hash    string   `json:"hash"`
	Effect  string   `json:"effect"`
	Written bool     `json:"written"`
}

// Validate checks the result's bounds and strings.
func (r *EditEgressResult) Validate() error {
	if !hashRe.MatchString(r.Hash) {
		return bad("an egress edit without its diff's hash")
	}
	if len(r.Diff) == 0 || len(r.Diff) > MaxOfferDiffLines {
		return bad("an egress edit's diff of %d lines", len(r.Diff))
	}
	n := 0
	for _, l := range r.Diff {
		n += len(l) + 1
		if err := checkText("egress edit diff", l, MaxDiff); err != nil {
			return err
		}
	}
	if n > MaxDiff {
		return bad("an egress edit's diff over %d bytes", MaxDiff)
	}
	if r.Effect == "" {
		return bad("an egress edit without its effect")
	}
	return checkText("effect", r.Effect, maxViewText)
}

// DiffLines is a config.toml diff as the protocol carries it: one
// printable line each, tabs made spaces.
func DiffLines(diff string) []string {
	var out []string
	if d := strings.TrimSuffix(diff, "\n"); d != "" {
		for _, l := range strings.Split(d, "\n") {
			out = append(out, Text(strings.ReplaceAll(l, "\t", " "), MaxDiff))
		}
	}
	return out
}

// EditEgress computes, or with p.ConfirmDiff writes, an egress edit.
func (c *Client) EditEgress(ctx context.Context, p EditEgressParams) (*EditEgressResult, error) {
	var r EditEgressResult
	if err := checkParams(&p); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, MethodEditEgress, &p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
