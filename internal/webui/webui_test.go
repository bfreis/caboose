package webui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeDispatch records every call; allowed is the web page's allowlist.
type fakeDispatch struct {
	mu      sync.Mutex
	allowed map[string]bool
	calls   []fakeCall
	result  json.RawMessage
	err     *daemonproto.Error
}

type fakeCall struct {
	method string
	params string
}

func (d *fakeDispatch) Allowed(m string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.allowed[m]
}

func (d *fakeDispatch) Call(_ context.Context, m string, p json.RawMessage) (json.RawMessage, *daemonproto.Error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, fakeCall{m, string(p)})
	return d.result, d.err
}

func (d *fakeDispatch) called() []fakeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]fakeCall(nil), d.calls...)
}

// fakeClock is a clock the test moves.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// page is the handler served on 127.0.0.1:0, from a listener of Listen's.
type page struct {
	t        *testing.T
	srv      *httptest.Server
	origin   string
	host     string
	port     int
	d        *fakeDispatch
	clock    *fakeClock
	log      *syncBuf
	mu       sync.Mutex
	token    string
	activity int
	codes    map[string]bool // one-time codes the fake redeem takes, once each
	redeemed []string
}

var testToken = strings.Repeat("Ab3_-", 8) + "xyz" // 43 characters

func newPage(t *testing.T) *page {
	t.Helper()
	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := Bound(ln)
	if !ok || bound.Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("bound %v", ln.Addr())
	}
	p := &page{t: t, origin: Origin(bound), host: bound.String(), port: int(bound.Port()), token: testToken,
		d:     &fakeDispatch{allowed: map[string]bool{"ping": true, "status": true, "requests": true, "answer": true}, result: json.RawMessage(`{"ok":true}`)},
		clock: &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}, log: &syncBuf{}}
	p.codes = map[string]bool{}
	h := Handler(Options{Origin: p.origin, Token: p.currentToken, Redeem: p.redeem, Dispatch: p.d, Activity: p.active,
		Log: log.New(p.log, "", 0), Now: p.clock.Now})
	p.srv = httptest.NewUnstartedServer(h)
	p.srv.Config = NewServer(h, nil) // the page's own server settings
	p.srv.Listener.Close()
	p.srv.Listener = ln
	p.srv.Start()
	t.Cleanup(p.srv.Close)
	return p
}

func (p *page) currentToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.token
}

// redeem is a daemon's: a code it made opens the token once.
func (p *page) redeem(code string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.redeemed = append(p.redeemed, code)
	ok := p.codes[code]
	delete(p.codes, code)
	return p.token, ok && p.token != ""
}

func (p *page) addCode(c string) {
	p.mu.Lock()
	p.codes[c] = true
	p.mu.Unlock()
}

func (p *page) redeemCalls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.redeemed...)
}

func (p *page) setToken(tok string) {
	p.mu.Lock()
	p.token = tok
	p.mu.Unlock()
}

func (p *page) active() {
	p.mu.Lock()
	p.activity++
	p.mu.Unlock()
}

func (p *page) activities() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activity
}

// req is an API request with every check passing; edit then changes it.
func (p *page) req(method, path, body string, edit func(*http.Request)) *http.Request {
	r, err := http.NewRequest(method, p.origin+path, strings.NewReader(body))
	if err != nil {
		p.t.Fatal(err)
	}
	r.Header.Set("Origin", p.origin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(TokenHeader, p.currentToken())
	if edit != nil {
		edit(r)
	}
	return r
}

type resp struct {
	status int
	header http.Header
	body   string
}

// do sends r and checks what every response must carry.
func (p *page) do(r *http.Request) resp {
	p.t.Helper()
	res, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		p.t.Fatalf("%s %s: %v", r.Method, r.URL, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	checkHeaders(p.t, r.Method+" "+r.URL.String(), res.Header)
	return resp{res.StatusCode, res.Header, string(b)}
}

// raw sends a request written out by hand, for what net/http's client
// will not send.
func (p *page) raw(text string) resp {
	p.t.Helper()
	c, err := net.Dial("tcp4", p.host)
	if err != nil {
		p.t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, text); err != nil {
		p.t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		p.t.Fatalf("%q: %v", text, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, string(b)}
}

// wantHeaders is what every response must carry, written out here rather
// than taken from the package, so that a row dropped there fails.
var wantHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'; manifest-src 'none'; worker-src 'none'; require-trusted-types-for 'script'; trusted-types 'none'"},
	{"X-Frame-Options", "DENY"},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
	{"Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), serial=(), hid=(), bluetooth=(), payment=(), clipboard-read=(), display-capture=()"},
	{"Cache-Control", "no-store"},
}

// checkHeaders holds that a response carries the security headers,
// exactly, and nothing of CORS, cookies or a server name.
func checkHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for _, kv := range wantHeaders {
		if got := h.Values(kv[0]); len(got) != 1 || got[0] != kv[1] {
			t.Errorf("%s: %s is %q", what, kv[0], got)
		}
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%s: a CORS header %s", what, k)
		}
	}
	for _, k := range []string{"Set-Cookie", "Server"} {
		if h.Get(k) != "" {
			t.Errorf("%s: %s sent", what, k)
		}
	}
	if h.Get("Content-Type") == "" {
		t.Errorf("%s: no Content-Type", what)
	}
}

