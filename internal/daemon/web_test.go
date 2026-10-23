package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

var webAddr = netip.MustParseAddrPort("127.0.0.1:0")

// syncLog is a daemon log the test reads back.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// inTempDir fails the test unless dir is under the system's temp dir:
// what a test chmods or replaces is its own.
func inTempDir(t *testing.T, dir string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(tmp, d)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		t.Fatalf("%s is not under %s", dir, tmp)
	}
}

// webDaemon runs a daemon serving the web state in the harness's dir,
// logging to lg, until the returned stop.
func webDaemon(t *testing.T, h *harness, lg *syncLog) (*server, func()) {
	t.Helper()
	inTempDir(t, h.home)
	got := make(chan *server, 1)
	stop := runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: lg,
		Web: &WebOptions{Addr: webAddr}, onServer: func(s *server) { got <- s }})
	return <-got, stop
}

// oldTokenFile is where a daemon once kept the page's token, which none
// reads or writes now.
func oldTokenFile(h *harness) string { return filepath.Join(h.dir, "web.token") }

// failTokens makes newToken fail until the returned undo.
func failTokens(t *testing.T) (undo func()) {
	t.Helper()
	was := newToken
	newToken = func() (string, error) { return "", errors.New("no randomness") }
	t.Cleanup(func() { newToken = was })
	return func() { newToken = was }
}

// webOf asks web for a code, rotating first when rotate is set.
func webOf(t *testing.T, c *daemonproto.Client, rotate bool) *daemonproto.WebResult {
	t.Helper()
	r, err := c.Web(t.Context(), daemonproto.WebParams{Rotate: rotate, Code: true})
	if err != nil {
		t.Fatalf("web (rotate %v): %v", rotate, err)
	}
	return r
}

// tokenOf is the token r's code opens, redeemed as the page would.
func tokenOf(t *testing.T, s *server, r *daemonproto.WebResult) string {
	t.Helper()
	tok, ok := s.web.redeem(r.Code())
	if !ok {
		t.Fatalf("the code of %+v redeemed nothing", r)
	}
	return tok
}

// The token is made at the start, kept in memory alone, and is what web
// hands out; status says how the page stands, never the token.
func TestWebTokenMade(t *testing.T) {
	h := newHarness(t)
	lg := &syncLog{}
	s, _ := webDaemon(t, h, lg)
	tok := s.web.current()
	if !daemonproto.ValidToken(tok) {
		t.Fatalf("the token is %q", tok)
	}
	ents, _ := os.ReadDir(h.dir)
	for _, e := range ents {
		if b, err := os.ReadFile(filepath.Join(h.dir, e.Name())); err == nil && bytes.Contains(b, []byte(tok)) {
			t.Errorf("%s holds the token", e.Name())
		}
	}
	// Served on the address it bound, which is what the URLs name.
	bound := servedAddr(t, s)
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	r := webOf(t, c, false)
	if r.Code() == "" || r.URL != "http://"+bound+"/" || r.CodeURL != r.URL+"#code="+r.Code() || r.Addr != bound ||
		!strings.HasPrefix(bound, "127.0.0.1:") || bound == "127.0.0.1:0" {
		t.Errorf("web: %+v", r)
	}
	if !r.Listening || r.Error != "" {
		t.Errorf("not served: %+v", r)
	}
	if strings.Contains(r.CodeURL, tok) {
		t.Error("the address holds the token")
	}
	if tokenOf(t, s, r) != tok {
		t.Error("the code opens another token")
	}
	// Asked without a code, web makes none.
	if r, err := c.Web(t.Context(), daemonproto.WebParams{}); err != nil || r.CodeURL != "" || !r.Listening {
		t.Errorf("web with no code: %+v %v", r, err)
	}
	if !daemonproto.TokenEqual(tok, s.web.current()) {
		t.Error("the token refused")
	}
	for _, bad := range []string{"", tok[:42], tok + "x", "x" + tok[1:], strings.ToUpper(tok)} {
		if bad != tok && daemonproto.TokenEqual(bad, s.web.current()) {
			t.Errorf("%q taken", bad)
		}
	}
	st, err := c.Status(t.Context())
	if err != nil || st.Daemon.Web == nil || !st.Daemon.Web.Listening || st.Daemon.Web.Addr != bound {
		t.Fatalf("status: %+v %v", st, err)
	}
	b, _ := json.Marshal(st)
	if bytes.Contains(b, []byte(tok)) {
		t.Error("status carries the token")
	}
	wb, e := s.callWeb(t.Context(), daemonproto.MethodStatus, nil)
	if e != nil || bytes.Contains(wb, []byte(tok)) || !bytes.Contains(wb, []byte(`"web":{`)) {
		t.Errorf("status from the web: %s %v", wb, e)
	}
	if !strings.Contains(lg.String(), "made a new token for the page") {
		t.Errorf("log: %s", lg)
	}
}

