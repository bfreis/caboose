package daemonproto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

func TestNewToken(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidToken(tok) || len(tok) != 43 {
			t.Fatalf("token %q", tok)
		}
		if b, err := base64.RawURLEncoding.DecodeString(tok); err != nil || len(b) != 32 {
			t.Fatalf("token %q decodes to %d bytes: %v", tok, len(b), err)
		}
		if seen[tok] {
			t.Fatal("a token came twice")
		}
		seen[tok] = true
	}
}

func TestNewCode(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		c, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidCode(c) || len(c) != CodeLen {
			t.Fatalf("code %q", c)
		}
		if b, err := base64.RawURLEncoding.DecodeString(c); err != nil || len(b) != CodeBytes {
			t.Fatalf("code %q decodes to %d bytes: %v", c, len(b), err)
		}
		if seen[c] {
			t.Fatal("a code came twice")
		}
		seen[c] = true
	}
	for _, s := range []string{"", testToken[:42], testToken + "A", testToken[:42] + "=", testToken[:42] + "+", testToken[:41] + "é"} {
		if ValidCode(s) {
			t.Errorf("ValidCode(%q)", s)
		}
	}
	if CodeTTL != 60*time.Second {
		t.Errorf("CodeTTL %v", CodeTTL)
	}
}

func TestValidToken(t *testing.T) {
	if !ValidToken(testToken) || !ValidToken(strings.Repeat("-", 42)+"_") {
		t.Error("a good token refused")
	}
	for _, s := range []string{"", testToken[:42], testToken + "A", testToken[:42] + "=", testToken[:42] + "+",
		testToken[:42] + "/", testToken[:42] + "\n", testToken[:42] + " ", testToken[:41] + "é"} {
		if ValidToken(s) || ValidNonce(s) {
			t.Errorf("%q taken", s)
		}
	}
}

// The token is compared whole: a wrong one, one a character short or
// long, a prefix, and anything against a want that is no token are
// refused.
func TestTokenEqual(t *testing.T) {
	if !TokenEqual(testToken, testToken) {
		t.Fatal("the token refused")
	}
	other := "x" + testToken[1:]
	for _, c := range []struct{ got, want string }{
		{other, testToken}, {testToken[:42], testToken}, {testToken + "x", testToken},
		{"", testToken}, {testToken[:10], testToken}, {"", ""}, {testToken, ""},
		{testToken[:42], testToken[:42]}, {"=" + testToken[1:], "=" + testToken[1:]},
		{strings.ToUpper(testToken), testToken},
	} {
		if TokenEqual(c.got, c.want) {
			t.Errorf("TokenEqual(%q, %q)", c.got, c.want)
		}
	}
}

// The compare is constant time: TokenEqual and WebProofEqual compare the
// secret only through crypto/subtle or hmac.Equal, never with == or a
// byte compare that stops at the first difference.
func TestTokenCompareIsConstantTime(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "web.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"TokenEqual": "subtle.ConstantTimeCompare", "WebProofEqual": "hmac.Equal"}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || want[fn.Name.Name] == "" {
			continue
		}
		used := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if s, ok := n.Fun.(*ast.SelectorExpr); ok {
					name := s.X.(*ast.Ident).Name + "." + s.Sel.Name
					switch name {
					case want[fn.Name.Name]:
						used = true
					case "bytes.Equal", "strings.EqualFold", "strings.Compare", "bytes.Compare", "strings.HasPrefix", "bytes.HasPrefix", "reflect.DeepEqual":
						t.Errorf("%s calls %s", fn.Name.Name, name)
					}
				}
			case *ast.BinaryExpr:
				if n.Op == token.EQL || n.Op == token.NEQ {
					// Only lengths, fixed numbers and the constant-time
					// compare's result are compared directly.
					for _, side := range []ast.Expr{n.X, n.Y} {
						if !isLenOrConst(side) {
							t.Errorf("%s compares %T with %s", fn.Name.Name, side, n.Op)
						}
					}
				}
			}
			return true
		})
		if !used {
			t.Errorf("%s does not call %s", fn.Name.Name, want[fn.Name.Name])
		}
		delete(want, fn.Name.Name)
	}
	if len(want) != 0 {
		t.Errorf("not found: %v", want)
	}
}

func isLenOrConst(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return e.Name == "TokenLen"
	case *ast.CallExpr:
		switch f := e.Fun.(type) {
		case *ast.Ident:
			return f.Name == "len"
		case *ast.SelectorExpr:
			// Its result, an int.
			return f.Sel.Name == "ConstantTimeCompare"
		}
	}
	return false
}