// refused checks a refusal: its status, its kind, and that nothing was
// dispatched.
func (p *page) refused(what string, r resp, status int, kind string) {
	p.t.Helper()
	if r.status != status {
		p.t.Errorf("%s: status %d, want %d (%s)", what, r.status, status, r.body)
	}
	if kind != "" {
		var e struct {
			Error daemonproto.Error `json:"error"`
		}
		if err := json.Unmarshal([]byte(r.body), &e); err != nil || e.Error.Kind != kind {
			p.t.Errorf("%s: body %q, want kind %s", what, r.body, kind)
		}
	}
	if c := p.d.called(); len(c) != 0 {
		p.t.Errorf("%s: dispatched %v", what, c)
	}
	p.d.mu.Lock()
	p.d.calls = nil
	p.d.mu.Unlock()
}

func TestAPICallDispatched(t *testing.T) {
	p := newPage(t)
	r := p.do(p.req("POST", "/api/requests", `{"env":"alpha"}`, nil))
	if r.status != 200 || r.body != `{"result":{"ok":true}}`+"\n" || r.header.Get("Content-Type") != typeJSON {
		t.Fatalf("%d %q %v", r.status, r.body, r.header)
	}
	if c := p.d.called(); len(c) != 1 || c[0] != (fakeCall{"requests", `{"env":"alpha"}`}) {
		t.Errorf("calls %v", c)
	}
	if p.activities() != 1 {
		t.Errorf("activity %d", p.activities())
	}
	// An empty body is {}; the charset may be named.
	p.d.calls = nil
	r = p.do(p.req("POST", "/api/ping", "", func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }))
	if c := p.d.called(); r.status != 200 || len(c) != 1 || c[0].params != "{}" {
		t.Errorf("%d %v", r.status, c)
	}
	// A nil result is {}.
	p.d.result, p.d.calls = nil, nil
	if r := p.do(p.req("POST", "/api/ping", "{}", nil)); r.body != `{"result":{}}`+"\n" {
		t.Errorf("nil result: %q", r.body)
	}
}

func TestTokenRequired(t *testing.T) {
	p := newPage(t)
	tok := p.currentToken()
	cases := map[string]func(*http.Request){
		"missing":      func(r *http.Request) { r.Header.Del(TokenHeader) },
		"empty":        func(r *http.Request) { r.Header.Set(TokenHeader, "") },
		"wrong":        func(r *http.Request) { r.Header.Set(TokenHeader, strings.Repeat("A", 43)) },
		"one short":    func(r *http.Request) { r.Header.Set(TokenHeader, tok[:42]) },
		"one long":     func(r *http.Request) { r.Header.Set(TokenHeader, tok+"A") },
		"a prefix":     func(r *http.Request) { r.Header.Set(TokenHeader, tok[:10]) },
		"last differs": func(r *http.Request) { r.Header.Set(TokenHeader, tok[:42]+"Q") },
		"upper case":   func(r *http.Request) { r.Header.Set(TokenHeader, strings.ToUpper(tok)) },
		"twice":        func(r *http.Request) { r.Header.Add(TokenHeader, tok) },
		"in a cookie":  func(r *http.Request) { r.Header.Del(TokenHeader); r.Header.Set("Cookie", "token="+tok) },
		"in the query": func(r *http.Request) { r.Header.Del(TokenHeader); r.URL.RawQuery = "token=" + tok },
		"another name": func(r *http.Request) { r.Header.Del(TokenHeader); r.Header.Set("Authorization", "Bearer "+tok) },
	}
	for name, edit := range cases {
		r := p.do(p.req("POST", "/api/answer", `{}`, edit))
		p.refused(name, r, 401, KindUnauthorized)
		if !strings.Contains(r.body, "caboose ui") || strings.Contains(r.body, tok) {
			t.Errorf("%s: body %q", name, r.body)
		}
	}
	if p.activities() != 0 {
		t.Errorf("an unauthenticated request counted as activity")
	}
	// No token at all: nothing is let in, an empty header included.
	p.setToken("")
	p.refused("no token", p.do(p.req("POST", "/api/ping", `{}`, nil)), 401, KindUnauthorized)
	// Rotation: the old token is refused at once, the new one taken.
	p.setToken(tok)
	p.setToken(strings.Repeat("Z", 43))
	p.refused("old token", p.do(p.req("POST", "/api/ping", `{}`, func(r *http.Request) { r.Header.Set(TokenHeader, tok) })), 401, KindUnauthorized)
	if r := p.do(p.req("POST", "/api/ping", `{}`, nil)); r.status != 200 {
		t.Errorf("new token: %d", r.status)
	}
}

