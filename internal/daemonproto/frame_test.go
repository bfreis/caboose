package daemonproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func mustResult(t *testing.T, id uint64, v any) *Frame {
	t.Helper()
	f, err := Result(id, v)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRoundTrip(t *testing.T) {
	req, _ := Request(7, MethodEnsureLink, &EnvParams{Env: "foo"})
	frames := []*Frame{
		Hello(ClientCLI, "v1.2.3"),
		Hello(ClientDaemon, "dev"),
		req,
		mustResult(t, 7, &LinkResult{Action: ActionStarted, Env: EnvStatus{Name: "foo", Sandbox: SandboxRunning, Link: LinkRunning}}),
		mustResult(t, 8, nil),
		Failure(9, NewError(KindUnknownEnv, "no environment %q", "x")),
		Failure(0, NewError(KindProtocolMismatch, "old")),
	}
	var buf bytes.Buffer
	for _, f := range frames {
		if err := Write(&buf, f); err != nil {
			t.Fatalf("write %+v: %v", f, err)
		}
	}
	r := NewReader(&buf)
	for i, want := range frames {
		got, err := r.Read()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Errorf("frame %d: got %s want %s", i, b, a)
		}
	}
	if _, err := r.Read(); err != io.EOF {
		t.Errorf("after the last frame: %v, want EOF", err)
	}
}

func TestErrorKindsRoundTrip(t *testing.T) {
	for _, k := range []string{KindBadRequest, KindUnknownMethod, KindUnknownEnv, KindProtocolMismatch, KindBusy, KindInternal} {
		line, err := Encode(Failure(3, NewError(k, "why")))
		if err != nil {
			t.Fatal(err)
		}
		f, err := Decode(bytes.TrimSuffix(line, []byte("\n")))
		if err != nil {
			t.Fatal(err)
		}
		if f.Error.Kind != k || f.Error.Message != "why" || !errors.Is(f.Error, &Error{Kind: k}) {
			t.Errorf("%s: %+v", k, f.Error)
		}
		if Kind(f.Error) != k {
			t.Errorf("Kind(%s)", k)
		}
	}
	if errors.Is(&Error{Kind: KindBusy}, &Error{Kind: KindInternal}) {
		t.Error("different kinds compare equal")
	}
	if Kind(io.EOF) != KindInternal {
		t.Error("a foreign error is internal")
	}
}

func TestOversizeRefused(t *testing.T) {
	big := strings.Repeat("a", MaxLine)
	// Reader: a line over the bound is refused without being read to its end.
	r := NewReader(strings.NewReader(`{"type":"request","id":1,"method":"ping","params":{"k":"` + big + "\"}}\n"))
	if _, err := r.Read(); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("oversize line: %v", err)
	}
	// Exactly at the bound is fine; one byte more is not.
	pad := func(n int) []byte {
		prefix := `{"type":"request","id":1,"method":"ping","params":{"k":"`
		suffix := `"}}`
		return []byte(prefix + strings.Repeat("a", n-len(prefix)-len(suffix)) + suffix)
	}
	if _, err := Decode(pad(MaxLine - 1)); err != nil { // plus the newline: MaxLine
		t.Errorf("at the bound: %v", err)
	}
	if _, err := Decode(pad(MaxLine)); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("over the bound: %v", err)
	}
	// Encode refuses to send one.
	f := &Frame{Type: TypeRequest, ID: 1, Method: "ping", Params: json.RawMessage(`{"k":"` + big + `"}`)}
	if _, err := Encode(f); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("encode oversize: %v", err)
	}
	// A long message and a long name.
	if err := Failure(1, &Error{Kind: KindInternal, Message: strings.Repeat("m", maxMessage+1)}).Validate(); err == nil {
		t.Error("long message accepted")
	}
	if err := Failure(1, &Error{Kind: strings.Repeat("k", maxName+1)}).Validate(); err == nil {
		t.Error("long kind accepted")
	}
	long := NewError(KindInternal, "%s", strings.Repeat("é", maxMessage))
	if err := Failure(1, long).Validate(); err != nil {
		t.Errorf("NewError did not cut its message to fit: %v", err)
	}
}

// endless yields bytes with no newline until stopped counting.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.n += len(p)
	return len(p), nil
}

func TestReaderStopsAtTheBoundWithoutANewline(t *testing.T) {
	src := &endless{}
	if _, err := NewReader(src).Read(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("got %v", err)
	}
	if src.n > MaxLine+8192 {
		t.Errorf("read %d bytes before refusing", src.n)
	}
}

func TestKindOfUnknownIsInternal(t *testing.T) {
	if Kind(&Error{Kind: "from_the_future"}) != KindInternal {
		t.Error("unknown kind passed through")
	}
	if Kind(&Error{Kind: KindBusy}) != KindBusy {
		t.Error("known kind lost")
	}
}

