package daemonproto

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func roundTrip[T any](t *testing.T, in T) T {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip:\n%+v\n%+v", in, out)
	}
	return out
}

func TestBrokerMessagesRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	view := RequestView{ID: 3, Kind: "egress", Key: "egress:a.example", Effect: "e", State: "allowed", Scope: "hours", Hours: 2,
		Created: now, Expires: now.Add(time.Hour), DecidedAt: now, DecidedBy: "cli", Reason: "r", Seq: 9, Attempts: 2, Session: "s",
		Ask: &Ask{Context: "c", Options: []string{"a", "b"}, Recommendation: "r", Meanwhile: "m"}}
	hash := strings.Repeat("ab", 32)
	for _, v := range []interface{ Validate() error }{
		roundTrip(t, &RequestsParams{Env: "default", All: true, Limit: 10}),
		roundTrip(t, &RequestParams{Env: "e", ID: 4}),
		roundTrip(t, &AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "hours", Hours: 168}),
		roundTrip(t, &AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "always", ConfirmDiff: hash}),
		roundTrip(t, &AnswerParams{Env: "e", ID: 1, Verdict: "deny"}),
		roundTrip(t, &RevokeParams{Env: "e", Grant: "g12"}),
		roundTrip(t, &RequestsResult{Requests: []RequestView{view}, Truncated: true}),
		roundTrip(t, &RequestResult{Request: view}),
		roundTrip(t, &RequestResult{Request: view, Offers: []Offer{{Scope: "once", Effect: "allows that once"},
			{Scope: "hours", Effect: "allows x for {hours} hours", HoursMin: 1, HoursMax: 168},
			{Scope: "always", Effect: "adds x", Diff: []string{"-a", "+b"}, Hash: hash}}}),
		roundTrip(t, &RequestResult{Request: view, Offers: []Offer{{Scope: "once", Effect: "o"}}, Withheld: []Withheld{{Scope: "always", Why: "hostile"}}}),
		roundTrip(t, &AnswerResult{Request: view, Effect: "x", Diff: "-a\n+b\n", DiffHash: hash, Recorded: true, Grant: "g1", Also: []uint64{5, 6}, OpenError: "no"}),
		roundTrip(t, &GrantsResult{Grants: []GrantView{{ID: "g1", Kind: "egress", Key: "k", Once: true, Addrs: []string{"10.0.0.1"}, Created: now, Expires: now, LeftSeconds: 5, Request: 2}}}),
		roundTrip(t, &RevokeResult{Revoked: "g1"}),
		roundTrip(t, &PermissionsResult{Env: "e", Trust: "fallible", Isolation: "vm", Profile: "vm.default",
			Roots:    []RootPerm{{Name: "dev", Host: "/h", Container: "/work/dev", Writable: []string{"."}}},
			Aliases:  []AliasPerm{{Name: "a", Command: []string{"/bin/x"}, Policy: "ask"}},
			HostExec: HostExecPerm{Configured: true}, OpenURLs: OpenURLsPerm{Mode: "ask", Allow: []string{"https://x"}},
			Egress: EgressPerm{On: true, Ports: "22", Allow: []string{"a"}, Deny: []string{"b"}}, Grants: []GrantView{{ID: "g1", Kind: "k", Key: "k"}},
			Mods: &ModsPerm{Sessions: 1}, ForwardPorts: "3000-3999 5173", Drift: []string{"the VM was created with trust fallible; the configuration says hostile"}}),
	} {
		if err := v.Validate(); err != nil {
			t.Errorf("%T: %v", v, err)
		}
	}
}