// Every start makes a new token: one a page kept from before lets nothing
// in once the daemon is back.
func TestWebTokenNewAtEveryStart(t *testing.T) {
	h := newHarness(t)
	s, stop := webDaemon(t, h, &syncLog{})
	tok := s.web.current()
	stop()
	s, _ = webDaemon(t, h, &syncLog{})
	if now := s.web.current(); !daemonproto.ValidToken(now) || now == tok {
		t.Fatalf("after a restart the token is %q, was %q", now, tok)
	}
	if daemonproto.TokenEqual(tok, s.web.current()) {
		t.Error("the token from before the restart is taken")
	}
}

// Rotation writes a new token and takes it at once: the old one lets
// nothing in from the moment web answers, and its proofs no longer match.
func TestWebTokenRotation(t *testing.T) {
	h := newHarness(t)
	lg := &syncLog{}
	s, stop := webDaemon(t, h, lg)
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	old := tokenOf(t, s, webOf(t, c, false))
	r := webOf(t, c, true)
	tok := tokenOf(t, s, r)
	if tok == "" || tok == old {
		t.Fatalf("rotated to %q from %q", tok, old)
	}
	if daemonproto.TokenEqual(old, s.web.current()) {
		t.Error("the old token still taken")
	}
	if !daemonproto.TokenEqual(tok, s.web.current()) {
		t.Error("the new token refused")
	}
	if tokenOf(t, s, webOf(t, c, false)) != tok {
		t.Error("web hands out another token")
	}
	// The app rotates too; and a restart keeps no rotated token either.
	a := dialAs(t, h.dir, daemonproto.ClientApp, 0)
	tok = tokenOf(t, s, webOf(t, a, true))
	stop()
	s, _ = webDaemon(t, h, lg)
	if s.web.current() == tok {
		t.Error("a restart kept the rotated token")
	}
	if !strings.Contains(lg.String(), "replaced with a new one") {
		t.Errorf("log: %s", lg)
	}
}

// web is the socket's alone: the page cannot ask for the token, or
// rotate it.
func TestWebMethodIsNotThePages(t *testing.T) {
	h := newHarness(t)
	s, _ := webDaemon(t, h, &syncLog{})
	tok := s.web.current()
	for _, p := range []string{``, `{}`, `{"rotate":true}`, `{"code":true}`} {
		b, e := s.callWeb(t.Context(), daemonproto.MethodWeb, json.RawMessage(p))
		if webKind(e) != daemonproto.KindNotAllowed || b != nil {
			t.Errorf("web %s from the page: %s %v", p, b, e)
		}
	}
	if s.web.current() != tok {
		t.Error("the page rotated the token")
	}
}

