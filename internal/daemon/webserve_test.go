package daemon

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/webui"
)

// webCall is one API call to a daemon's page, every check passing but
// the token, which is tok.
func webCall(t *testing.T, addr, tok, method, body string) (int, string) {
	t.Helper()
	r, err := http.NewRequest("POST", "http://"+addr+"/api/"+method, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "http://"+addr)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Content-Type", "application/json")
	if tok != "" {
		r.Header.Set(webui.TokenHeader, tok)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func servedAddr(t *testing.T, s *server) string {
	t.Helper()
	waitFor(t, 5*time.Second, "the page is served", func() bool { return s.web.state().Listening })
	return s.web.state().Addr
}

func (s *server) listens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.webListens
}

// The daemon serves its page on the loopback address it bound, calls
// the method table as the web kind, and nothing else.
func TestWebPageServed(t *testing.T) {
	h := newHarness(t)
	s, stop := webDaemon(t, h, &syncLog{})
	addr := servedAddr(t, s)
	if a := netip.MustParseAddrPort(addr); a.Addr() != netip.MustParseAddr("127.0.0.1") || a.Port() == 0 {
		t.Fatalf("served on %s", addr)
	}
	tok := s.web.current()

	res, err := http.Get("http://" + addr + "/")
	if err != nil || res.StatusCode != 200 || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("GET /: %v %v", res, err)
	}
	res.Body.Close()

	// status through the page is the socket's.
	code, body := webCall(t, addr, tok, "status", `{}`)
	var got struct {
		Result daemonproto.StatusResult `json:"result"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || got.Result.Daemon.Web == nil || !got.Result.Daemon.Web.Listening {
		t.Fatalf("status: %d %s", code, body)
	}
	if strings.Contains(body, tok) {
		t.Error("status carries the token")
	}
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	st, err := c.Status(t.Context())
	if err != nil || st.Daemon.PID != got.Result.Daemon.PID || st.Daemon.Web.Addr != addr {
		t.Errorf("socket status %+v %v", st, err)
	}

	// No token, no call; the methods the page may not call are not there.
	if code, _ := webCall(t, addr, "", "status", `{}`); code != 401 {
		t.Errorf("no token: %d", code)
	}
	for _, m := range []string{"web", "stop", "replace", "ensure_link", "writable", "subscribe"} {
		code, body := webCall(t, addr, tok, m, `{}`)
		if code != 404 || !strings.Contains(body, daemonproto.KindUnknownMethod) || strings.Contains(body, tok) {
			t.Errorf("%s: %d %s", m, code, body)
		}
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("the daemon is gone after the page's calls: %v", err)
	}

	// The proof is of the bound origin.
	nonce := strings.Repeat("q", 43)
	code, body = webCall(t, addr, "", "proof", `{"nonce":"`+nonce+`"}`)
	if want := daemonproto.WebProof(tok, "http://"+addr, nonce); code != 200 || !strings.Contains(body, `"mac":"`+want+`"`) {
		t.Errorf("proof: %d %s", code, body)
	}

	// A code from web opens the page once, through /api/redeem, which
	// takes no token: the code's answer is the token.
	cr := webOf(t, c, false)
	code, body = webCall(t, addr, "", "redeem", `{"code":"`+cr.Code()+`"}`)
	if code != 200 || body != `{"result":{"token":"`+tok+`"}}`+"\n" {
		t.Errorf("redeem: %d %s", code, body)
	}
	if code, body := webCall(t, addr, "", "redeem", `{"code":"`+cr.Code()+`"}`); code != 401 || strings.Contains(body, tok) {
		t.Errorf("redeem again: %d %s", code, body)
	}

	// A rotation takes effect on the page at once.
	nt := tokenOf(t, s, webOf(t, c, true))
	if code, _ := webCall(t, addr, tok, "ping", `{}`); code != 401 {
		t.Errorf("old token after rotation: %d", code)
	}
	if code, _ := webCall(t, addr, nt, "ping", `{}`); code != 200 {
		t.Errorf("new token: %d", code)
	}

	// Stopping the daemon stops the page, and frees the port.
	stop()
	if conn, err := net.DialTimeout("tcp4", addr, time.Second); err == nil {
		conn.Close()
		t.Error("the page still listens after the daemon stopped")
	}
	if st := s.web.state(); st.Listening {
		t.Errorf("listening after the stop: %+v", st)
	}
}

// startWeb starts at most one server: called again while one serves (a
// rotation does), it changes nothing; once the daemon stops it starts
// none, even with no server left to guard against.
func TestWebStartsOnce(t *testing.T) {
	h := newHarness(t)
	s, stop := webDaemon(t, h, &syncLog{})
	addr := servedAddr(t, s)
	done := func() chan struct{} {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.webDone
	}
	first, n := done(), s.listens()
	for range 3 {
		s.startWeb()
	}
	if done() != first || s.listens() != n || s.web.state().Addr != addr {
		t.Fatalf("a second start: done changed %v, listens %d -> %d", done() != first, n, s.listens())
	}
	stop()
	select {
	case <-first:
	default:
		t.Fatal("the page's server still runs after the stop")
	}
	s.mu.Lock()
	stopping := s.stopping
	s.webDone = nil // as if none had run: only stopping guards now
	s.mu.Unlock()
	if !stopping {
		t.Fatal("the daemon is not stopping after the stop")
	}
	s.startWeb()
	time.Sleep(50 * time.Millisecond)
	if done() != nil || s.listens() != n {
		t.Errorf("a start after the stop: listens %d -> %d", n, s.listens())
	}
}

// With no web options, nothing listens.
func TestNoWebNoListener(t *testing.T) {
	h := newHarness(t)
	got := make(chan *server, 1)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: io.Discard, onServer: func(s *server) { got <- s }})
	s := <-got
	time.Sleep(50 * time.Millisecond)
	if s.listens() != 0 || s.webDone != nil {
		t.Errorf("listened %d times with no web options", s.listens())
	}
}

// An address refused, or no token, starts no listener either.
func TestWebNotServable(t *testing.T) {
	h := newHarness(t)
	got := make(chan *server, 1)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: io.Discard,
		Web: &WebOptions{Addr: netip.MustParseAddrPort("0.0.0.0:0")}, onServer: func(s *server) { got <- s }})
	s := <-got
	time.Sleep(50 * time.Millisecond)
	if s.listens() != 0 || s.web.state().Listening {
		t.Errorf("listened on 0.0.0.0: %d", s.listens())
	}
}

// A port in use leaves the daemon running without its page, says so,
// logs it once, and the page is served once the port is free.
func TestWebPortTaken(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	addr := held.Addr().(*net.TCPAddr).AddrPort()
	h := newHarness(t)
	inTempDir(t, h.home)
	lg := &syncLog{}
	got := make(chan *server, 1)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: lg, webRetry: 20 * time.Millisecond,
		Web: &WebOptions{Addr: addr}, onServer: func(s *server) { got <- s }})
	s := <-got
	waitFor(t, 5*time.Second, "a few tries", func() bool { return s.listens() >= 4 })
	c := dialAs(t, h.dir, daemonproto.ClientCLI, 0)
	st, err := c.Status(t.Context())
	if err != nil || st.Daemon.Web == nil || st.Daemon.Web.Listening || !strings.Contains(st.Daemon.Web.Error, "in use") {
		t.Fatalf("status: %+v %v", st.Daemon.Web, err)
	}
	if r := webOf(t, c, false); r.Listening || r.URL != "http://"+addr.String()+"/" {
		t.Errorf("web: %+v", r)
	}
	if n := strings.Count(lg.String(), "is in use"); n != 1 {
		t.Errorf("logged %d times: %s", n, lg)
	}
	held.Close()
	if got := servedAddr(t, s); got != addr.String() {
		t.Errorf("served on %s, not %s", got, addr)
	}
	if code, _ := webCall(t, addr.String(), s.web.current(), "ping", `{}`); code != 200 {
		t.Errorf("ping: %d", code)
	}
}

// A page that polls with its token keeps the daemon from exiting idle;
// requests without it do not.
func TestWebActivityKeepsTheDaemon(t *testing.T) {
	h := newHarness(t)
	inTempDir(t, h.home)
	got := make(chan *server, 1)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Second, Log: io.Discard,
		Web: &WebOptions{Addr: webAddr}, onServer: func(s *server) { got <- s }})
	s := <-got
	addr := servedAddr(t, s)
	tok := s.web.current()
	for end := time.Now().Add(2500 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if code, _ := webCall(t, addr, tok, "ping", `{}`); code != 200 {
			t.Fatalf("ping: %d", code)
		}
	}
	select {
	case <-s.stopped:
		t.Fatal("exited idle while the page polled")
	default:
	}
	// Unauthenticated requests are no activity: it exits.
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-s.stopped:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("kept by requests without the token")
		}
		// The daemon may exit mid-request here, which is the point: the
		// call's error is not the test's.
		if conn, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond); err == nil {
			conn.Close()
			if r, err := http.NewRequest("POST", "http://"+addr+"/api/ping", strings.NewReader(`{}`)); err == nil {
				r.Header.Set("Origin", "http://"+addr)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set("Content-Type", "application/json")
				if res, err := http.DefaultClient.Do(r); err == nil {
					res.Body.Close()
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The page's allowlist is the method table's: what the table lists for
// the web kind, and nothing else.
func TestWebDispatchAllowed(t *testing.T) {
	h := newHarness(t)
	s, _ := webDaemon(t, h, &syncLog{})
	d := webDispatch{s}
	for _, m := range []string{"ping", "status", "requests", "request", "answer", "grants", "revoke", "permissions", "checkpoints"} {
		if !d.Allowed(m) {
			t.Errorf("%s refused to the page", m)
		}
	}
	for _, m := range []string{"web", "stop", "replace", "ensure_link", "restart_link", "stop_link", "writable", "subscribe", "nothing", ""} {
		if d.Allowed(m) {
			t.Errorf("%s allowed to the page", m)
		}
	}
}