func TestParamsRefusals(t *testing.T) {
	hash := strings.Repeat("0", 64)
	bad := []interface{ Validate() error }{
		&RequestsParams{Env: "Bad"}, &RequestsParams{Env: "e", Limit: -1}, &RequestsParams{Env: "e", Limit: 201},
		&RequestParams{Env: "e"}, &RequestParams{Env: "../x", ID: 1},
		&AnswerParams{Env: "e", ID: 0, Verdict: "deny"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "maybe"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "forever"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "hours"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "hours", Hours: 169},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "hours", Hours: -2},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "once", Hours: 2},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "always", Hours: 2},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "always"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "once", ConfirmDiff: hash},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "always", ConfirmDiff: "nothex"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "allow", Scope: "always", ConfirmDiff: strings.ToUpper(strings.Repeat("a", 64))},
		&AnswerParams{Env: "e", ID: 1, Verdict: "deny", Scope: "once"},
		&AnswerParams{Env: "e", ID: 1, Verdict: "deny", ConfirmDiff: hash},
		&RevokeParams{Env: "e", Grant: ""}, &RevokeParams{Env: "e", Grant: "x1"}, &RevokeParams{Env: "e", Grant: "g"}, &RevokeParams{Env: "e", Grant: "g1\n"},
		&RevokeParams{Env: "e", Grant: "g" + strings.Repeat("1", 30)},
	}
	for _, v := range bad {
		if err := v.Validate(); err == nil {
			t.Errorf("%+v accepted", v)
		}
	}
	// Answers have no field for who answers: a client that names one has
	// it ignored, not decoded.
	var p AnswerParams
	f := &Frame{Type: TypeRequest, ID: 1, Method: MethodAnswer, Params: json.RawMessage(`{"env":"e","id":1,"verdict":"deny","by":"dialog","decided_by":"dialog"}`)}
	if err := DecodeParams(f, &p); err != nil {
		t.Fatal(err)
	}
	if _, has := reflect.TypeOf(p).FieldByName("By"); has {
		t.Error("AnswerParams has a By")
	}
}

// Whatever the sandbox managed to put in the text a result carries, the
// client refuses a result with a control, bidi or invalid character, or
// one over its bound, instead of printing it.
func TestResultsRefuseHostileText(t *testing.T) {
	hostile := []string{"a\x1b[31mb", "a\nb", "x\u202ey", "x\u200bz", "bad\xffutf", "tab\tx", "nul\x00", "\u2028", strings.Repeat("a", 5000)}
	good := func() RequestView { return RequestView{ID: 1, Kind: "egress", State: "pending", Created: time.Now()} }
	mutate := map[string]func(v *RequestView, s string){
		"key": func(v *RequestView, s string) { v.Key = s }, "effect": func(v *RequestView, s string) { v.Effect = s },
		"reason": func(v *RequestView, s string) { v.Reason = s }, "session": func(v *RequestView, s string) { v.Session = s },
		"kind": func(v *RequestView, s string) { v.Kind = s }, "state": func(v *RequestView, s string) { v.State = s },
		"context": func(v *RequestView, s string) { v.Ask = &Ask{Context: s} }, "meanwhile": func(v *RequestView, s string) { v.Ask = &Ask{Meanwhile: s} },
		"recommendation": func(v *RequestView, s string) { v.Ask = &Ask{Recommendation: s} }, "option": func(v *RequestView, s string) { v.Ask = &Ask{Options: []string{s}} },
	}
	for name, m := range mutate {
		for _, h := range hostile {
			v := good()
			m(&v, h)
			if err := (&RequestsResult{Requests: []RequestView{v}}).Validate(); err == nil {
				t.Errorf("%s %q accepted", name, h)
			}
			if err := (&AnswerResult{Request: v}).Validate(); err == nil {
				t.Errorf("answer: %s %q accepted", name, h)
			}
		}
	}
	for _, h := range hostile[:8] {
		for what, r := range map[string]interface{ Validate() error }{
			"effect":    &AnswerResult{Request: good(), Effect: h},
			"open":      &AnswerResult{Request: good(), OpenError: h},
			"grant key": &GrantsResult{Grants: []GrantView{{ID: "g1", Kind: "k", Key: h}}},
			"grant adr": &GrantsResult{Grants: []GrantView{{ID: "g1", Kind: "k", Key: "k", Addrs: []string{h}}}},
			"root":      &PermissionsResult{Env: "e", Roots: []RootPerm{{Host: h}}},
			"alias":     &PermissionsResult{Env: "e", Aliases: []AliasPerm{{Name: "a", Command: []string{h}}}},
			"allow":     &PermissionsResult{Env: "e", Egress: EgressPerm{Allow: []string{h}}},
			"urls":      &PermissionsResult{Env: "e", OpenURLs: OpenURLsPerm{Allow: []string{h}}},
			"trust":     &PermissionsResult{Env: "e", Trust: h},
			"why":       &PermissionsResult{Env: "e", HostExec: HostExecPerm{Why: h}},
			"drift":     &PermissionsResult{Env: "e", Drift: []string{h}},
			"ports":     &PermissionsResult{Env: "e", ForwardPorts: h},
		} {
			if err := r.Validate(); err == nil {
				t.Errorf("%s %q accepted", what, h)
			}
		}
	}
	// A diff has lines; what is in them is as printable as any text.
	if err := (&AnswerResult{Request: good(), Diff: "-a\n+b\n  c\n"}).Validate(); err != nil {
		t.Errorf("a diff: %v", err)
	}
	for _, d := range []string{"-a\n+\x1b[2J\n", "\u202e", strings.Repeat("x", MaxDiff+1)} {
		if err := (&AnswerResult{Request: good(), Diff: d}).Validate(); err == nil {
			t.Errorf("diff %q accepted", d)
		}
	}
	for _, r := range []interface{ Validate() error }{
		&AnswerResult{Request: good(), DiffHash: "x"}, &AnswerResult{Request: good(), Grant: "x"}, &RevokeResult{Revoked: "no"},
		&GrantsResult{Grants: []GrantView{{ID: "../x", Kind: "k"}}}, &PermissionsResult{Env: "BAD"},
		&PermissionsResult{Env: "e", Drift: slices.Repeat([]string{"the VM mounts /a at /work/a"}, MaxPermDrift+1)}, &PermissionsResult{Env: "e", Drift: []string{""}},
		&RequestsResult{Requests: make([]RequestView, MaxRequestsLimit+1)}, &RequestResult{},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%T accepted", r)
		}
	}
}