// A token file left in the directory is never read, nor written: the
// token is the daemon's memory's alone.
func TestWebTokenNeverReadFromDisk(t *testing.T) {
	planted := "P" + strings.Repeat("q", 42)
	h := newHarness(t)
	inTempDir(t, h.home)
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, oldTokenFile(h), planted+"\n", 0o600)
	s, _ := webDaemon(t, h, &syncLog{})
	if daemonproto.TokenEqual(planted, s.web.current()) {
		t.Fatal("the planted token taken")
	}
	webOf(t, dialAs(t, h.dir, daemonproto.ClientCLI, 0), true)
	if b, _ := os.ReadFile(oldTokenFile(h)); string(b) != planted+"\n" {
		t.Errorf("the file was written: %q", b)
	}
}

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// A token that cannot be made leaves the daemon running with no token:
// nothing is let in, web hands out none, and status says why.
func TestWebTokenUnavailable(t *testing.T) {
	h := newHarness(t)
	failTokens(t)
	lg := &syncLog{}
	s, _ := webDaemon(t, h, lg)
	if s.web.current() != "" {
		t.Fatal("a token")
	}
	for _, got := range []string{"", strings.Repeat("A", 43)} {
		if daemonproto.TokenEqual(got, s.web.current()) {
			t.Errorf("%q taken with no token", got)
		}
	}
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	r := webOf(t, c, false)
	if r.CodeURL != "" || !strings.Contains(r.Error, "token") {
		t.Errorf("web: %+v", r)
	}
	st, err := c.Status(t.Context())
	if err != nil || st.Daemon.Web == nil || !strings.Contains(st.Daemon.Web.Error, "token") {
		t.Errorf("status: %+v %v", st, err)
	}
}

// A rotation once the token can be had again clears the start's error and
// serves the page, with no restart of the daemon.
func TestWebRotationAfterNoTokenServes(t *testing.T) {
	h := newHarness(t)
	undo := failTokens(t)
	s, _ := webDaemon(t, h, &syncLog{})
	time.Sleep(50 * time.Millisecond)
	if s.listens() != 0 || s.web.state().Listening {
		t.Fatal("served with no token")
	}
	undo()
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	r := webOf(t, c, true)
	tok := s.web.current()
	if r.CodeURL == "" || tokenOf(t, s, r) != tok || strings.Contains(r.Error, "token") {
		t.Errorf("rotate: %+v", r)
	}
	if !daemonproto.TokenEqual(tok, s.web.current()) {
		t.Error("the new token is not taken")
	}
	waitFor(t, 5*time.Second, "the page is served", func() bool { return s.web.state().Listening })
	// Another rotation starts no second server.
	webOf(t, c, true)
	time.Sleep(50 * time.Millisecond)
	if n := s.listens(); n != 1 {
		t.Errorf("listened %d times", n)
	}
	st, err := c.Status(t.Context())
	if err != nil || st.Daemon.Web == nil || !st.Daemon.Web.Listening || st.Daemon.Web.Error != "" {
		t.Errorf("status: %+v %v", st, err)
	}
}

// A rotation that cannot make a token leaves the old one, says so without
// a token, and logs none.
func TestWebRotationFailureKeepsTheToken(t *testing.T) {
	h := newHarness(t)
	lg := &syncLog{}
	s, _ := webDaemon(t, h, lg)
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	old := s.web.current()
	undo := failTokens(t)
	_, err := c.Web(t.Context(), daemonproto.WebParams{Rotate: true, Code: true})
	undo()
	if daemonproto.Kind(err) != daemonproto.KindInternal || !strings.Contains(err.Error(), "old one stays") {
		t.Fatalf("rotate: %v", err)
	}
	if !daemonproto.TokenEqual(old, s.web.current()) {
		t.Error("the old token did not stay")
	}
	if strings.Contains(lg.String(), old) || strings.Contains(err.Error(), old) {
		t.Error("the token in the log or the error")
	}
}

