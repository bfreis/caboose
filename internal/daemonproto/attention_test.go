package daemonproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAttentionValidate(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ok := func(it AttentionItem) AttentionItem { it.Since = at; return it }
	for _, it := range []AttentionItem{
		ok(AttentionItem{Env: "foo", Kind: AttentionRequests, Text: "2 pending requests", Route: RouteEnvRequests("foo")}),
		ok(AttentionItem{Kind: AttentionVnodes, Text: "many vnodes"}),
		ok(AttentionItem{Kind: AttentionUpdate, Text: "v2 is out", Route: ""}),
		ok(AttentionItem{Env: "a", Kind: AttentionImageStale, Text: "x", Route: RouteEnv("a")}),
	} {
		if err := it.Validate(); err != nil {
			t.Errorf("%+v: %v", it, err)
		}
	}
	for name, it := range map[string]AttentionItem{
		"unknown kind":      {Env: "a", Kind: "nope", Text: "x"},
		"empty kind":        {Env: "a", Text: "x"},
		"no env for kind":   {Kind: AttentionRequests, Text: "x"},
		"bad env":           {Env: "../x", Kind: AttentionProposals, Text: "x"},
		"escape in text":    {Env: "a", Kind: AttentionLinkFailed, Text: "x\x1b[2J"},
		"bidi in text":      {Env: "a", Kind: AttentionLinkFailed, Text: "x\u202ey"},
		"long text":         {Env: "a", Kind: AttentionLinkFailed, Text: strings.Repeat("x", MaxAttentionText+1)},
		"bad route":         {Env: "a", Kind: AttentionCheckpoint, Text: "x", Route: "http://evil"},
		"route bad env":     {Env: "a", Kind: AttentionCheckpoint, Text: "x", Route: "env/../x"},
		"unknown route":     {Env: "a", Kind: AttentionCheckpoint, Text: "x", Route: "settings"},
		"env on bad update": {Env: "a b", Kind: AttentionUpdate, Text: "x"},
	} {
		if it.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, k := range AttentionKinds() {
		if _, known := attentionKinds[k]; !known {
			t.Errorf("kind %s missing from the table", k)
		}
	}
	if len(AttentionKinds()) != len(attentionKinds) {
		t.Error("the kinds list and table differ")
	}
}

func TestAttentionInStatus(t *testing.T) {
	items := make([]AttentionItem, MaxAttention)
	for i := range items {
		items[i] = AttentionItem{Env: "a", Kind: AttentionRequests, Text: "x"}
	}
	st := &StatusResult{Attention: items}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	st.Attention = append(items, items[0])
	if st.Validate() == nil {
		t.Error("65 items accepted")
	}
	st.Attention = []AttentionItem{{Kind: "x", Text: "y"}}
	if st.Validate() == nil {
		t.Error("a bad item in a status was accepted")
	}
	b, _ := json.Marshal(&StatusResult{})
	if !strings.Contains(string(b), `"attention":null`) && !strings.Contains(string(b), `"attention":[]`) {
		t.Errorf("attention not carried: %s", b)
	}
}

func FuzzAttention(f *testing.F) {
	for _, s := range []string{
		`{"daemon":{},"envs":[],"attention":[{"env":"a","kind":"requests","text":"2 pending","route":"requests/a","since":"2026-01-01T00:00:00Z"}]}`,
		`{"attention":[{"kind":"vnodes","text":"\u202e","route":"env/../x"}]}`,
		`{"attention":[{"kind":"nope","text":"x"}]}`,
		`[]`, `null`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var st StatusResult
		if json.Unmarshal(b, &st) != nil || st.Validate() != nil {
			return
		}
		if len(st.Attention) > MaxAttention {
			t.Fatalf("accepted %d items", len(st.Attention))
		}
		for _, it := range st.Attention {
			allowEmpty, known := attentionKinds[it.Kind]
			if !known || (it.Env == "" && !allowEmpty) || (it.Env != "" && !ValidEnv(it.Env)) ||
				!printable(it.Text) || len(it.Text) > MaxAttentionText || !ValidRoute(it.Route) {
				t.Fatalf("accepted %+v", it)
			}
		}
	})
}