func TestLines(t *testing.T) {
	if got := Lines("a\tb\n\x1b[31mc\u202e\nd", 100); got != "a b\n?[31mc?\nd" {
		t.Errorf("Lines = %q", got)
	}
	long := Lines(strings.Repeat("line\n", 100), 50)
	if len(long) > 50 || !strings.HasSuffix(long, "...") {
		t.Errorf("long = %q", long)
	}
	if err := checkLines("x", Lines("\x00\x1b\u202e\xff\n\r\n", 100), 100); err != nil {
		t.Errorf("Lines output refused: %v", err)
	}
}

// The longest requests the broker can hold, as many as a listing may name,
// never make a frame over MaxLine: the daemon trims the list, and says so.
func TestRequestsListFitsAFrame(t *testing.T) {
	worst := RequestView{ID: 1 << 62, Kind: "open_url", Key: strings.Repeat("<&>", 200), Effect: strings.Repeat("<", 4000),
		State: "allowed", Scope: "hours", Created: time.Now(), Expires: time.Now(), DecidedAt: time.Now(), DecidedBy: "cli",
		Reason: strings.Repeat(">", 4000), Session: strings.Repeat("&", 100),
		Ask: &Ask{Context: strings.Repeat("<", 2000), Recommendation: strings.Repeat("<", 500), Meanwhile: strings.Repeat("<", 1000),
			Options: []string{strings.Repeat("<", 200), strings.Repeat("<", 200), strings.Repeat("<", 200), strings.Repeat("<", 200), strings.Repeat("<", 200)}}}
	views := make([]RequestView, MaxRequestsLimit)
	for i := range views {
		views[i] = worst
	}
	whole, err := Result(1, &RequestsResult{Requests: views})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(whole); err == nil {
		t.Fatal("the test's worst case fits a frame untrimmed: make it worse")
	}
	fit, truncated := FitRequests(views)
	if !truncated || len(fit) == 0 || len(fit) == len(views) {
		t.Fatalf("kept %d of %d, truncated %v", len(fit), len(views), truncated)
	}
	f, err := Result(1, &RequestsResult{Requests: fit, Truncated: truncated})
	if err != nil {
		t.Fatal(err)
	}
	line, err := Encode(f)
	if err != nil {
		t.Fatalf("the trimmed list: %v", err)
	}
	if len(line) > MaxLine {
		t.Errorf("%d bytes", len(line))
	}
	// The default listing of ordinary requests is whole.
	short := []RequestView{{ID: 1, Kind: "egress", State: "pending", Created: time.Now()}}
	if got, tr := FitRequests(short); tr || len(got) != 1 {
		t.Error("a short list was trimmed")
	}
	if got, tr := FitRequests(nil); tr || len(got) != 0 {
		t.Error("nothing was trimmed to something")
	}
}

