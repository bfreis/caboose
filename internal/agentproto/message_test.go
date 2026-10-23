package agentproto

import (
	"bytes"
	"testing"
	"time"
)

// The decoder takes whatever the other side sends: it never panics, and
// what it decodes encodes, decodes and encodes again the same way.
func FuzzDecode(f *testing.F) {
	seeds := []Message{
		{Type: TypeHello, Version: Version, Broker: true, HostExec: true, Egress: EgressListen},
		{Type: TypeRequest, ID: 3, Op: OpBrokerRequest, Ask: &Ask{Kind: "egress", Host: "api.example.com", Port: 443,
			Context: "why", Options: []string{"a", "b"}, Recommendation: "a", Meanwhile: "m", Session: "s",
			Args: map[string]string{"k": "v"}}},
		{Type: TypeRequest, ID: 4, Op: OpBrokerRequests, Since: 9, Wait: 240, Session: "s", All: true},
		{Type: TypeResponse, ID: 4, OK: true, Cursor: 12, Requests: []RequestInfo{{ID: 1, Kind: "egress", Effect: "connect",
			State: "pending", Scope: "hours", Hours: 8, Expires: time.Unix(1700000000, 0).UTC(), Seq: 3}}},
		{Type: TypeResponse, ID: 5, Reason: ReasonPending, RequestID: 41, Verdict: VerdictPending, Error: "pending #41"},
		{Type: TypeResponse, ID: 6, Reason: EgressPending, RequestID: 2},
		{Type: TypeMods, Mods: &ModsInfo{Sessions: 2, WithMod: 1}},
		{Type: TypeHello, Version: Version, AliasesOn: true},
		{Type: TypeRequest, ID: 7, Op: OpAliasRun, Exec: &ExecRequest{Argv: []string{"test", "suite=unit"}, Dir: "/work/dev"}},
		{Type: TypeResponse, ID: 7, Reason: AliasPending, RequestID: 3, Error: "pending #3"},
		{Type: TypeResponse, ID: 8, OK: true, Aliases: []AliasInfo{{Name: "test", Description: "d", Params: map[string][]string{"suite": {"unit", "all"}}, Asks: true}}},
	}
	for _, m := range seeds {
		b, err := Encode(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, s := range []string{`{}`, `{"type":""}`, `{"type":"mods","mods":{"sessions":-1}}`, `{"type":"request","ask":null}`,
		`{"type":"response","requests":[{"expires":"x"}]}`, `{"type":"request","wait":1e99}`, `[`, ``,
		`{"type":"response","aliases":[{"params":{"a":null}}]}`, `{"type":"request","op":"alias-run","exec":{"argv":null}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Decode(b)
		if err != nil {
			return
		}
		if m.Type == "" {
			t.Fatal("decoded a message without a type")
		}
		again, err := Encode(m)
		if err != nil {
			t.Fatalf("cannot encode what was decoded: %v", err)
		}
		m2, err := Decode(again)
		if err != nil {
			t.Fatalf("cannot decode what was encoded: %v", err)
		}
		// What it decoded encodes as it did: nothing the decoder took in
		// is lost or changed on the way out.
		third, err := Encode(m2)
		if err != nil || !bytes.Equal(again, third) {
			t.Fatalf("round trip changed it:\n%s\n%s", again, third)
		}
	})
}

// Every field the requests added is omitted when unset, so a message of
// an older kind encodes as it did: an older peer reads it unchanged.
func TestNewFieldsOmitted(t *testing.T) {
	b, err := Encode(Message{Type: TypeResponse, ID: 1, OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"response","id":1,"ok":true}` {
		t.Fatalf("got %s", b)
	}
}