func TestFormatCharactersRefused(t *testing.T) {
	for _, r := range []rune{0x200b, 0x2060, 0x061c, 0xe0041, 0x202e, 0x0a, 0x85} {
		if printable("a" + string(r)) {
			t.Errorf("U+%04X accepted", r)
		}
	}
	if !printable("héllo wörld 日本") {
		t.Error("ordinary text refused")
	}
}

func TestReaderEnds(t *testing.T) {
	if _, err := NewReader(strings.NewReader(`{"type":"hello"`)).Read(); err != io.ErrUnexpectedEOF {
		t.Errorf("partial line: %v", err)
	}
	if _, err := NewReader(strings.NewReader("\n")).Read(); !errors.Is(err, ErrBadFrame) {
		t.Errorf("empty line: %v", err)
	}
}

func TestGarbledRefused(t *testing.T) {
	for name, line := range map[string]string{
		"not json":           `hello`,
		"array":              `[1]`,
		"null":               `null`,
		"trailing data":      `{"type":"hello","protocol":1,"version":"v","client":"cli"} {}`,
		"trailing junk":      `{"type":"hello","protocol":1,"version":"v","client":"cli"}x`,
		"unknown type":       `{"type":"nope"}`,
		"no type":            `{"id":1,"method":"ping"}`,
		"hello no protocol":  `{"type":"hello","version":"v","client":"cli"}`,
		"hello no version":   `{"type":"hello","protocol":1,"client":"cli"}`,
		"hello bad client":   `{"type":"hello","protocol":1,"version":"v","client":"root"}`,
		"hello with id":      `{"type":"hello","protocol":1,"version":"v","client":"cli","id":1}`,
		"hello ctl version":  `{"type":"hello","protocol":1,"version":"v\u001b[2J","client":"cli"}`,
		"request no id":      `{"type":"request","method":"ping"}`,
		"request no method":  `{"type":"request","id":1}`,
		"request bad method": `{"type":"request","id":1,"method":"Ping; rm"}`,
		"request params arr": `{"type":"request","id":1,"method":"ping","params":[1]}`,
		"request w/ result":  `{"type":"request","id":1,"method":"ping","result":{}}`,
		"response neither":   `{"type":"response","id":1}`,
		"response both":      `{"type":"response","id":1,"result":{},"error":{"kind":"busy","message":""}}`,
		"result id 0":        `{"type":"response","result":{}}`,
		"result not object":  `{"type":"response","id":1,"result":3}`,
		"error bad kind":     `{"type":"response","id":1,"error":{"kind":"Bad Kind","message":""}}`,
		"error ctl message":  `{"type":"response","id":1,"error":{"kind":"busy","message":"a\nb"}}`,
		"error bidi message": `{"type":"response","id":1,"error":{"kind":"busy","message":"a\u202eb"}}`,
		"event":              `{"type":"event","event":"x"}`,
		"id wrong type":      `{"type":"request","id":"1","method":"ping"}`,
		"negative id":        `{"type":"request","id":-1,"method":"ping"}`,
	} {
		if f, err := Decode([]byte(line)); err == nil {
			t.Errorf("%s: accepted as %+v", name, f)
		} else if !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: error %v is not ErrBadFrame", name, err)
		}
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	f, err := Decode([]byte(`{"type":"request","id":2,"method":"ping","later":{"x":1}}`))
	if err != nil || f.Method != MethodPing {
		t.Fatalf("%v %+v", err, f)
	}
	// null payloads are absent.
	f, err = Decode([]byte(`{"type":"request","id":2,"method":"ping","params":null}`))
	if err != nil || f.Params != nil {
		t.Fatalf("%v %+v", err, f)
	}
}

func TestDecodeParams(t *testing.T) {
	good, _ := Request(1, MethodStopLink, &EnvParams{Env: "foo"})
	var p EnvParams
	if e := DecodeParams(good, &p); e != nil || p.Env != "foo" {
		t.Fatalf("%v %+v", e, p)
	}
	for _, env := range []string{"", "Foo", "a/b", "../x", strings.Repeat("a", 33), "a b"} {
		f, _ := Request(1, MethodStopLink, &EnvParams{Env: env})
		if e := DecodeParams(f, &EnvParams{}); e == nil || e.Kind != KindBadRequest {
			t.Errorf("env %q: %v", env, e)
		}
	}
	f, _ := Request(1, MethodStopLink, nil)
	if e := DecodeParams(f, &EnvParams{}); e == nil || e.Kind != KindBadRequest {
		t.Errorf("no params: %v", e)
	}
	f = &Frame{Type: TypeRequest, ID: 1, Method: MethodReplace, Params: json.RawMessage(`{"version":3}`)}
	if e := DecodeParams(f, &ReplaceParams{}); e == nil || e.Kind != KindBadRequest {
		t.Errorf("wrong type: %v", e)
	}
	rp, _ := Request(1, MethodReplace, &ReplaceParams{Version: "v1\x1b"})
	if e := DecodeParams(rp, &ReplaceParams{}); e == nil {
		t.Error("control character in a version accepted")
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		client, daemon int
		want           Compat
	}{
		{1, 1, CompatSame}, {2, 1, CompatClientNewer}, {1, 2, CompatClientOlder}, {5, 5, CompatSame},
	} {
		if got := Compare(c.client, c.daemon); got != c.want {
			t.Errorf("Compare(%d,%d) = %v, want %v", c.client, c.daemon, got, c.want)
		}
	}
}