func TestBrokerKindsAndClients(t *testing.T) {
	for _, k := range []string{KindNotOwner, KindNotFound, KindAlreadyAnswered, KindStaleDiff, KindNotAllowed, KindBrokerUnavailable} {
		if got := Kind(&Error{Kind: k}); got != k {
			t.Errorf("Kind(%s) = %s", k, got)
		}
		if err := (&Error{Kind: k, Message: "m"}).validate(); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	if Kind(&Error{Kind: "from_the_future"}) != KindInternal {
		t.Error("an unknown kind is internal")
	}
	if e := (&EnvStatus{Name: "e", Pending: -1}).validate(); e == nil {
		t.Error("a negative pending count")
	}
}

// The client's helpers send the method and params they name, decode the
// result and refuse one that breaks its bounds.
func TestClientHelpers(t *testing.T) {
	var got []string
	srv := newFake(t, serve(func(c *Conn, req *Frame) bool {
		got = append(got, req.Method+" "+string(req.Params))
		var res any
		switch req.Method {
		case MethodRequests:
			res = &RequestsResult{Requests: []RequestView{{ID: 2, Kind: "egress", State: "pending", Created: time.Now()}}}
		case MethodRequest:
			res = &RequestResult{Request: RequestView{ID: 2, Kind: "egress", State: "pending", Created: time.Now(), Effect: "bad\x1b"}}
		case MethodAnswer:
			res = &AnswerResult{Request: RequestView{ID: 2, Kind: "egress", State: "denied", Created: time.Now()}, Recorded: true}
		case MethodGrants:
			res = &GrantsResult{}
		case MethodRevoke:
			res = &RevokeResult{Revoked: "g1"}
		case MethodPermissions:
			res = &PermissionsResult{Env: "e", Trust: "fallible", Isolation: "container"}
		}
		f, err := Result(req.ID, res)
		if err != nil {
			return false
		}
		return c.Write(f, time.Second) == nil
	}))
	c, err := Dial(context.Background(), srv.path, Options{Client: ClientCLI, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if r, err := c.Requests(ctx, "e", true, 7); err != nil || len(r.Requests) != 1 {
		t.Fatalf("%v %v", r, err)
	}
	if r, err := c.Answer(ctx, AnswerParams{Env: "e", ID: 2, Verdict: "deny"}); err != nil || !r.Recorded {
		t.Fatalf("%v %v", r, err)
	}
	if _, err := c.Grants(ctx, "e"); err != nil {
		t.Fatal(err)
	}
	if r, err := c.Revoke(ctx, "e", "g1"); err != nil || r.Revoked != "g1" {
		t.Fatalf("%v %v", r, err)
	}
	if r, err := c.Permissions(ctx, "e"); err != nil || r.Trust != "fallible" {
		t.Fatalf("%v %v", r, err)
	}
	want := []string{`requests {"env":"e","all":true,"limit":7}`, `answer {"env":"e","id":2,"verdict":"deny"}`,
		`grants {"env":"e"}`, `revoke {"env":"e","grant":"g1"}`, `permissions {"env":"e"}`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent\n%q\nwant\n%q", got, want)
	}
	// A bad request is refused before it is sent.
	if _, err := c.Answer(ctx, AnswerParams{Env: "e", ID: 2, Verdict: "allow"}); Kind(err) != KindBadRequest || len(got) != 5 {
		t.Errorf("a bad answer: %v, sent %d", err, len(got))
	}
	// A daemon that sends a control character in a request's text is not
	// believed: the result is refused and the connection is done with.
	if _, err := c.Request(ctx, "e", 2); Kind(err) != KindBadFrame {
		t.Errorf("a hostile result: %v", err)
	}
	if _, err := c.Grants(ctx, "e"); Kind(err) != KindUnavailable {
		t.Errorf("after it: %v", err)
	}
}

// Whatever bytes arrive as params, decoding never panics, and what is
// accepted is within the bounds the broker relies on.
func FuzzBrokerParams(f *testing.F) {
	for _, s := range []string{`{}`, `{"env":"e","id":1,"verdict":"allow","scope":"hours","hours":8}`, `{"env":"e","id":18446744073709551615,"verdict":"deny"}`,
		`{"env":"e","grant":"g1"}`, `{"env":"e","verdict":"allow","scope":"always","confirm_diff":"` + strings.Repeat("a", 64) + `"}`, `[]`, `null`, `{"env":1}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fr := &Frame{Type: TypeRequest, ID: 1, Method: MethodAnswer, Params: b}
		var a AnswerParams
		if DecodeParams(fr, &a) == nil {
			if !ValidEnv(a.Env) || a.ID == 0 || (a.Verdict != VerdictAllow && a.Verdict != VerdictDeny) {
				t.Fatalf("accepted %+v", a)
			}
			if a.Verdict == VerdictAllow && a.Scope == ScopeHours && (a.Hours < 1 || a.Hours > MaxHours) {
				t.Fatalf("accepted hours %+v", a)
			}
			if a.ConfirmDiff != "" && !hashRe.MatchString(a.ConfirmDiff) {
				t.Fatalf("accepted confirmation %+v", a)
			}
			if a.Verdict == VerdictAllow && a.Scope == ScopeAlways && a.ConfirmDiff == "" {
				t.Fatalf("accepted always without a confirmation %+v", a)
			}
		}
		var rv RevokeParams
		if DecodeParams(fr, &rv) == nil && !grantIDRe.MatchString(rv.Grant) {
			t.Fatalf("accepted %+v", rv)
		}
		var rq RequestsParams
		if DecodeParams(fr, &rq) == nil && (rq.Limit < 0 || rq.Limit > MaxRequestsLimit || !ValidEnv(rq.Env)) {
			t.Fatalf("accepted %+v", rq)
		}
	})
}

// A request's offers are checked as the page and the command line rely
// on: known scopes once each, an effect, hours bounds only for hours, and
// a diff with its hash only for always, bounded and printable.
func TestRequestResultOffersRefusals(t *testing.T) {
	view := RequestView{ID: 1, Kind: "egress", State: "pending", Created: time.Now()}
	hash := strings.Repeat("ab", 32)
	long := make([]string, MaxOfferDiffLines+1)
	for i := range long {
		long[i] = "+x"
	}
	for _, o := range [][]Offer{
		{{Scope: "forever", Effect: "e"}},
		{{Scope: "once"}},
		{{Scope: "once", Effect: "e\u202e"}},
		{{Scope: "once", Effect: "e"}, {Scope: "once", Effect: "e"}},
		{{Scope: "once", Effect: "e", HoursMax: 2}},
		{{Scope: "hours", Effect: "e"}},
		{{Scope: "hours", Effect: "e", HoursMin: 1, HoursMax: MaxHours + 1}},
		{{Scope: "hours", Effect: "e", HoursMin: 5, HoursMax: 2}},
		{{Scope: "once", Effect: "e", Hash: hash}},
		{{Scope: "hours", Effect: "e", HoursMin: 1, HoursMax: 2, Diff: []string{"+a"}}},
		{{Scope: "always", Effect: "e", Diff: []string{"+a"}}},
		{{Scope: "always", Effect: "e", Diff: []string{"+a"}, Hash: "nothex"}},
		{{Scope: "always", Effect: "e", Diff: []string{"+a\x1b[2J"}, Hash: hash}},
		{{Scope: "always", Effect: "e", Diff: []string{"+a\n+b"}, Hash: hash}},
		{{Scope: "always", Effect: "e", Diff: long, Hash: hash}},
		{{Scope: "always", Effect: "e", Diff: []string{strings.Repeat("x", MaxDiff+1)}, Hash: hash}},
	} {
		if err := (&RequestResult{Request: view, Offers: o}).Validate(); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	for _, w := range [][]Withheld{{{Scope: "x", Why: "y"}}, {{Scope: "always", Why: "\x07"}}, {{Scope: "once", Why: "a"}, {Scope: "once", Why: "b"}}} {
		if err := (&RequestResult{Request: view, Withheld: w}).Validate(); err == nil {
			t.Errorf("%+v accepted", w)
		}
	}
	if err := (&RequestResult{Request: view, Offers: []Offer{{Scope: "once", Effect: "e"}}, Withheld: []Withheld{{Scope: "once", Why: "w"}}}).Validate(); err == nil {
		t.Error("a scope both offered and withheld")
	}
	r := &RequestResult{Request: view, Offers: []Offer{{Scope: "always", Effect: "e", Hash: hash}}}
	if r.Offered("always") != &r.Offers[0] || r.Offered("once") != nil {
		t.Error("Offered")
	}
}

// Whatever bytes arrive as a permissions report, what validates is within
// the bounds the page and the command line rely on.
func FuzzPermissionsResult(f *testing.F) {
	for _, s := range []string{`{}`, `{"env":"e","trust":"fallible","isolation":"vm","drift":["the VM mounts /a at /work/a"]}`,
		`{"env":"e","drift":["",""]}`, `{"env":"e","roots":[{"name":"r","host":"/h","container":"/work/r","read_only":true}],"drift":null}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var r PermissionsResult
		if json.Unmarshal(b, &r) != nil || r.Validate() != nil {
			return
		}
		if !ValidEnv(r.Env) || len(r.Drift) > MaxPermDrift || len(r.Roots) > maxPermList || len(r.Grants) > MaxGrants {
			t.Fatalf("accepted %+v", r)
		}
		for _, d := range r.Drift {
			if d == "" || len(d) > maxViewText || !printable(d) {
				t.Fatalf("accepted drift %q", d)
			}
		}
	})
}

// Whatever bytes arrive as a request's result, what validates is within
// the bounds the page and the command line rely on.
func FuzzRequestResult(f *testing.F) {
	h := strings.Repeat("ab", 32)
	for _, s := range []string{`{}`, `{"request":{"id":1,"kind":"egress","state":"pending","created":"2026-01-01T00:00:00Z"},"offers":[{"scope":"once","effect":"e"},{"scope":"hours","effect":"e {hours}","hours_min":1,"hours_max":168},{"scope":"always","effect":"e","diff":["-a","+b"],"hash":"` + h + `"}],"withheld":[]}`,
		`{"request":{"id":1,"kind":"k","state":"s","created":"2026-01-01T00:00:00Z"},"offers":[{"scope":"always","effect":"e","diff":[]}]}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var r RequestResult
		if json.Unmarshal(b, &r) != nil || r.Validate() != nil {
			return
		}
		seen := map[string]bool{}
		for _, o := range r.Offers {
			if !validScope(o.Scope) || seen[o.Scope] || o.Effect == "" || !printable(o.Effect) {
				t.Fatalf("accepted %+v", o)
			}
			seen[o.Scope] = true
			if (o.Scope == ScopeAlways) != (o.Hash != "") || len(o.Diff) > MaxOfferDiffLines {
				t.Fatalf("accepted %+v", o)
			}
			for _, l := range o.Diff {
				if !printable(l) || strings.Contains(l, "\n") {
					t.Fatalf("accepted diff line %q", l)
				}
			}
		}
	})
}