// The Host must be the listener's own: a DNS name rebound to 127.0.0.1
// sends its own.
func TestHostChecked(t *testing.T) {
	p := newPage(t)
	other := strconv.Itoa(p.port + 1)
	for _, host := range []string{"localhost:" + strconv.Itoa(p.port), "evil.example:" + strconv.Itoa(p.port), "evil.example:7171",
		"127.0.0.1:" + other, "127.0.0.1", "[::1]:" + strconv.Itoa(p.port), "127.1:" + strconv.Itoa(p.port),
		"0x7f.0.0.1:" + strconv.Itoa(p.port), p.host + ".", "localhost"} {
		for _, path := range []string{"/", "/static/app.css", "/api/ping", "/api/proof", "/nope"} {
			method := "POST"
			if !strings.HasPrefix(path, "/api/") {
				method = "GET"
			}
			r := p.do(p.req(method, path, `{}`, func(r *http.Request) { r.Host = host }))
			p.refused(host+" "+path, r, http.StatusMisdirectedRequest, "")
			if r.body != "open "+p.origin+"/\n" || r.header.Get("Content-Type") != typeText {
				t.Errorf("%s: %q %q", host, r.body, r.header.Get("Content-Type"))
			}
		}
	}
	// No Host at all (HTTP/1.0).
	r := p.raw("GET / HTTP/1.0\r\n\r\n")
	checkHeaders(t, "no host", r.header)
	p.refused("no host", r, http.StatusMisdirectedRequest, "")
	// An absolute URL in the request line stands in for no Host.
	r = p.raw("POST http://" + p.host + "/api/ping HTTP/1.1\r\nHost: evil.example\r\nOrigin: " + p.origin +
		"\r\nSec-Fetch-Site: same-origin\r\nContent-Type: application/json\r\n" + TokenHeader + ": " + testToken + "\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
	p.refused("absolute form", r, http.StatusMisdirectedRequest, "")
	r = p.raw("GET http://" + p.host + "/ HTTP/1.1\r\nHost: " + p.host + "\r\nConnection: close\r\n\r\n")
	p.refused("absolute form, same host", r, http.StatusMisdirectedRequest, "")
	// OPTIONS * reaches the handler, and is refused with its headers.
	r = p.raw("OPTIONS * HTTP/1.1\r\nHost: " + p.host + "\r\nConnection: close\r\n\r\n")
	checkHeaders(t, "OPTIONS *", r.header)
	p.refused("OPTIONS *", r, http.StatusMisdirectedRequest, "")
	// The listener's own, in any case, is taken.
	if r := p.do(p.req("GET", "/", "", func(r *http.Request) { r.Host = strings.ToUpper(p.host) })); r.status != 200 {
		t.Errorf("own host: %d", r.status)
	}
}

func TestOriginChecked(t *testing.T) {
	p := newPage(t)
	other := "http://127.0.0.1:" + strconv.Itoa(p.port+1)
	for name, edit := range map[string]func(*http.Request){
		"missing":     func(r *http.Request) { r.Header.Del("Origin") },
		"null":        func(r *http.Request) { r.Header.Set("Origin", "null") },
		"other port":  func(r *http.Request) { r.Header.Set("Origin", other) },
		"localhost":   func(r *http.Request) { r.Header.Set("Origin", "http://localhost:"+strconv.Itoa(p.port)) },
		"https":       func(r *http.Request) { r.Header.Set("Origin", "https://"+p.host) },
		"a path":      func(r *http.Request) { r.Header.Set("Origin", p.origin+"/") },
		"another":     func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"twice":       func(r *http.Request) { r.Header.Add("Origin", p.origin) },
		"upper":       func(r *http.Request) { r.Header.Set("Origin", strings.ToUpper(p.origin)) },
		"empty":       func(r *http.Request) { r.Header.Set("Origin", "") },
		"no Host but": func(r *http.Request) { r.Header.Set("Origin", "http://"+p.host+"@evil.example") },
	} {
		// The approve itself, with the right token: CSRF.
		p.refused(name, p.do(p.req("POST", "/api/answer", `{"env":"a","id":1,"scope":"once"}`, edit)), 403, KindForbidden)
		p.refused(name+" (proof)", p.do(p.req("POST", "/api/proof", `{"nonce":"`+strings.Repeat("n", 43)+`"}`, edit)), 403, KindForbidden)
	}
}

func TestFetchSiteChecked(t *testing.T) {
	p := newPage(t)
	for _, site := range []string{"cross-site", "same-site", "none", "", "SAME-ORIGIN", "same-origin, cross-site"} {
		p.refused("api "+site, p.do(p.req("POST", "/api/ping", `{}`, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", site) })), 403, KindForbidden)
	}
	r := p.do(p.req("POST", "/api/ping", `{}`, func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") }))
	p.refused("api missing", r, 403, KindForbidden)
	if !strings.Contains(r.body, "Safari, Chrome or Firefox") {
		t.Errorf("missing: %q", r.body)
	}
	p.refused("api twice", p.do(p.req("POST", "/api/ping", `{}`, func(r *http.Request) { r.Header.Add("Sec-Fetch-Site", "same-origin") })), 403, KindForbidden)
	// The page itself: same-origin, none or not sent.
	for _, site := range []string{"cross-site", "same-site"} {
		for _, path := range []string{"/", "/static/app.css"} {
			p.refused(path+" "+site, p.do(p.req("GET", path, "", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", site) })), 403, KindForbidden)
		}
	}
	p.refused("static none", p.do(p.req("GET", "/static/app.css", "", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") })), 403, KindForbidden)
	for _, site := range []string{"same-origin", "none", ""} {
		r := p.do(p.req("GET", "/", "", func(r *http.Request) {
			if site == "" {
				r.Header.Del("Sec-Fetch-Site")
			} else {
				r.Header.Set("Sec-Fetch-Site", site)
			}
		}))
		if r.status != 200 {
			t.Errorf("/ with %q: %d", site, r.status)
		}
	}
}

func TestFetchDestChecked(t *testing.T) {
	p := newPage(t)
	for _, dest := range []string{"iframe", "frame", "object", "embed", "IFRAME", "fencedframe"} {
		for _, path := range []string{"/", "/static/app.css"} {
			p.refused(path+" "+dest, p.do(p.req("GET", path, "", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", dest) })), 403, KindForbidden)
		}
	}
	for _, dest := range []string{"document", "style"} {
		if r := p.do(p.req("GET", "/", "", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", dest) })); r.status != 200 {
			t.Errorf("%s: %d", dest, r.status)
		}
	}
}

func TestContentTypeChecked(t *testing.T) {
	p := newPage(t)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x",
		"application/json; charset=latin1", "application/jsonx", "application/json; x=y", "text/json", "", "application/json;"} {
		p.refused(ct, p.do(p.req("POST", "/api/answer", `{}`, func(r *http.Request) { r.Header.Set("Content-Type", ct) })), 415, KindUnsupportedMedia)
	}
	p.refused("none", p.do(p.req("POST", "/api/answer", `{}`, func(r *http.Request) { r.Header.Del("Content-Type") })), 415, KindUnsupportedMedia)
	p.refused("twice", p.do(p.req("POST", "/api/answer", `{}`, func(r *http.Request) { r.Header.Add("Content-Type", "application/json") })), 415, KindUnsupportedMedia)
}

func TestAPIMethodsAndNoCORS(t *testing.T) {
	p := newPage(t)
	for _, m := range []string{"GET", "HEAD", "PUT", "DELETE", "PATCH"} {
		r := p.do(p.req(m, "/api/answer", "", nil))
		p.refused(m, r, 405, "")
		if r.header.Get("Allow") != "POST" {
			t.Errorf("%s: Allow %q", m, r.header.Get("Allow"))
		}
	}
	// A preflight is never answered.
	r := p.do(p.req("OPTIONS", "/api/answer", "", func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "content-type, x-caboose-token")
	}))
	p.refused("preflight", r, 405, "")
	r = p.do(p.req("OPTIONS", "/api/answer", "", func(r *http.Request) { r.Header.Set("Access-Control-Request-Method", "POST") }))
	p.refused("preflight, own origin", r, 405, "")
}

// Only what the daemon lets the page call is reached, whatever passes
// the other checks.
func TestOnlyTheWebAllowlist(t *testing.T) {
	p := newPage(t)
	for _, m := range []string{"web", "stop", "replace", "ensure_link", "restart_link", "stop_link", "subscribe", "writable", "nothing"} {
		r := p.do(p.req("POST", "/api/"+m, `{}`, nil))
		p.refused(m, r, 404, daemonproto.KindUnknownMethod)
	}
	for _, path := range []string{"/api/", "/api/Status", "/api/status/x", "/api/st-atus", "/api/" + strings.Repeat("a", 65), "/api/1abc", "/api/st%61tus", "/api//status"} {
		p.refused(path, p.do(p.req("POST", path, `{}`, nil)), 404, daemonproto.KindUnknownMethod)
	}
}

func TestBodyChecked(t *testing.T) {
	p := newPage(t)
	big := `{"x":"` + strings.Repeat("a", MaxBody) + `"}`
	for name, body := range map[string]string{
		"over 64 KiB": big, "two values": `{} {}`, "an array": `[]`, "a string": `"x"`, "null": `null`,
		"not JSON": `{`, "trailing": `{}x`, "a number": `1`,
	} {
		p.refused(name, p.do(p.req("POST", "/api/ping", body, nil)), 400, daemonproto.KindBadRequest)
	}
	p.refused("a query", p.do(p.req("POST", "/api/ping?x=1", `{}`, nil)), 400, daemonproto.KindBadRequest)
	p.refused("an empty query", p.do(p.req("POST", "/api/ping?", `{}`, nil)), 400, daemonproto.KindBadRequest)
	// Just at the limit is taken.
	exact := `{"x":"` + strings.Repeat("a", MaxBody-8) + `"}`
	if r := p.do(p.req("POST", "/api/ping", exact, nil)); r.status != 200 {
		t.Errorf("at the limit: %d %s", r.status, r.body)
	}
}

func TestRateLimited(t *testing.T) {
	p := newPage(t)
	for i := range apiBurst {
		if r := p.do(p.req("POST", "/api/ping", `{}`, nil)); r.status != 200 {
			t.Fatalf("call %d: %d", i, r.status)
		}
	}
	n := len(p.d.called())
	r := p.do(p.req("POST", "/api/ping", `{}`, nil))
	if r.status != 429 || len(p.d.called()) != n {
		t.Fatalf("over the burst: %d", r.status)
	}
	p.clock.add(time.Second)
	for i := range apiRate {
		if r := p.do(p.req("POST", "/api/ping", `{}`, nil)); r.status != 200 {
			t.Fatalf("after a second, call %d: %d", i, r.status)
		}
	}
	if r := p.do(p.req("POST", "/api/ping", `{}`, nil)); r.status != 429 {
		t.Errorf("past the rate: %d", r.status)
	}
	// Proofs have their own, as high as the API's, since the page proves
	// the server before every call.
	if proofRate != apiRate || proofBurst != apiBurst {
		t.Errorf("proof limit %d/%d, API's %d/%d", proofRate, proofBurst, apiRate, apiBurst)
	}
	nonce := `{"nonce":"` + strings.Repeat("n", 43) + `"}`
	for i := range proofBurst {
		if r := p.do(p.req("POST", "/api/proof", nonce, nil)); r.status != 200 {
			t.Fatalf("proof %d: %d", i, r.status)
		}
	}
	if r := p.do(p.req("POST", "/api/proof", nonce, nil)); r.status != 429 {
		t.Errorf("proofs past the burst: %d", r.status)
	}
}

func refProof(token, origin, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("caboose-web-proof-v1\n" + origin + "\n" + nonce))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func proofOf(t *testing.T, r resp) string {
	t.Helper()
	var v struct {
		Result ProofResult `json:"result"`
	}
	if r.status != 200 || json.Unmarshal([]byte(r.body), &v) != nil || v.Result.MAC == "" {
		t.Fatalf("proof: %d %q", r.status, r.body)
	}
	return v.Result.MAC
}

// The proof needs no token, binds the listener's own origin and the
// nonce, and is checked like every other API call.
func TestProof(t *testing.T) {
	p := newPage(t)
	n1, n2 := strings.Repeat("n", 43), strings.Repeat("m", 42)+"_"
	noToken := func(r *http.Request) { r.Header.Del(TokenHeader) }
	mac := proofOf(t, p.do(p.req("POST", "/api/proof", `{"nonce":"`+n1+`"}`, noToken)))
	if mac != refProof(testToken, p.origin, n1) {
		t.Errorf("mac %s, want %s", mac, refProof(testToken, p.origin, n1))
	}
	if strings.Contains(mac, testToken) {
		t.Error("the token in the proof")
	}
	if m2 := proofOf(t, p.do(p.req("POST", "/api/proof", `{"nonce":"`+n2+`"}`, noToken))); m2 == mac || m2 != refProof(testToken, p.origin, n2) {
		t.Error("the proof does not bind the nonce")
	}
	// Another listener, another origin, another MAC, for the same nonce.
	q := newPage(t)
	if m3 := proofOf(t, q.do(q.req("POST", "/api/proof", `{"nonce":"`+n1+`"}`, noToken))); m3 == mac || m3 != refProof(testToken, q.origin, n1) {
		t.Error("the proof does not bind the origin")
	}
	if len(p.d.called()) != 0 || p.activities() != 0 {
		t.Error("a proof was dispatched or counted as activity")
	}
	for _, body := range []string{`{}`, `{"nonce":""}`, `{"nonce":"` + n1[:42] + `"}`, `{"nonce":"` + n1 + `a"}`,
		`{"nonce":"` + n1[:42] + `="}`, `{"nonce":"` + n1[:42] + `/"}`, `{"nonce":"` + n1 + `","origin":"http://evil.example"}`, `{"nonce":1}`} {
		p.clock.add(time.Second)
		p.refused(body, p.do(p.req("POST", "/api/proof", body, noToken)), 400, daemonproto.KindBadRequest)
	}
	// Checks 1-7 hold: never a MAC to another origin or site.
	for name, edit := range map[string]func(*http.Request){
		"origin":       func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"fetch site":   func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"content type": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"host":         func(r *http.Request) { r.Host = "evil.example:" + strconv.Itoa(p.port) },
	} {
		r := p.do(p.req("POST", "/api/proof", `{"nonce":"`+n1+`"}`, edit))
		if r.status == 200 || strings.Contains(r.body, "mac") {
			t.Errorf("%s: %d %q", name, r.status, r.body)
		}
	}
	if r := p.do(p.req("GET", "/api/proof", "", nil)); r.status != 405 {
		t.Errorf("GET proof: %d", r.status)
	}
	// No token: no proof.
	p.clock.add(time.Second)
	p.setToken("")
	p.refused("no token", p.do(p.req("POST", "/api/proof", `{"nonce":"`+n1+`"}`, noToken)), 503, daemonproto.KindUnavailable)
}

func TestDispatchErrors(t *testing.T) {
	p := newPage(t)
	for kind, status := range map[string]int{
		daemonproto.KindBadRequest: 400, daemonproto.KindUnknownMethod: 404, daemonproto.KindUnknownEnv: 404,
		daemonproto.KindNotFound: 404, daemonproto.KindAlreadyAnswered: 409, daemonproto.KindStaleDiff: 409,
		daemonproto.KindNotOwner: 409, daemonproto.KindBusy: 503, daemonproto.KindBrokerUnavailable: 503,
		daemonproto.KindTimeout: 504, daemonproto.KindInternal: 500, daemonproto.KindProtocolMismatch: 500,
		"something_new": 500, daemonproto.KindNotAllowed: 404,
	} {
		p.d.err = &daemonproto.Error{Kind: kind, Message: "no\x1b[31m good \u202e"}
		r := p.do(p.req("POST", "/api/ping", `{}`, nil))
		var e struct {
			Error daemonproto.Error `json:"error"`
		}
		if r.status != status || json.Unmarshal([]byte(r.body), &e) != nil {
			t.Errorf("%s: %d %q", kind, r.status, r.body)
			continue
		}
		want := kind
		switch kind {
		case "something_new":
			want = daemonproto.KindInternal
		case daemonproto.KindNotAllowed:
			want = daemonproto.KindUnknownMethod
		}
		if e.Error.Kind != want || strings.ContainsAny(e.Error.Message, "\x1b\u202e") {
			t.Errorf("%s: %+v", kind, e.Error)
		}
	}
}

func TestStatic(t *testing.T) {
	p := newPage(t)
	get := func(path string) resp { return p.do(p.req("GET", path, "", nil)) }
	if r := get("/"); r.status != 200 || r.header.Get("Content-Type") != typeHTML || r.body != string(indexAsset.body) {
		t.Errorf("/: %d %q", r.status, r.header.Get("Content-Type"))
	}
	for name, typ := range served {
		if r := get("/static/" + name); r.status != 200 || r.header.Get("Content-Type") != typ || r.body != string(assets[name].body) {
			t.Errorf("%s: %d %q", name, r.status, r.header.Get("Content-Type"))
		}
		// A query is ignored on a static file.
		if r := get("/static/" + name + "?v=1"); r.status != 200 {
			t.Errorf("%s with a query: %d", name, r.status)
		}
		if r := p.do(p.req("POST", "/static/"+name, "", nil)); r.status != 405 {
			t.Errorf("POST %s: %d", name, r.status)
		}
	}
	if r := p.do(p.req("HEAD", "/", "", nil)); r.status != 200 || r.body != "" {
		t.Errorf("HEAD /: %d %q", r.status, r.body)
	}
	if r := p.do(p.req("POST", "/", "", nil)); r.status != 405 {
		t.Errorf("POST /: %d", r.status)
	}
	for _, path := range []string{"/static/", "/static", "/static/x.test.mjs", "/static/index.html", "/static/nothing.js",
		"/index.html", "/static/app.css/", "/static/%61pp.css", "/favicon.ico", "/api"} {
		if r := get(path); r.status != 404 {
			t.Errorf("%s: %d", path, r.status)
		}
	}
	if r := p.do(p.req("POST", "/favicon.ico", "", nil)); r.status != 404 {
		t.Errorf("POST unknown: %d", r.status)
	}
	// Paths net/http's client would clean.
	for _, path := range []string{"/static/../go.mod", "/static/../webui.go", "/static/./app.css", "/../go.mod"} {
		r := p.raw("GET " + path + " HTTP/1.1\r\nHost: " + p.host + "\r\nConnection: close\r\n\r\n")
		checkHeaders(t, path, r.header)
		if r.status != 404 {
			t.Errorf("%s: %d", path, r.status)
		}
	}
}

// Every route and status carries the same headers (checked in do for
// every response of the suite), and each its own content type.
func TestHeadersOnEveryResponse(t *testing.T) {
	p := newPage(t)
	for _, c := range []struct {
		method, path string
		edit         func(*http.Request)
		status       int
		typ          string
	}{
		{"GET", "/", nil, 200, typeHTML},
		{"GET", "/static/app.css", nil, 200, typeCSS},
		{"GET", "/nope", nil, 404, typeJSON},
		{"POST", "/", nil, 405, typeJSON},
		{"GET", "/", func(r *http.Request) { r.Host = "evil.example" }, 421, typeText},
		{"GET", "/", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403, typeJSON},
		{"POST", "/api/ping", nil, 200, typeJSON},
		{"POST", "/api/ping", func(r *http.Request) { r.Header.Del(TokenHeader) }, 401, typeJSON},
		{"POST", "/api/ping", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, typeJSON},
		{"POST", "/api/stop", nil, 404, typeJSON},
		{"POST", "/api/ping", func(r *http.Request) { r.URL.RawQuery = "a" }, 400, typeJSON},
		{"OPTIONS", "/api/ping", nil, 405, typeJSON},
		{"POST", "/api/proof", nil, 400, typeJSON},
	} {
		r := p.do(p.req(c.method, c.path, `{}`, c.edit))
		if r.status != c.status || r.header.Get("Content-Type") != c.typ {
			t.Errorf("%s %s: %d %q, want %d %q", c.method, c.path, r.status, r.header.Get("Content-Type"), c.status, c.typ)
		}
	}
	// The CSP holds what keeps the page unframed and its script its own.
	csp := ""
	for _, kv := range Headers {
		if kv[0] == "Content-Security-Policy" {
			csp = kv[1]
		}
	}
	for _, d := range []string{"frame-ancestors 'none'", "default-src 'none'", "script-src 'self'", "form-action 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("the CSP lacks %s", d)
		}
	}
	if strings.Contains(csp, "unsafe") || strings.Contains(csp, "*") {
		t.Errorf("the CSP: %s", csp)
	}
}

// Refusals are counted in the log, never with what a header held.
func TestRefusalsLoggedWithoutValues(t *testing.T) {
	p := newPage(t)
	secret := strings.Repeat("S", 43)
	for range 5 {
		p.do(p.req("POST", "/api/ping", `{}`, func(r *http.Request) {
			r.Header.Set(TokenHeader, secret)
		}))
		p.do(p.req("POST", "/api/ping", `{}`, func(r *http.Request) { r.Header.Set("Origin", "http://marker-origin.example") }))
		p.do(p.req("GET", "/", "", func(r *http.Request) { r.Host = "marker-host.example" }))
	}
	out := p.log.String()
	for _, s := range []string{secret, "marker-origin", "marker-host", testToken} {
		if strings.Contains(out, s) {
			t.Errorf("the log holds %q: %s", s, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("%d lines in a minute: %s", n, out)
	}
	p.clock.add(time.Minute)
	p.do(p.req("GET", "/", "", func(r *http.Request) { r.Host = "marker-host.example" }))
	out = p.log.String()
	if n := strings.Count(out, "\n"); n != 2 || !strings.Contains(out, "token 4") || !strings.Contains(out, "origin 5") || !strings.Contains(out, "host 6") {
		t.Errorf("after a minute: %s", out)
	}
}

// A code is traded for the token at /api/redeem, which takes no token,
// once; every code that opens nothing gets the one same answer.
func TestRedeem(t *testing.T) {
	p := newPage(t)
	code := strings.Repeat("c", 42) + "_"
	p.addCode(code)
	noToken := func(r *http.Request) { r.Header.Del(TokenHeader) }
	r := p.do(p.req("POST", "/api/redeem", `{"code":"`+code+`"}`, noToken))
	if r.status != 200 || r.body != `{"result":{"token":"`+testToken+`"}}`+"\n" || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("redeem: %d %q %v", r.status, r.body, r.header)
	}
	if len(p.d.called()) != 0 {
		t.Error("a redeem was dispatched")
	}
	if p.activities() != 1 {
		t.Errorf("activity %d", p.activities())
	}
	// Used, wrong, malformed, or no code: one answer for all.
	var first *resp
	other := strings.Repeat("d", 43)
	p.addCode(other)
	for _, body := range []string{`{"code":"` + code + `"}`, `{"code":"` + strings.Repeat("e", 43) + `"}`, `{"code":"` + code[:42] + `"}`,
		`{"code":"` + code + `a"}`, `{"code":"` + code[:42] + `="}`, `{}`, `{"code":1}`, `{"code":"` + other + `","x":1}`, `{"token":"` + testToken + `"}`} {
		p.clock.add(time.Second)
		r := p.do(p.req("POST", "/api/redeem", body, noToken))
		p.refused(body, r, 401, KindBadCode)
		if strings.Contains(r.body, testToken) || !strings.Contains(r.body, "caboose ui") {
			t.Errorf("%s: %q", body, r.body)
		}
		if first == nil {
			first = &r
		} else if r.body != first.body {
			t.Errorf("%s: %q, not %q", body, r.body, first.body)
		}
	}
	// The daemon was asked only for codes of the right form; the one sent
	// with an extra field was not tried, so it still opens the page.
	for _, c := range p.redeemCalls() {
		if !daemonproto.ValidCode(c) {
			t.Errorf("redeem asked for %q", c)
		}
	}
	p.clock.add(time.Second)
	if r := p.do(p.req("POST", "/api/redeem", `{"code":"`+other+`"}`, noToken)); r.status != 200 {
		t.Errorf("a good code after a bad body: %d %s", r.status, r.body)
	}
	// No token at the daemon: nothing opens, the same answer.
	p.clock.add(time.Second)
	p.addCode(code)
	p.setToken("")
	r = p.do(p.req("POST", "/api/redeem", `{"code":"`+code+`"}`, noToken))
	p.refused("no token", r, 401, KindBadCode)
	if r.body != first.body {
		t.Errorf("no token: %q", r.body)
	}
	if r := p.do(p.req("GET", "/api/redeem", "", nil)); r.status != 405 {
		t.Errorf("GET redeem: %d", r.status)
	}
}

// The redeem passes every check the proof does: never a token to another
// origin or site, or to a request that is not the page's JSON.
func TestRedeemChecked(t *testing.T) {
	p := newPage(t)
	code := strings.Repeat("c", 43)
	p.addCode(code)
	body := `{"code":"` + code + `"}`
	for name, edit := range map[string]func(*http.Request){
		"origin":        func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"no origin":     func(r *http.Request) { r.Header.Del("Origin") },
		"other port":    func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:"+strconv.Itoa(p.port+1)) },
		"fetch site":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same site":     func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"no fetch site": func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"fetch dest":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "iframe") },
		"content type":  func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"form":          func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		"host":          func(r *http.Request) { r.Host = "evil.example:" + strconv.Itoa(p.port) },
		"localhost":     func(r *http.Request) { r.Host = "localhost:" + strconv.Itoa(p.port) },
		"a query":       func(r *http.Request) { r.URL.RawQuery = "x=1" },
		"over 64 KiB": func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"x":"` + strings.Repeat("a", MaxBody) + `"}`))
			r.ContentLength = -1
		},
		"not one object":  func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(body + body)); r.ContentLength = -1 },
		"an escaped path": func(r *http.Request) { r.URL.Path = "/api/redeem"; r.URL.RawPath = "/api/re%64eem" },
	} {
		r := p.do(p.req("POST", "/api/redeem", body, edit))
		if r.status == 200 || strings.Contains(r.body, testToken) {
			t.Errorf("%s: %d %q", name, r.status, r.body)
		}
	}
	if len(p.redeemCalls()) != 0 {
		t.Errorf("a refused request asked the daemon: %v", p.redeemCalls())
	}
	if r := p.do(p.req("POST", "/api/redeem", body, nil)); r.status != 200 {
		t.Errorf("the code after the refusals: %d %s", r.status, r.body)
	}
}

// Redeems have a limit of their own, low, apart from the API's and the
// proofs'.
func TestRedeemRateLimited(t *testing.T) {
	p := newPage(t)
	bad := `{"code":"` + strings.Repeat("z", 43) + `"}`
	for i := range redeemBurst {
		if r := p.do(p.req("POST", "/api/redeem", bad, nil)); r.status != 401 {
			t.Fatalf("redeem %d: %d", i, r.status)
		}
	}
	code := strings.Repeat("c", 43)
	p.addCode(code)
	if r := p.do(p.req("POST", "/api/redeem", `{"code":"`+code+`"}`, nil)); r.status != 429 || strings.Contains(r.body, testToken) {
		t.Fatalf("past the burst: %d %s", r.status, r.body)
	}
	// The API and the proofs are not held up by it.
	if r := p.do(p.req("POST", "/api/ping", `{}`, nil)); r.status != 200 {
		t.Errorf("ping: %d", r.status)
	}
	if r := p.do(p.req("POST", "/api/proof", `{"nonce":"`+strings.Repeat("n", 43)+`"}`, nil)); r.status != 200 {
		t.Errorf("proof: %d", r.status)
	}
	p.clock.add(time.Second)
	if r := p.do(p.req("POST", "/api/redeem", `{"code":"`+code+`"}`, nil)); r.status != 200 {
		t.Errorf("a second later: %d", r.status)
	}
	if r := p.do(p.req("POST", "/api/redeem", bad, nil)); r.status != 429 {
		t.Errorf("past the rate: %d", r.status)
	}
	if redeemRate != 1 || redeemBurst != 5 {
		t.Errorf("redeem limit %d/%d", redeemRate, redeemBurst)
	}
}

// A handler whose origin is not a loopback http:// one refuses
// everything.
func TestHandlerBadOrigin(t *testing.T) {
	for _, o := range []string{"", "http://0.0.0.0:7171", "http://localhost:7171", "https://127.0.0.1:7171", "http://127.0.0.1:0",
		"http://127.0.0.1", "http://[::1]:7171", "http://192.168.1.2:7171", "http://127.0.0.1:7171/", "http://127.000.0.1:7171"} {
		h := Handler(Options{Origin: o, Token: func() string { return testToken }, Dispatch: &fakeDispatch{}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = strings.TrimPrefix(o, "http://")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 500 {
			t.Errorf("%q: %d", o, w.Code)
		}
	}
}

func TestCheckAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1:0", "127.0.0.1:7171", "127.0.0.1:7172", "127.1.2.3:7171"} {
		if err := CheckAddr(netip.MustParseAddrPort(a)); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"0.0.0.0:0", "0.0.0.0:7171", "[::1]:0", "[::]:7171", "[::]:0", "192.168.1.1:0", "10.0.0.1:7171",
		"[::ffff:127.0.0.1]:7171", "[fe80::1]:0", "169.254.1.1:0"} {
		if err := CheckAddr(netip.MustParseAddrPort(a)); err == nil {
			t.Errorf("%s taken", a)
		}
	}
	// Listen refuses them too; port 0 only, so that a regression binds
	// no real port.
	for _, a := range []string{"0.0.0.0:0", "[::1]:0", "[::]:0"} {
		if ln, err := Listen(netip.MustParseAddrPort(a)); err == nil {
			ln.Close()
			t.Errorf("%s listened on", a)
		}
	}
	if CheckAddr(netip.AddrPort{}) == nil {
		t.Error("no address taken")
	}
}

func TestListenPortInUse(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	a := held.Addr().(*net.TCPAddr).AddrPort()
	ln, err := Listen(a)
	if err == nil {
		ln.Close()
		t.Fatal("listened on a port in use")
	}
	if !strings.Contains(err.Error(), "in use") || !strings.Contains(err.Error(), a.String()) {
		t.Errorf("error: %v", err)
	}
}

// At most MaxConns connections are open at once: one more is closed as
// it is taken, and a place frees once one closes.
func TestConnectionLimit(t *testing.T) {
	p := newPage(t)
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for range MaxConns {
		c, err := net.Dial("tcp4", p.host)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	// Each is taken: a request on the last answers.
	last := conns[len(conns)-1]
	io.WriteString(last, "GET / HTTP/1.1\r\nHost: "+p.host+"\r\n\r\n")
	last.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(last), nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("the 64th: %v", err)
	}
	res.Body.Close()
	extra, err := net.Dial("tcp4", p.host)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := extra.Read(make([]byte, 1)); n != 0 || err == nil || isTimeout(err) {
		t.Fatalf("the 65th was not closed: %d %v", n, err)
	}
	conns[0].Close()
	conns = conns[1:]
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := tryGet(p.host)
		if err == nil && r == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no place after one closed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func tryGet(host string) (int, error) {
	c, err := net.Dial("tcp4", host)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

func TestServerSettings(t *testing.T) {
	s := NewServer(http.NotFoundHandler(), nil)
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 10*time.Second || s.WriteTimeout != 30*time.Second ||
		s.IdleTimeout != 60*time.Second || s.MaxHeaderBytes != 16<<10 {
		t.Errorf("%+v", s)
	}
	if s.Protocols == nil || !s.Protocols.HTTP1() || s.Protocols.HTTP2() || s.Protocols.UnencryptedHTTP2() {
		t.Errorf("protocols %v", s.Protocols)
	}
	if s.TLSConfig != nil {
		t.Error("TLS")
	}
}

func TestErrorLogLimited(t *testing.T) {
	var buf syncBuf
	c := &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	lg := ErrorLog(log.New(&buf, "", 0), c.Now)
	for range 10 {
		lg.Print("http: something\x1b")
	}
	c.add(time.Second)
	lg.Print("http: again")
	out := buf.String()
	if n := strings.Count(out, "\n"); n != 2 || !strings.Contains(out, "9 more left out") || strings.Contains(out, "\x1b") {
		t.Errorf("%q", out)
	}
}