// A code opens the page once, within its minute: redeemed it is gone,
// expired it opens nothing, and a code of the wrong form, one never made
// or one from before a rotation opens nothing either.
func TestWebCodes(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	w := newWebState(WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171"), Now: clock}, testLogger(t))
	tok := w.current()
	mint := func() string {
		t.Helper()
		r, err := w.result(true)
		if err != nil || r.Code() == "" || strings.Contains(r.CodeURL, tok) {
			t.Fatalf("result: %+v %v", r, err)
		}
		return r.Code()
	}
	if r, _ := w.result(false); r.CodeURL != "" || len(w.codes) != 0 {
		t.Errorf("a code not asked for: %+v", r)
	}

	// Once.
	c := mint()
	if got, ok := w.redeem(c); !ok || got != tok {
		t.Fatalf("redeem: %q %v", got, ok)
	}
	if _, ok := w.redeem(c); ok {
		t.Error("a code redeemed twice")
	}

	// Within the minute: 59 s is in time, 61 s is not, and the late try
	// spends the code too.
	c1, c2 := mint(), mint()
	now = now.Add(59 * time.Second)
	if _, ok := w.redeem(c1); !ok {
		t.Error("refused at 59 s")
	}
	now = now.Add(2 * time.Second)
	if _, ok := w.redeem(c2); ok {
		t.Error("taken at 61 s")
	}
	if _, ok := w.codes[c2]; ok {
		t.Error("an expired code kept after its try")
	}

	// Wrong and malformed: refused, and the codes that are good stay.
	c3 := mint()
	for _, bad := range []string{"", c3[:42], c3 + "A", "x" + c3[1:], strings.ToUpper(c3), c3[:42] + "=", tok} {
		if bad == c3 {
			continue
		}
		if _, ok := w.redeem(bad); ok {
			t.Errorf("%q redeemed", bad)
		}
	}
	// A rotation clears every code.
	if _, err := w.rotate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.redeem(c3); ok {
		t.Error("a code from before the rotation redeemed")
	}
	if got, ok := w.redeem(mint()); !ok || got != w.current() || got == tok {
		t.Errorf("after rotation: %q %v", got, ok)
	}
}

// At most maxCodes are outstanding: the next drops the oldest, and the
// expired go first.
func TestWebCodesCapped(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	w := newWebState(WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171"), Now: clock}, testLogger(t))
	var codes []string
	for range maxCodes + 3 {
		r, err := w.result(true)
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, r.Code())
		now = now.Add(time.Second)
	}
	if len(w.codes) != maxCodes {
		t.Fatalf("%d outstanding", len(w.codes))
	}
	for i, c := range codes {
		_, ok := w.redeem(c)
		if want := i >= 3; ok != want {
			t.Errorf("code %d: redeemed %v", i, ok)
		}
	}
	for range maxCodes {
		if _, err := w.result(true); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(daemonproto.CodeTTL)
	if _, err := w.result(true); err != nil {
		t.Fatal(err)
	}
	if len(w.codes) != 1 {
		t.Errorf("expired codes kept: %d", len(w.codes))
	}
}

// With no token, a code is never made, and none redeems.
func TestWebNoTokenNoCode(t *testing.T) {
	w := newWebState(WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171")}, testLogger(t))
	r, err := w.result(true)
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.token = ""
	w.mu.Unlock()
	if r, _ := w.result(true); r.CodeURL != "" {
		t.Errorf("a code with no token: %+v", r)
	}
	if _, ok := w.redeem(r.Code()); ok {
		t.Error("a code redeemed with no token")
	}
}

// Neither the token nor a code ever appears in the daemon's log, whatever
// it did.
func TestWebTokenNeverLogged(t *testing.T) {
	h := newHarness(t)
	lg := &syncLog{}
	s, stop := webDaemon(t, h, lg)
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	toks := []string{s.web.current()}
	for range 3 {
		r := webOf(t, c, true)
		toks = append(toks, r.Code(), tokenOf(t, s, r))
	}
	_, _ = s.callWeb(t.Context(), daemonproto.MethodWeb, nil)
	_, _ = c.Status(t.Context())
	stop()
	webDaemon(t, h, lg)
	for _, tok := range toks {
		if strings.Contains(lg.String(), tok) {
			t.Errorf("the log holds a token: %s", lg)
		}
	}
}

// A daemon not asked for the web page makes no token, and says it serves
// no page.
func TestNoWebNoToken(t *testing.T) {
	dir, s := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}})
	if s.web != nil {
		t.Fatal("web state with no Options.Web")
	}
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	r := webOf(t, c, false)
	if r.Listening || r.URL != "" || r.CodeURL != "" || r.Error == "" {
		t.Errorf("web: %+v", r)
	}
	if _, err := c.Web(t.Context(), daemonproto.WebParams{Rotate: true}); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("rotate: %v", err)
	}
	if st, err := c.Status(t.Context()); err != nil || st.Daemon.Web != nil {
		t.Errorf("status: %+v %v", st, err)
	}
}