// The proof is the HMAC-SHA256, keyed with the token, of the domain, the
// origin and the nonce: a vector computed elsewhere, and a reference
// computed here.
func TestWebProof(t *testing.T) {
	nonce := strings.Repeat("n", 43)
	origin := "http://127.0.0.1:7171"
	got := WebProof(testToken, origin, nonce)
	if got != "k_lWEWhX6demBpXsae3JEFRHmn9meQbECiXvvp1PYZo" {
		t.Errorf("proof %q", got)
	}
	m := hmac.New(sha256.New, []byte(testToken))
	m.Write([]byte("caboose-web-proof-v1\n" + origin + "\n" + nonce))
	if ref := base64.RawURLEncoding.EncodeToString(m.Sum(nil)); got != ref {
		t.Errorf("proof %q, reference %q", got, ref)
	}
	if !WebProofEqual(got, testToken, origin, nonce) {
		t.Error("WebProofEqual refused the proof")
	}
	// Each input changes it.
	other := map[string]string{
		"nonce":  WebProof(testToken, origin, strings.Repeat("m", 43)),
		"nonce2": WebProof(testToken, origin, nonce[:42]+"o"),
		"origin": WebProof(testToken, "http://127.0.0.1:7172", nonce),
		"token":  WebProof("x"+testToken[1:], origin, nonce),
		"domain": macOf(testToken, origin+"\n"+nonce),
	}
	for what, p := range other {
		if p == got {
			t.Errorf("the proof does not change with the %s", what)
		}
		if WebProofEqual(p, testToken, origin, nonce) {
			t.Errorf("WebProofEqual took the proof of another %s", what)
		}
	}
	if WebProofEqual(got[:42], testToken, origin, nonce) || WebProofEqual("", testToken, origin, nonce) {
		t.Error("WebProofEqual took a cut proof")
	}
}

// macOf is the HMAC-SHA256 of msg keyed with key, base64url.
func macOf(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func TestWebResultValidate(t *testing.T) {
	url := "http://127.0.0.1:7171/"
	good := []WebResult{
		{},
		{WebState: WebState{Addr: "127.0.0.1:7171", Error: "the page is not served yet"}, URL: url},
		{WebState: WebState{Listening: true, Addr: "127.0.0.1:7171", Since: time.Now()}, URL: url, CodeURL: url + "#code=" + testToken},
	}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if r := good[2]; r.Code() != testToken {
		t.Errorf("Code %q", r.Code())
	}
	if good[1].Code() != "" {
		t.Error("a code from none")
	}
	bad := []WebResult{
		{CodeURL: url + "#code=" + testToken},
		{URL: url, CodeURL: url + "#code=" + testToken[:42]},
		{URL: url, CodeURL: url + "#code=" + testToken + "&to=x"},
		{URL: url, CodeURL: "http://127.0.0.1:7172/#code=" + testToken},
		{URL: url, CodeURL: url + "?code=" + testToken},
		{URL: url, CodeURL: url + "#token=" + testToken},
		{URL: "https://127.0.0.1:7171/"},
		{URL: "http://127.0.0.1:7171"},
		{WebState: WebState{Error: "bad\x1b[0m"}},
		{WebState: WebState{Addr: "a\u202eb"}},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v taken", r)
		}
	}
}

// status carries the web state, checked like the rest, and never a token.
func TestStatusWebState(t *testing.T) {
	st := StatusResult{Daemon: DaemonInfo{Version: "v1", Web: &WebState{Addr: "127.0.0.1:7171", Error: "x\x07"}}, Envs: []EnvStatus{}}
	if st.Validate() == nil {
		t.Error("a control character in the web state taken")
	}
	st.Daemon.Web.Error = "address in use"
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"web":{"listening":false,"addr":"127.0.0.1:7171","error":"address in use"}`) {
		t.Errorf("status %s", b)
	}
	if strings.Contains(string(b), "token") {
		t.Errorf("a token field in status: %s", b)
	}
	st.Daemon.Web = nil
	b, _ = json.Marshal(st)
	if strings.Contains(string(b), `"web"`) {
		t.Errorf("a web state from none: %s", b)
	}
}

// Whatever bytes arrive as web's result, what validates hands out a
// code only in a fragment of the page's own address.
func FuzzWebResult(f *testing.F) {
	for _, s := range []string{`{}`,
		`{"listening":true,"addr":"127.0.0.1:7171","url":"http://127.0.0.1:7171/","code_url":"http://127.0.0.1:7171/#code=` + testToken + `"}`,
		`{"listening":false,"addr":"127.0.0.1:7171","error":"in use","since":"2026-01-01T00:00:00Z","url":"http://127.0.0.1:7171/"}`,
		`{"url":"http://x/","code_url":"http://x/#code=` + testToken + `x"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var r WebResult
		if json.Unmarshal(b, &r) != nil || r.Validate() != nil {
			return
		}
		for _, s := range []string{r.Addr, r.Error, r.URL, r.CodeURL} {
			if !printable(s) {
				t.Fatalf("accepted %q", s)
			}
		}
		if r.CodeURL != "" {
			if !strings.HasPrefix(r.URL, "http://") || r.CodeURL != r.URL+"#code="+r.Code() || !ValidCode(r.Code()) {
				t.Fatalf("accepted %+v", r)
			}
		}
	})
}
