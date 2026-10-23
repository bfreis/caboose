package daemonproto

import (
	"encoding/json"
	"strings"
	"testing"
)

func goodProposal() ProposalInfo {
	return ProposalInfo{File: "foo.toml", Title: "Install foo", Reason: "for the docs", Refusals: []string{}, Warnings: []string{},
		Parts:    []ProposalPart{{Kind: ProposalPartRoot, Diff: []string{" [roots]", "+other = \"~/src/other\""}, Summary: "mount ~/src/other read-only at /work/other"}},
		WebApply: true, Confirm: ProposalConfirm{Kind: ProposalConfirmTypeName, Name: "other"}, Hash: strings.Repeat("ab", 32)}
}

func TestProposalFileNames(t *testing.T) {
	for _, f := range []string{"foo.toml", `x\u001B[2J.toml`, "a b.toml"} {
		if !ValidProposalFile(f) {
			t.Errorf("%q refused", f)
		}
	}
	for _, f := range []string{"", "foo", ".foo.toml", "../foo.toml", "a/b.toml", "x\x1b.toml", "a\u202e.toml", "a\x00.toml", strings.Repeat("a", MaxProposalFile) + ".toml"} {
		if ValidProposalFile(f) {
			t.Errorf("%q accepted", f)
		}
	}
}

