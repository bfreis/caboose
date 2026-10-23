package daemon

import (
	"bytes"
	"encoding/json"
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

func tokenFile(h *harness) string { return filepath.Join(h.dir, TokenFile) }

func readTokenFile(t *testing.T, h *harness) string {
	t.Helper()
	b, err := os.ReadFile(tokenFile(h))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
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

// The token is made at the first start, 0600 in the 0700 directory, and
// is what web hands out; status says how the page stands, never the
// token.
func TestWebTokenMade(t *testing.T) {
	h := newHarness(t)
	lg := &syncLog{}
	s, _ := webDaemon(t, h, lg)
	fi, err := os.Lstat(tokenFile(h))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v", TokenFile, fi.Mode())
	}
	if di, _ := os.Lstat(h.dir); di.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v", di.Mode())
	}
	tok := readTokenFile(t, h)
	if !daemonproto.ValidToken(tok) {
		t.Fatalf("the file holds %q", tok)
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
	if !s.web.check(tok) {
		t.Error("the token refused")
	}
	for _, bad := range []string{"", tok[:42], tok + "x", "x" + tok[1:], strings.ToUpper(tok)} {
		if bad != tok && s.web.check(bad) {
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
	if !strings.Contains(lg.String(), "made the page's token") {
		t.Errorf("log: %s", lg)
	}
}

// The token survives a restart: the page's stored copy stays good.
func TestWebTokenSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	s, stop := webDaemon(t, h, &syncLog{})
	tok := s.web.current()
	stop()
	s, _ = webDaemon(t, h, &syncLog{})
	if s.web.current() != tok || readTokenFile(t, h) != tok {
		t.Error("a restart changed the token")
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
	nonce := strings.Repeat("A", 43)
	oldProof, _ := s.web.proof("http://127.0.0.1:0", nonce)
	r := webOf(t, c, true)
	tok := tokenOf(t, s, r)
	if tok == "" || tok == old {
		t.Fatalf("rotated to %q from %q", tok, old)
	}
	if s.web.check(old) {
		t.Error("the old token still taken")
	}
	if !s.web.check(tok) {
		t.Error("the new token refused")
	}
	if p, ok := s.web.proof("http://127.0.0.1:0", nonce); !ok || p == oldProof || p != daemonproto.WebProof(tok, "http://127.0.0.1:0", nonce) {
		t.Error("the proof did not move to the new token")
	}
	if readTokenFile(t, h) != tok {
		t.Error("the file does not hold the new token")
	}
	if fi, _ := os.Lstat(tokenFile(h)); fi.Mode().Perm() != 0o600 {
		t.Errorf("after rotation: %v", fi.Mode())
	}
	if tokenOf(t, s, webOf(t, c, false)) != tok {
		t.Error("web hands out another token")
	}
	// The new token is the one a restart reads.
	stop()
	s, _ = webDaemon(t, h, lg)
	if s.web.current() != tok {
		t.Error("a restart did not keep the rotated token")
	}
	if !strings.Contains(lg.String(), "replaced with a new one") {
		t.Errorf("log: %s", lg)
	}
	// No temp file is left beside it.
	ents, _ := os.ReadDir(h.dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "."+TokenFile) {
			t.Errorf("left %s", e.Name())
		}
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

// A file that cannot be trusted is replaced by a new token, and the log
// says so without either token.
func TestWebTokenUntrustedFileReplaced(t *testing.T) {
	planted := "P" + strings.Repeat("q", 42)
	cases := map[string]func(t *testing.T, p string){
		"mode 0644": func(t *testing.T, p string) { write(t, p, planted+"\n", 0o644) },
		"mode 0640": func(t *testing.T, p string) { write(t, p, planted+"\n", 0o640) },
		"mode 0400": func(t *testing.T, p string) { write(t, p, planted, 0o400) },
		"short":     func(t *testing.T, p string) { write(t, p, planted[:42]+"\n", 0o600) },
		"long":      func(t *testing.T, p string) { write(t, p, planted+"q\n", 0o600) },
		"two lines": func(t *testing.T, p string) { write(t, p, planted+"\n\n", 0o600) },
		"bad char":  func(t *testing.T, p string) { write(t, p, planted[:42]+"=\n", 0o600) },
		"spaces":    func(t *testing.T, p string) { write(t, p, " "+planted+"\n", 0o600) },
		"crlf":      func(t *testing.T, p string) { write(t, p, planted+"\r\n", 0o600) },
		"empty":     func(t *testing.T, p string) { write(t, p, "", 0o600) },
		"symlink": func(t *testing.T, p string) {
			target := filepath.Join(filepath.Dir(p), "elsewhere")
			write(t, target, planted+"\n", 0o600)
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, p string) {
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Skip(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			inTempDir(t, h.home)
			if err := os.MkdirAll(h.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			plant(t, tokenFile(h))
			lg := &syncLog{}
			s, _ := webDaemon(t, h, lg)
			tok := s.web.current()
			if !daemonproto.ValidToken(tok) || tok == planted {
				t.Fatalf("token %q", tok)
			}
			fi, err := os.Lstat(tokenFile(h))
			if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
				t.Fatalf("the file: %v %v", fi, err)
			}
			if readTokenFile(t, h) != tok {
				t.Error("the file does not hold the new token")
			}
			if s.web.check(planted) {
				t.Error("the planted token taken")
			}
			out := lg.String()
			if !strings.Contains(out, "replaced it with a new token") || strings.Contains(out, tok) || strings.Contains(out, planted) {
				t.Errorf("log: %s", out)
			}
			if name == "symlink" {
				// The link was replaced, never followed or written through.
				b, _ := os.ReadFile(filepath.Join(h.dir, "elsewhere"))
				if string(b) != planted+"\n" {
					t.Errorf("the symlink's target was written: %q", b)
				}
			}
		})
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

// A token that cannot be had (web.token is a directory, which no rename
// replaces) leaves the daemon running with no token: nothing is let in,
// web hands out none, and status says why.
func TestWebTokenUnavailable(t *testing.T) {
	h := newHarness(t)
	inTempDir(t, h.home)
	if err := os.MkdirAll(tokenFile(h), 0o700); err != nil {
		t.Fatal(err)
	}
	lg := &syncLog{}
	s, _ := webDaemon(t, h, lg)
	if s.web.current() != "" {
		t.Fatal("a token")
	}
	for _, got := range []string{"", strings.Repeat("A", 43)} {
		if s.web.check(got) {
			t.Errorf("%q taken with no token", got)
		}
	}
	if _, ok := s.web.proof("http://127.0.0.1:0", strings.Repeat("A", 43)); ok {
		t.Error("a proof with no token")
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
	inTempDir(t, h.home)
	if err := os.MkdirAll(tokenFile(h), 0o700); err != nil {
		t.Fatal(err)
	}
	s, _ := webDaemon(t, h, &syncLog{})
	time.Sleep(50 * time.Millisecond)
	if s.listens() != 0 || s.web.state().Listening {
		t.Fatal("served with no token")
	}
	if err := os.Remove(tokenFile(h)); err != nil {
		t.Fatal(err)
	}
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	r := webOf(t, c, true)
	tok := readTokenFile(t, h)
	if r.CodeURL == "" || tokenOf(t, s, r) != tok || strings.Contains(r.Error, "token") {
		t.Errorf("rotate: %+v", r)
	}
	if !s.web.check(tok) {
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

// A rotation that cannot write leaves the old token, says so without a
// token, and logs none.
func TestWebRotationFailureKeepsTheToken(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	h := newHarness(t)
	lg := &syncLog{}
	s, _ := webDaemon(t, h, lg)
	inTempDir(t, h.dir)
	// The hello round trip waits until the daemon has written its state,
	// which the read-only directory below would otherwise refuse.
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	old := s.web.current()
	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) })
	_, err := c.Web(t.Context(), daemonproto.WebParams{Rotate: true, Code: true})
	if daemonproto.Kind(err) != daemonproto.KindInternal || !strings.Contains(err.Error(), "old one stays") {
		t.Fatalf("rotate: %v", err)
	}
	if err := os.Chmod(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if s.web.current() != old || !s.web.check(old) || readTokenFile(t, h) != old {
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
	dir := t.TempDir()
	w := newWebState(dir, WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171"), Now: clock}, testLogger(t))
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
	w := newWebState(t.TempDir(), WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171"), Now: clock}, testLogger(t))
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
	w := newWebState(t.TempDir(), WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171")}, testLogger(t))
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
	if _, err := os.Lstat(filepath.Join(dir, TokenFile)); !os.IsNotExist(err) {
		t.Errorf("%s: %v", TokenFile, err)
	}
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	r := webOf(t, c, false)
	if r.Listening || r.URL != "" || r.CodeURL != "" || r.Error == "" {
		t.Errorf("web: %+v", r)
	}
	if _, err := c.Web(t.Context(), daemonproto.WebParams{Rotate: true}); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("rotate: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, TokenFile)); !os.IsNotExist(err) {
		t.Errorf("after rotate, %s: %v", TokenFile, err)
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

// The proof is the token's MAC of the origin and the nonce, and refuses a
// nonce of the wrong form.
func TestWebProof(t *testing.T) {
	h := newHarness(t)
	s, _ := webDaemon(t, h, &syncLog{})
	tok := s.web.current()
	n1, n2 := strings.Repeat("a", 43), strings.Repeat("b", 43)
	p1, ok := s.web.proof("http://127.0.0.1:7171", n1)
	if !ok || p1 != daemonproto.WebProof(tok, "http://127.0.0.1:7171", n1) {
		t.Fatal("proof")
	}
	if p2, _ := s.web.proof("http://127.0.0.1:7171", n2); p2 == p1 {
		t.Error("the proof does not bind the nonce")
	}
	if p3, _ := s.web.proof("http://127.0.0.1:7172", n1); p3 == p1 {
		t.Error("the proof does not bind the origin")
	}
	for _, n := range []string{"", n1[:42], n1 + "a", n1[:42] + "=", n1[:42] + "/"} {
		if _, ok := s.web.proof("http://127.0.0.1:7171", n); ok {
			t.Errorf("nonce %q taken", n)
		}
	}
}

// Every web state set changes since only when the state changes.
func TestWebServedState(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	w := newWebState(t.TempDir(), WebOptions{Addr: netip.MustParseAddrPort("127.0.0.1:7171"), Now: clock}, testLogger(t))
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

// A file of another user's is not taken, however good its token.
func TestReadTokenRefusesAnotherOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), TokenFile)
	tok := strings.Repeat("Q", 43)
	write(t, p, tok+"\n", 0o600)
	if got, why := readToken(p, ownUID()); got != tok || why != "" {
		t.Fatalf("own file: %q %q", got, why)
	}
	if got, why := readToken(p, ownUID()+1); got != "" || !strings.Contains(why, "another user") {
		t.Errorf("another's file: %q %q", got, why)
	}
}