func TestSocketPathLimit(t *testing.T) {
	if err := CheckSocketPath("/" + strings.Repeat("a", MaxSocketPath-1)); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	err := CheckSocketPath("/" + strings.Repeat("a", MaxSocketPath))
	if err == nil || !strings.Contains(err.Error(), "CABOOSE_HOME") {
		t.Errorf("over the limit: %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range []string{
		`{"type":"hello","protocol":1,"version":"v1","client":"cli"}`,
		`{"type":"request","id":1,"method":"ensure_link","params":{"env":"foo"}}`,
		`{"type":"response","id":1,"result":{"action":"started"}}`,
		`{"type":"request","id":2,"method":"web","params":{"rotate":true,"code":true}}`,
		`{"type":"request","id":3,"method":"edit_egress","params":{"env":"foo","add_allow":["git.corp.example"]}}`,
		`{"type":"request","id":4,"method":"proposal_apply","params":{"env":"foo","file":"x.toml","hash":"abababababababababababababababababababababababababababababababab","typed":"x"}}`,
		`{"type":"response","id":2,"result":{"listening":true,"addr":"127.0.0.1:7171","url":"http://127.0.0.1:7171/","code_url":"http://127.0.0.1:7171/#code=0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"}}`,
		`{"type":"request","id":5,"method":"app_attach","params":{"capabilities":["notifications","dialogs"],"version":"v1.0.0"}}`,
		`{"type":"request","id":6,"method":"notifications","params":{"after":3}}`,
		`{"type":"request","id":8,"method":"dialogs","params":{}}`,
		`{"type":"request","id":9,"method":"sandbox_restart","params":{"env":"alpha"}}`,
		`{"type":"response","id":9,"result":{"job":{"kind":"restart","state":"running","since":"2026-10-18T12:00:00Z"}}}`,
		`{"type":"response","id":10,"result":{"daemon":{},"envs":[{"name":"alpha","sandbox":"running","link":"running","job":{"kind":"stop","state":"failed","since":"2026-10-18T12:00:00Z","finished":"2026-10-18T12:01:00Z","error":"no"}}]}}`,
		`{"type":"response","id":8,"result":{"dialog":{"env":"alpha","id":12}}}`,
		`{"type":"response","id":6,"result":{"items":[{"seq":4,"env":"alpha","origin":"sandbox","title":"From the alpha sandbox","text":"t: x","route":"env/alpha","at":"2026-10-18T12:00:00Z"}],"next":4}}`,
		`{"type":"response","id":7,"result":{"daemon":{},"envs":[],"attention":[{"env":"alpha","kind":"proposals","text":"1 pending proposal","route":"proposals","since":"2026-10-18T12:00:00Z"}]}}`,
		`{"type":"response","id":0,"error":{"kind":"busy","message":"m"}}`,
		`{"type":"event","event":"x","data":{}}`,
		`{"type":"hello"`, `[]`, ``, `{"type":"request","id":1,"method":"ping","params":null}`,
		"{\"type\":\"response\",\"id\":1,\"error\":{\"kind\":\"k\",\"message\":\"\u202e\"}}",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		fr, err := Decode(line)
		if err != nil {
			if !errors.Is(err, ErrBadFrame) && !errors.Is(err, ErrLineTooLong) {
				t.Fatalf("error of an unexpected kind: %v", err)
			}
			return
		}
		// Whatever decodes is valid, and re-encodes to a fixed point.
		if err := fr.Validate(); err != nil {
			t.Fatalf("decoded an invalid frame: %v", err)
		}
		e1, err := Encode(fr)
		if err != nil {
			t.Fatalf("encode of a decoded frame: %v", err)
		}
		if len(e1) > MaxLine || e1[len(e1)-1] != '\n' || bytes.Contains(e1[:len(e1)-1], []byte("\n")) {
			t.Fatalf("bad encoding %q", e1)
		}
		fr2, err := Decode(bytes.TrimSuffix(e1, []byte("\n")))
		if err != nil {
			t.Fatalf("decode of an encoding: %v", err)
		}
		e2, err := Encode(fr2)
		if err != nil || !bytes.Equal(e1, e2) {
			t.Fatalf("not a fixed point: %q vs %q (%v)", e1, e2, err)
		}
		// The reader agrees with Decode on a single line.
		if !bytes.Contains(line, []byte("\n")) {
			rf, err := NewReader(bytes.NewReader(append(append([]byte(nil), line...), '\n'))).Read()
			if err != nil {
				t.Fatalf("reader refused what Decode took: %v", err)
			}
			e3, _ := Encode(rf)
			if !bytes.Equal(e1, e3) {
				t.Fatalf("reader and Decode differ")
			}
		}
	})
}