func TestProposalParams(t *testing.T) {
	h := strings.Repeat("ab", 32)
	if err := (&ProposalApplyParams{Env: "e", File: "f.toml", Hash: h, Typed: "other"}).Validate(); err != nil {
		t.Error(err)
	}
	for _, p := range []ProposalApplyParams{
		{Env: "E", File: "f.toml", Hash: h},
		{Env: "e", File: "../f.toml", Hash: h},
		{Env: "e", File: "f.toml"},
		{Env: "e", File: "f.toml", Hash: h, Typed: "a\x1b"},
		{Env: "e", File: "f.toml", Hash: h, Typed: strings.Repeat("a", MaxProposalTyped+1)},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	if err := (&ProposalFileParams{Env: "e", File: "f"}).Validate(); err == nil {
		t.Error("a file without .toml accepted")
	}
	if err := (&ProposalDeleteParams{Env: "e", File: "f.toml"}).Validate(); err == nil {
		t.Error("a delete without a hash accepted")
	}
	if err := (&ProposalDeleteParams{Env: "e", File: "f.toml", Hash: strings.Repeat("ab", 32)}).Validate(); err != nil {
		t.Errorf("a good delete refused: %v", err)
	}
}

func TestProposalsResult(t *testing.T) {
	good := ProposalsResult{Proposals: []ProposalInfo{goodProposal()}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	w := goodProposal()
	w.Parts[0].Kind = ProposalPartWritable
	if err := (&ProposalsResult{Proposals: []ProposalInfo{w}}).Validate(); err != nil {
		t.Errorf("a writable part refused: %v", err)
	}
	for name, mod := range map[string]func(*ProposalInfo){
		"html is text, and fine": nil,
		"control title":          func(p *ProposalInfo) { p.Title = "a\x1b[2J" },
		"bidi reason":            func(p *ProposalInfo) { p.Reason = "a\u202eb" },
		"long reason":            func(p *ProposalInfo) { p.Reason = strings.Repeat("a", maxProposalText+1) },
		"no hash":                func(p *ProposalInfo) { p.Hash = "" },
		"bad kind":               func(p *ProposalInfo) { p.Parts[0].Kind = "exec" },
		"control diff":           func(p *ProposalInfo) { p.Parts[0].Diff = []string{"+\x1b]0;x\a"} },
		"long diff":              func(p *ProposalInfo) { p.Parts[0].Diff = make([]string, MaxProposalDiffLines+1) },
		"no name":                func(p *ProposalInfo) { p.Confirm.Name = "" },
		"name without kind":      func(p *ProposalInfo) { p.Confirm.Kind = ProposalConfirmNone },
		"bad confirm":            func(p *ProposalInfo) { p.Confirm.Kind = "click" },
		"apply with refusals":    func(p *ProposalInfo) { p.Refusals = []string{"no"} },
		"apply without parts":    func(p *ProposalInfo) { p.Parts = nil },
		"apply with a why":       func(p *ProposalInfo) { p.WebApplyWhy = "packages" },
		"bad file":               func(p *ProposalInfo) { p.File = "../x.toml" },
	} {
		p := goodProposal()
		if mod == nil {
			p.Title = "<img src=x onerror=alert(1)>"
			if err := (&ProposalsResult{Proposals: []ProposalInfo{p}}).Validate(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		mod(&p)
		if err := (&ProposalsResult{Proposals: []ProposalInfo{p}}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	many := make([]ProposalInfo, MaxProposals+1)
	for i := range many {
		many[i] = goodProposal()
	}
	if err := (&ProposalsResult{Proposals: many}).Validate(); err == nil {
		t.Error("too many proposals accepted")
	}
	big := goodProposal()
	big.Parts[0].Diff = []string{strings.Repeat("a", MaxProposalDiff-10)}
	list := make([]ProposalInfo, MaxProposalsBytes/MaxProposalDiff+1)
	for i := range list {
		list[i] = big
	}
	if err := (&ProposalsResult{Proposals: list}).Validate(); err == nil {
		t.Error("a list over its budget accepted")
	}
	if err := (&ProposalApplyResult{Applied: true, Notes: []string{"a\u202e"}}).Validate(); err == nil {
		t.Error("a bidi note accepted")
	}
}

// Whatever bytes arrive as the proposal methods' params or results,
// decoding never panics, and what validates is within the bounds the page
// relies on.
func FuzzProposals(f *testing.F) {
	h := strings.Repeat("ab", 32)
	good, _ := json.Marshal(ProposalsResult{Proposals: []ProposalInfo{goodProposal()}})
	for _, s := range []string{`{}`, `{"env":"e","file":"f.toml"}`, `{"env":"e","file":"f.toml","hash":"` + h + `","typed":"x"}`,
		string(good), `{"applied":true,"notes":["n"]}`, `[]`, `null`, `{"env":"e","file":"../f.toml"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fr := &Frame{Type: TypeRequest, ID: 1, Method: MethodProposalApply, Params: b}
		var p ProposalApplyParams
		if DecodeParams(fr, &p) == nil {
			if !ValidEnv(p.Env) || !ValidProposalFile(p.File) || strings.Contains(p.File, "/") || !hashRe.MatchString(p.Hash) || !printable(p.Typed) {
				t.Fatalf("accepted %+v", p)
			}
		}
		fr.Method = MethodProposalDelete
		var d ProposalDeleteParams
		if DecodeParams(fr, &d) == nil && (!ValidEnv(d.Env) || !ValidProposalFile(d.File) || !hashRe.MatchString(d.Hash)) {
			t.Fatalf("accepted %+v", d)
		}
		var r ProposalsResult
		if json.Unmarshal(b, &r) == nil && r.Validate() == nil {
			if len(r.Proposals) > MaxProposals {
				t.Fatalf("accepted %d proposals", len(r.Proposals))
			}
			for _, q := range r.Proposals {
				for _, s := range []string{q.Title, q.Reason, q.WebApplyWhy, q.File} {
					if !printable(s) {
						t.Fatalf("accepted %q", s)
					}
				}
				for _, pt := range q.Parts {
					for _, l := range pt.Diff {
						if !printable(l) || strings.Contains(l, "\n") {
							t.Fatalf("accepted diff line %q", l)
						}
					}
				}
				if q.WebApply && len(q.Refusals) > 0 {
					t.Fatalf("accepted %+v", q)
				}
			}
		}
		var a ProposalApplyResult
		if json.Unmarshal(b, &a) == nil && a.Validate() == nil {
			for _, n := range a.Notes {
				if !printable(n) {
					t.Fatalf("accepted note %q", n)
				}
			}
		}
	})
}