// The page is served on an IPv4 loopback address only: any other is
// refused, and the daemon then hands out no address to open.
func TestWebAddrLoopbackOnly(t *testing.T) {
	for _, a := range []string{"127.0.0.1:0", "127.0.0.1:7171", "127.0.0.1:7172", "127.1.2.3:7171"} {
		if err := checkWebAddr(netip.MustParseAddrPort(a)); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"0.0.0.0:0", "0.0.0.0:7171", "[::1]:0", "[::]:7171", "192.168.1.1:0", "10.0.0.1:7171", "[::ffff:127.0.0.1]:7171"} {
		if err := checkWebAddr(netip.MustParseAddrPort(a)); err == nil {
			t.Errorf("%s taken", a)
		}
	}
	if checkWebAddr(netip.AddrPort{}) == nil {
		t.Error("no address taken")
	}
	h := newHarness(t)
	got := make(chan *server, 1)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: testLog{t},
		Web: &WebOptions{Addr: netip.MustParseAddrPort("0.0.0.0:0")}, onServer: func(s *server) { got <- s }})
	<-got
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	r := webOf(t, c, false)
	if r.URL != "" || r.CodeURL != "" || !strings.Contains(r.Error, "loopback") {
		t.Errorf("web: %+v", r)
	}
}

// Every web state set changes since only when the state changes.
func TestWebServedState(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	w := newWebState(WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171"), Now: clock}, testLogger(t))
	if st := w.state(); st.Listening || st.Error == "" || !st.Since.Equal(now) {
		t.Errorf("start: %+v", st)
	}
	now = now.Add(time.Minute)
	w.setServed(true, netip.AddrPort{}, nil)
	if st := w.state(); !st.Listening || st.Error != "" || !st.Since.Equal(now) {
		t.Errorf("served: %+v", st)
	}
	later := now
	now = now.Add(time.Minute)
	w.setServed(true, netip.AddrPort{}, nil)
	if st := w.state(); !st.Since.Equal(later) {
		t.Errorf("since moved with no change: %+v", st)
	}
	w.setServed(false, netip.AddrPort{}, syscall.EADDRINUSE)
	if st := w.state(); st.Listening || !strings.Contains(st.Error, "in use") || !st.Since.Equal(now) {
		t.Errorf("in use: %+v", st)
	}
}

// Nothing in the daemon compares the token itself: every check goes
// through daemonproto.TokenEqual, which is constant time.
func TestTokenComparedOnlyInConstantTime(t *testing.T) {
	fset := token.NewFileSet()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			f, err := parser.ParseFile(fset, n, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files[n] = f
		}
	}
	if files["web.go"] == nil {
		t.Fatal("web.go not found")
	}
	secret := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "tok" || e.Name == "token" || e.Name == "got"
		case *ast.SelectorExpr:
			return e.Sel.Name == "token"
		case *ast.CallExpr:
			if s, ok := e.Fun.(*ast.SelectorExpr); ok {
				return s.Sel.Name == "current"
			}
		}
		return false
	}
	{
		for name, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BinaryExpr:
					if n.Op != token.EQL && n.Op != token.NEQ {
						return true
					}
					// Against "" is whether there is one, not which.
					if lit, ok := n.Y.(*ast.BasicLit); ok && lit.Value == `""` {
						return true
					}
					if lit, ok := n.X.(*ast.BasicLit); ok && lit.Value == `""` {
						return true
					}
					if secret(n.X) || secret(n.Y) {
						t.Errorf("%s: %s compares a token with %s", name, fset.Position(n.Pos()), n.Op)
					}
				case *ast.CallExpr:
					if s, ok := n.Fun.(*ast.SelectorExpr); ok {
						switch s.Sel.Name {
						case "Equal", "EqualFold", "Compare", "HasPrefix", "ConstantTimeCompare":
							for _, a := range n.Args {
								if secret(a) {
									t.Errorf("%s: %s compares a token with %s", name, fset.Position(n.Pos()), s.Sel.Name)
								}
							}
						}
					}
				}
				return true
			})
		}
	}
}

func testLogger(t *testing.T) *log.Logger { return log.New(testLog{t}, "", 0) }
