package daemonproto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditEgressParams(t *testing.T) {
	h := strings.Repeat("ab", 32)
	ok := []EditEgressParams{
		{Env: "e", AddAllow: []string{"git.corp.example"}},
		{Env: "e", RemoveDeny: []string{"10.0.0.0/8"}, ConfirmDiff: h},
	}
	for _, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	many := make([]string, MaxEgressEditEntries+1)
	for i := range many {
		many[i] = "a.example"
	}
	bad := []EditEgressParams{
		{Env: "E", AddAllow: []string{"a.example"}},
		{Env: "e"},
		{Env: "e", AddAllow: many},
		{Env: "e", AddAllow: many[:20], RemoveDeny: many[:13]},
		{Env: "e", AddDeny: []string{""}},
		{Env: "e", AddDeny: []string{"a\u202e.example"}},
		{Env: "e", RemoveAllow: []string{strings.Repeat("a", MaxEgressEntry+1)}},
		{Env: "e", AddAllow: []string{"a.example"}, ConfirmDiff: "nothex"},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}

func TestEditEgressResult(t *testing.T) {
	h := strings.Repeat("ab", 32)
	good := EditEgressResult{Diff: DiffLines("-a = 1\n+a\t= 2\n"), Hash: h, Effect: "allows connections to x from environment e"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if good.Diff[1] != "+a = 2" {
		t.Errorf("diff lines %q", good.Diff)
	}
	for name, r := range map[string]EditEgressResult{
		"no hash":   {Diff: []string{"+a"}, Effect: "e"},
		"no diff":   {Hash: h, Effect: "e"},
		"no effect": {Diff: []string{"+a"}, Hash: h},
		"control":   {Diff: []string{"+a\x1b[31m"}, Hash: h, Effect: "e"},
		"bidi":      {Diff: []string{"+a"}, Hash: h, Effect: "e\u202e"},
		"long":      {Diff: make([]string, MaxOfferDiffLines+1), Hash: h, Effect: "e"},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Whatever bytes arrive as edit_egress's params or result, decoding never
// panics, and what validates is within the bounds the broker and the page
// rely on.
func FuzzEditEgress(f *testing.F) {
	h := strings.Repeat("ab", 32)
	for _, s := range []string{`{}`, `{"env":"e","add_allow":["git.corp.example"]}`, `{"env":"e","remove_deny":["10.0.0.0/8"],"confirm_diff":"` + h + `"}`,
		`{"diff":["-a","+b"],"hash":"` + h + `","effect":"e","written":true}`, `[]`, `null`, `{"env":"e","add_allow":[1]}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fr := &Frame{Type: TypeRequest, ID: 1, Method: MethodEditEgress, Params: b}
		var p EditEgressParams
		if DecodeParams(fr, &p) == nil {
			n := len(p.AddAllow) + len(p.RemoveAllow) + len(p.AddDeny) + len(p.RemoveDeny)
			if !ValidEnv(p.Env) || n == 0 || n > MaxEgressEditEntries || (p.ConfirmDiff != "" && !hashRe.MatchString(p.ConfirmDiff)) {
				t.Fatalf("accepted %+v", p)
			}
			for _, l := range [][]string{p.AddAllow, p.RemoveAllow, p.AddDeny, p.RemoveDeny} {
				for _, e := range l {
					if e == "" || len(e) > MaxEgressEntry || !printable(e) {
						t.Fatalf("accepted entry %q", e)
					}
				}
			}
		}
		var r EditEgressResult
		if json.Unmarshal(b, &r) == nil && r.Validate() == nil {
			if !hashRe.MatchString(r.Hash) || len(r.Diff) == 0 || len(r.Diff) > MaxOfferDiffLines || r.Effect == "" || !printable(r.Effect) {
				t.Fatalf("accepted %+v", r)
			}
			for _, l := range r.Diff {
				if !printable(l) || strings.Contains(l, "\n") {
					t.Fatalf("accepted diff line %q", l)
				}
			}
		}
	})
}
