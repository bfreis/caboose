package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// egressHost is a fake host end that serves OpConnect: answer decides,
// per request, a refusal (Reason, Error) or, when it returns "", a stream
// that serve handles.
type egressHost struct {
	*fakeHost
	proxy string

	mu   sync.Mutex
	seen []agentproto.Message
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startEgress runs a link whose hello offers the proxy on a free port,
// with a fake host that answers OpConnect with answer's refusal, or with a
// stream handed to serve.
func startEgress(t *testing.T, answer func(m agentproto.Message) (reason, msg string), serve func(st *agentproto.Stream)) *egressHost {
	t.Helper()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	h := &egressHost{proxy: addr}
	h.fakeHost = startLinkWith(t, agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, Egress: addr}, procFixture(t))
	go func() {
		for b := range h.sess.Control() {
			m, _ := agentproto.Decode(b)
			if m.Type != agentproto.TypeRequest || m.Op != agentproto.OpConnect {
				continue
			}
			h.mu.Lock()
			h.seen = append(h.seen, m)
			h.mu.Unlock()
			if reason, msg := answer(m); reason != "" {
				h.sess.Send(agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, Error: msg, Reason: reason})
				continue
			}
			hdr, _ := json.Marshal(agentproto.StreamHeader{Request: m.ID})
			st, err := h.sess.Open(hdr)
			if err != nil {
				continue
			}
			h.sess.Send(agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, OK: true})
			go serve(st)
		}
	}()
	waitTCP(t, addr)
	return h
}

func (h *egressHost) requests() []agentproto.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.seen)
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listens at %s", addr)
}

func accept(agentproto.Message) (string, string) { return "", "" }

// upper echoes what it reads, upper-cased, and closes when the client has
// sent everything.
func upper(st *agentproto.Stream) {
	b, _ := io.ReadAll(st)
	st.Write(bytes.ToUpper(b))
	st.Close()
}

// rawRequest sends req to the proxy and reads its answer.
func rawRequest(t *testing.T, proxy, req string) (*http.Response, string) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, req)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer to %q: %v", req, err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// CONNECT reaches the host as a name and a port, and the bytes sent with
// it, before the 200, go through too.
func TestEgressConnect(t *testing.T) {
	h := startEgress(t, accept, upper)
	c, err := net.Dial("tcp", h.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\nhello, ")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	io.WriteString(c, "world")
	c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(br)
	if string(got) != "HELLO, WORLD" {
		t.Fatalf("through the tunnel: %q", got)
	}
	if r := h.requests(); len(r) != 1 || r[0].Host != "example.com" || r[0].Port != 443 {
		t.Fatalf("the host was asked %+v", r)
	}
}

// A plain-HTTP request for an absolute URL goes on in origin form, without
// what concerned the proxy.
func TestEgressPlainHTTP(t *testing.T) {
	type seen struct {
		uri, host, body, conn, proxyAuth, ua string
	}
	got := make(chan seen, 1)
	h := startEgress(t, accept, func(st *agentproto.Stream) {
		defer st.Close()
		req, err := http.ReadRequest(bufio.NewReader(st))
		if err != nil {
			return
		}
		b, _ := io.ReadAll(req.Body)
		got <- seen{req.RequestURI, req.Host, string(b), req.Header.Get("Connection"),
			req.Header.Get("Proxy-Authorization"), req.Header.Get("User-Agent")}
		io.WriteString(st, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	})
	u, _ := url.Parse("http://user:pw@" + h.proxy)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, "http://example.com:8080/path?q=1", strings.NewReader("body"))
	req.Header.Set("User-Agent", "test/1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(b) != "ok" {
		t.Fatalf("response %d %q", resp.StatusCode, b)
	}
	s := <-got
	if want := (seen{"/path?q=1", "example.com:8080", "body", "", "", "test/1"}); s != want {
		t.Fatalf("the server saw %+v, want %+v", s, want)
	}
	if r := h.requests(); len(r) != 1 || r[0].Host != "example.com" || r[0].Port != 8080 {
		t.Fatalf("the host was asked %+v", r)
	}

	// Port 80 by default, and no User-Agent where the client sent none.
	_, _ = rawRequest(t, h.proxy, "GET http://example.org/ HTTP/1.1\r\nHost: example.org\r\n\r\n")
	if s := <-got; s.ua != "" || s.uri != "/" {
		t.Fatalf("the server saw %+v", s)
	}
	if r := h.requests(); r[len(r)-1].Port != 80 {
		t.Fatalf("the host was asked %+v", r[len(r)-1])
	}
}

// keepServer serves HTTP/1.1 on a stream, request after request, as a
// server that keeps connections: what each path answers is below. It
// says what it was asked on seen, as "METHOD URI body", and ends the
// stream with "EOF".
func keepServer(seen chan<- string) func(st *agentproto.Stream) {
	return func(st *agentproto.Stream) {
		defer st.Close()
		br := bufio.NewReader(st)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				seen <- "EOF"
				return
			}
			if req.Header.Get("Expect") == "100-continue" {
				io.WriteString(st, "HTTP/1.1 100 Continue\r\n\r\n")
			}
			b, _ := io.ReadAll(req.Body)
			seen <- req.Method + " " + req.RequestURI + " " + string(b)
			switch req.URL.Path {
			case "/chunked":
				io.WriteString(st, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n")
			case "/head":
				io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n")
			case "/304":
				io.WriteString(st, "HTTP/1.1 304 Not Modified\r\nETag: \"x\"\r\n\r\n")
			case "/nolen":
				io.WriteString(st, "HTTP/1.1 200 OK\r\n\r\nto the end")
				return
			case "/bye":
				// Answered as kept, then closed, as a server's idle
				// timeout does.
				io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nbye")
				seen <- "EOF"
				return
			default:
				out := "got:" + string(b)
				fmt.Fprintf(st, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: keep-alive\r\nKeep-Alive: timeout=5\r\n\r\n%s", len(out), out)
			}
		}
	}
}

// keepClient is a raw connection to the proxy that sends requests and
// reads their responses in order.
type keepClient struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func dialKeep(t *testing.T, proxy string) *keepClient {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return &keepClient{t: t, c: c, br: bufio.NewReader(c)}
}

func (k *keepClient) send(req string) {
	k.t.Helper()
	if _, err := io.WriteString(k.c, req); err != nil {
		k.t.Fatal(err)
	}
}

func (k *keepClient) read(method string) (*http.Response, string) {
	k.t.Helper()
	resp, err := http.ReadResponse(k.br, &http.Request{Method: method})
	if err != nil {
		k.t.Fatalf("no response: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		k.t.Fatalf("the response's body: %v", err)
	}
	return resp, string(b)
}

// closed says whether the proxy has closed the connection.
func (k *keepClient) closed() bool {
	k.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := k.br.ReadByte()
	return errors.Is(err, io.EOF)
}

func expectSeen(t *testing.T, seen <-chan string, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-seen:
			if got != w {
				t.Fatalf("the server saw %q, want %q", got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the server saw nothing, want %q", w)
		}
	}
}

// Requests one after another on one client connection go on one stream to
// the host, whatever their bodies' framing, pipelined ones included.
func TestEgressKeepAlive(t *testing.T) {
	seen := make(chan string, 32)
	h := startEgress(t, accept, keepServer(seen))
	k := dialKeep(t, h.proxy)

	k.send("GET http://example.com/a HTTP/1.1\r\nHost: example.com\r\nProxy-Connection: keep-alive\r\n\r\n")
	if resp, body := k.read("GET"); resp.StatusCode != 200 || body != "got:" || resp.Close || resp.Header.Get("Keep-Alive") != "" {
		t.Fatalf("first: %d %q close=%v %v", resp.StatusCode, body, resp.Close, resp.Header)
	}
	k.send("POST http://example.com/len HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello")
	if _, body := k.read("POST"); body != "got:hello" {
		t.Fatalf("content-length body: %q", body)
	}
	k.send("POST http://example.com/len HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nhi\r\n3\r\nyou\r\n0\r\n\r\n")
	if _, body := k.read("POST"); body != "got:hiyou" {
		t.Fatalf("chunked body: %q", body)
	}
	k.send("GET http://example.com/chunked HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if resp, body := k.read("GET"); body != "abcde" || resp.Close {
		t.Fatalf("chunked response: %q close=%v", body, resp.Close)
	}
	k.send("HEAD http://example.com/head HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if resp, body := k.read("HEAD"); resp.ContentLength != 100 || body != "" || resp.Close {
		t.Fatalf("HEAD: length %d %q close=%v", resp.ContentLength, body, resp.Close)
	}
	k.send("GET http://example.com/304 HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if resp, body := k.read("GET"); resp.StatusCode != 304 || body != "" || resp.Close {
		t.Fatalf("304: %d %q close=%v", resp.StatusCode, body, resp.Close)
	}
	k.send("POST http://example.com/len HTTP/1.1\r\nHost: example.com\r\nExpect: 100-continue\r\nContent-Length: 4\r\n\r\n")
	if resp, err := http.ReadResponse(k.br, nil); err != nil || resp.StatusCode != 100 {
		t.Fatalf("no 100 Continue: %v %v", resp, err)
	}
	k.send("wait")
	if _, body := k.read("POST"); body != "got:wait" {
		t.Fatalf("after 100 Continue: %q", body)
	}
	// Pipelined: both sent before either is answered.
	k.send("GET http://example.com/p1 HTTP/1.1\r\nHost: example.com\r\n\r\nGET http://example.com/p2 HTTP/1.1\r\nHost: example.com\r\n\r\n")
	k.read("GET")
	k.read("GET")
	expectSeen(t, seen, "GET /a ", "POST /len hello", "POST /len hiyou", "GET /chunked ", "HEAD /head ",
		"GET /304 ", "POST /len wait", "GET /p1 ", "GET /p2 ")
	if r := h.requests(); len(r) != 1 {
		t.Fatalf("%d streams to the host, want 1: %+v", len(r), r)
	}
}

// Another host and port is another stream, and the one before is closed.
func TestEgressKeepAliveHostSwitch(t *testing.T) {
	seen := make(chan string, 32)
	h := startEgress(t, accept, keepServer(seen))
	k := dialKeep(t, h.proxy)
	for _, u := range []string{"http://a.example/1", "http://a.example/2", "http://b.example/3", "http://b.example:8080/4", "http://b.example:8080/5"} {
		k.send("GET " + u + " HTTP/1.1\r\nHost: x\r\n\r\n")
		if resp, _ := k.read("GET"); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", u, resp.StatusCode)
		}
	}
	expectSeen(t, seen, "GET /1 ", "GET /2 ", "EOF", "GET /3 ", "EOF", "GET /4 ", "GET /5 ")
	var got []string
	for _, m := range h.requests() {
		got = append(got, fmt.Sprint(m.Host, ":", m.Port))
	}
	if want := []string{"a.example:80", "b.example:80", "b.example:8080"}; !slices.Equal(got, want) {
		t.Fatalf("the host was asked %q, want %q", got, want)
	}
}

// A client's Connection: close, or HTTP/1.0 without keep-alive, ends the
// connection after the response, which says so; as does a response whose
// end is the connection's.
func TestEgressKeepAliveClose(t *testing.T) {
	seen := make(chan string, 32)
	h := startEgress(t, accept, keepServer(seen))
	for _, c := range []struct{ req, method, body string }{
		{"GET http://example.com/a HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n", "GET", "got:"},
		{"GET http://example.com/a HTTP/1.0\r\nHost: example.com\r\n\r\n", "GET", "got:"},
		{"GET http://example.com/chunked HTTP/1.0\r\nHost: example.com\r\n\r\n", "GET", "abcde"},
		{"GET http://example.com/nolen HTTP/1.1\r\nHost: example.com\r\n\r\n", "GET", "to the end"},
	} {
		k := dialKeep(t, h.proxy)
		k.send(c.req)
		resp, body := k.read(c.method)
		if body != c.body || !resp.Close || len(resp.TransferEncoding) != 0 && resp.ProtoMinor == 0 {
			t.Errorf("%.50q: %q close=%v te=%v", c.req, body, resp.Close, resp.TransferEncoding)
		}
		if !k.closed() {
			t.Errorf("%.50q: the connection stayed open", c.req)
		}
	}
	// HTTP/1.0 asking to keep it.
	k := dialKeep(t, h.proxy)
	k.send("GET http://example.com/a HTTP/1.0\r\nHost: example.com\r\nConnection: keep-alive\r\n\r\n")
	if resp, _ := k.read("GET"); resp.Close {
		t.Error("an HTTP/1.0 keep-alive was closed")
	}
}

// A kept stream the server has closed is found out by the next request,
// which goes again on a new one when it has no body.
func TestEgressKeepAliveServerClosed(t *testing.T) {
	seen := make(chan string, 32)
	h := startEgress(t, accept, keepServer(seen))
	k := dialKeep(t, h.proxy)
	k.send("GET http://example.com/bye HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if _, body := k.read("GET"); body != "bye" {
		t.Fatalf("first: %q", body)
	}
	expectSeen(t, seen, "GET /bye ", "EOF")
	k.send("GET http://example.com/again HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if resp, body := k.read("GET"); resp.StatusCode != 200 || body != "got:" {
		t.Fatalf("after the server closed: %d %q", resp.StatusCode, body)
	}
	if r := h.requests(); len(r) != 2 {
		t.Fatalf("%d streams to the host, want 2", len(r))
	}

	// One with a body is not sent twice: the client hears so.
	k.send("GET http://example.com/bye HTTP/1.1\r\nHost: example.com\r\n\r\n")
	k.read("GET")
	k.send("POST http://example.com/len HTTP/1.1\r\nHost: example.com\r\nContent-Length: 1\r\n\r\nx")
	if resp, _ := k.read("POST"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a body on a closed stream: %d", resp.StatusCode)
	}
}

// A kept connection with no next request is closed after the idle timeout.
func TestEgressKeepAliveIdle(t *testing.T) {
	old := egressIdleTimeout
	egressIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { egressIdleTimeout = old })
	seen := make(chan string, 32)
	h := startEgress(t, accept, keepServer(seen))
	k := dialKeep(t, h.proxy)
	k.send("GET http://example.com/a HTTP/1.1\r\nHost: example.com\r\n\r\n")
	k.read("GET")
	start := time.Now()
	if !k.closed() {
		t.Fatal("an idle connection stayed open")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("closed only after %v", d)
	}
	expectSeen(t, seen, "GET /a ", "EOF")
}

// Each refusal is answered with its status and the host's words.
func TestEgressRefusals(t *testing.T) {
	cases := map[string]int{
		agentproto.EgressOff:     403,
		agentproto.EgressBad:     403,
		agentproto.EgressPort:    403,
		agentproto.EgressDenied:  403,
		agentproto.EgressResolve: 502,
		agentproto.EgressDial:    502,
		agentproto.EgressLimit:   503,
	}
	h := startEgress(t, func(m agentproto.Message) (string, string) {
		return m.Host, "refused as " + m.Host
	}, upper)
	for reason, status := range cases {
		for _, req := range []string{
			"CONNECT " + reason + ":443 HTTP/1.1\r\n\r\n",
			"GET http://" + reason + "/ HTTP/1.1\r\nHost: " + reason + "\r\n\r\n",
		} {
			resp, body := rawRequest(t, h.proxy, req)
			if resp.StatusCode != status || body != "caboose: refused as "+reason+"\n" {
				t.Errorf("%q: %d %q, want %d", req, resp.StatusCode, body, status)
			}
		}
	}
	if got := egressStatus(""); got != 503 {
		t.Errorf("the link's own trouble: %d, want 503", got)
	}
}

// What is not a proxy request is refused here, and never reaches the host.
func TestEgressBadRequests(t *testing.T) {
	h := startEgress(t, accept, upper)
	for req, want := range map[string]string{
		"GET / HTTP/1.1\r\nHost: x\r\n\r\n":                           "it takes CONNECT host:port",
		"GET https://example.com/ HTTP/1.1\r\nHost: x\r\n\r\n":        "goes through CONNECT",
		"CONNECT example.com HTTP/1.1\r\n\r\n":                        "CONNECT needs host:port",
		"CONNECT example.com:99999 HTTP/1.1\r\n\r\n":                  "is not a port",
		"CONNECT " + strings.Repeat("a", 300) + ":1 HTTP/1.1\r\n\r\n": "at most 253 bytes",
		"nonsense\r\n\r\n": "not an HTTP request",
	} {
		resp, body := rawRequest(t, h.proxy, req)
		if resp.StatusCode != 400 || !strings.Contains(body, want) {
			t.Errorf("%.40q: %d %q, want 400 and %q", req, resp.StatusCode, body, want)
		}
	}
	if r := h.requests(); len(r) != 0 {
		t.Fatalf("the host was asked %+v", r)
	}
}

func TestEgressHeadLimits(t *testing.T) {
	old := egressHeadTimeout
	egressHeadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { egressHeadTimeout = old })
	h := startEgress(t, accept, upper)

	big := "GET http://example.com/ HTTP/1.1\r\nX: " + strings.Repeat("a", egressHeadLimit) + "\r\n\r\n"
	if resp, _ := rawRequest(t, h.proxy, big); resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("oversized headers: %d", resp.StatusCode)
	}

	// A byte at a time, slower than the whole head may take.
	c, err := net.Dial("tcp", h.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	go func() {
		for _, b := range []byte("GET http://example.com/ HTTP/1.1\r\nX: slow") {
			if _, err := c.Write([]byte{b}); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("slow headers: %v %v", resp, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cut off only after %v", d)
	}
}

// The proxy goes with the link, and its port is never reported.
func TestEgressListensWhileLinked(t *testing.T) {
	port := freePort(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "net"), 0o755)
	os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(fmt.Sprintf(`  sl  local_address rem_address   st
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000   501        0 1 1 0 100 0 0 10 0
   1: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000   501        0 1 1 0 100 0 0 10 0
`, port)), 0o644)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	h := startLinkWith(t, agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, Egress: addr}, root)
	waitTCP(t, addr)
	deadline := time.Now().Add(5 * time.Second)
	for {
		m := h.nextOf(t, agentproto.TypePorts)
		if slices.Equal(m.Ports, []int{3000}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ports %v, want only 3000", m.Ports)
		}
	}
	h.sess.Close()
	<-h.done
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			break
		}
		c.Close()
		if i > 200 {
			t.Fatal("the proxy outlived its link")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A hello that offers no proxy, or one not on loopback, gets none.
func TestEgressOnlyOnLoopback(t *testing.T) {
	port := freePort(t)
	startLinkWith(t, agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, Egress: fmt.Sprintf("0.0.0.0:%d", port)}, procFixture(t))
	time.Sleep(200 * time.Millisecond)
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		c.Close()
		t.Fatal("the proxy listens on a hello's non-loopback address")
	}
}

func TestSplitTarget(t *testing.T) {
	for in, want := range map[string]string{
		"example.com:22":   "example.com 22",
		"[2001:db8::1]:22": "2001:db8::1 22",
		"example.com":      "example.com 80",
		"[::1]":            "::1 80",
		"example.com:":     "example.com 80",
	} {
		h, p, err := splitTarget(in, 80)
		if got := fmt.Sprint(h, " ", p); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"2001:db8::1", ":22", "x:0", "x:http"} {
		if _, _, err := splitTarget(in, 80); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
	if _, _, err := splitTarget("example.com", -1); err == nil {
		t.Error("CONNECT without a port: no error")
	}
}

// fakeProxy answers one CONNECT with status and body, and on a 200 echoes
// what follows, upper-cased.
func fakeProxy(t *testing.T, status int, body string) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	asked := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		asked <- req.Method + " " + req.URL.Host
		fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\n\r\n%s", status, http.StatusText(status), len(body), body)
		if status == 200 {
			b, _ := io.ReadAll(br)
			c.Write(bytes.ToUpper(b))
		}
	}()
	return ln.Addr().String(), asked
}

func TestConnectCommand(t *testing.T) {
	proxy, asked := fakeProxy(t, 200, "")
	var out bytes.Buffer
	if err := Connect(proxy, "2001:db8::1", 22, strings.NewReader("ssh-2.0"), &out); err != nil {
		t.Fatal(err)
	}
	if a := <-asked; a != "CONNECT [2001:db8::1]:22" {
		t.Errorf("asked %q", a)
	}
	if out.String() != "SSH-2.0" {
		t.Errorf("got %q", out.String())
	}

	proxy, _ = fakeProxy(t, 403, "caboose: git.example:25: port 25 is not in egress_ports (config.toml on the host)\n")
	err := Connect(proxy, "git.example", 25, strings.NewReader(""), io.Discard)
	if err == nil || err.Error() != "cannot connect to git.example:25 through the host (403): git.example:25: port 25 is not in egress_ports (config.toml on the host)" {
		t.Errorf("refused: %v", err)
	}

	if err := Connect(fmt.Sprintf("127.0.0.1:%d", freePort(t)), "x", 22, strings.NewReader(""), io.Discard); err != ErrNoProxy {
		t.Errorf("no proxy: %v", err)
	}

	var stderr bytes.Buffer
	if code := Main([]string{"connect", "x", "port"}, strings.NewReader(""), nopCloser{io.Discard}, &stderr); code != 2 || !strings.Contains(stderr.String(), `"port" is not a port`) {
		t.Errorf("bad port: %d %q", code, stderr.String())
	}
}

// Through the real proxy, over a fake link: what ssh's ProxyCommand does.
func TestConnectThroughTheLink(t *testing.T) {
	h := startEgress(t, accept, upper)
	var out bytes.Buffer
	if err := Connect(h.proxy, "git.example", 22, strings.NewReader("ssh-2.0"), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "SSH-2.0" {
		t.Fatalf("got %q", out.String())
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// wait-proxy waits for the proxy to listen, and gives up when it does not.
func TestWaitProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := WaitProxy(addr, 300*time.Millisecond); !errors.Is(err, ErrNoProxy) {
		t.Errorf("nothing listening: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		if l, err := net.Listen("tcp", addr); err == nil {
			t.Cleanup(func() { l.Close() })
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
		}
	}()
	if err := WaitProxy(addr, 5*time.Second); err != nil {
		t.Errorf("listening after a while: %v", err)
	}
	var stderr strings.Builder
	for _, args := range [][]string{{"wait-proxy", "x"}, {"wait-proxy", "601"}, {"wait-proxy", "1", "2"}} {
		if code := Main(args, nil, nopCloser{io.Discard}, &stderr); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

// The guest's hostname joins NO_PROXY and no_proxy, once, and only where
// they are set.
func TestNoProxyHost(t *testing.T) {
	env := append([]string{"PATH=/bin"}, agentproto.EgressEnv(agentproto.EgressListen)...)
	got := noProxyHost(env, "caboose")
	want := agentproto.EgressNoProxy + ",caboose"
	if envValue(got, "NO_PROXY") != want || envValue(got, "no_proxy") != want || envValue(got, "PATH") != "/bin" {
		t.Errorf("%q", got)
	}
	if again := noProxyHost(got, "caboose"); !slices.Equal(again, got) {
		t.Errorf("added twice: %q", again)
	}
	if envValue(env, "NO_PROXY") != agentproto.EgressNoProxy {
		t.Error("env was changed in place")
	}
	for _, h := range []string{"", "a,b", "a b"} {
		if out := noProxyHost(env, h); !slices.Equal(out, env) {
			t.Errorf("hostname %q: %q", h, out)
		}
	}
	if out := noProxyHost([]string{"PATH=/bin"}, "caboose"); !slices.Equal(out, []string{"PATH=/bin"}) {
		t.Errorf("set where unset: %q", out)
	}
}

// envValue is the last value env gives name.
func envValue(env []string, name string) string {
	v := ""
	for _, e := range env {
		if k, val, ok := strings.Cut(e, "="); ok && k == name {
			v = val
		}
	}
	return v
}

// A stream answering a request that the session refused fails the request
// at once, not when its wait runs out.
func TestRefusedStreamFailsTheRequest(t *testing.T) {
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	host := agentproto.NewSession(hr, hw, true)
	l := &Link{
		sess:    agentproto.NewSession(ar, aw, false),
		pending: map[uint64]chan agentproto.Message{},
		waiting: map[uint64]chan *agentproto.Stream{},
	}
	l.sess.OnRefused(l.refused)
	t.Cleanup(func() { host.Close(); l.sess.Close() })
	go l.readControl()
	go func() {
		for b := range host.Control() {
			m, _ := agentproto.Decode(b)
			if m.Type != agentproto.TypeRequest {
				continue
			}
			// What the session does with a stream it cannot take.
			hdr, _ := json.Marshal(agentproto.StreamHeader{Request: m.ID})
			l.refused(hdr)
			host.Send(agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, OK: true})
		}
	}()
	start := time.Now()
	st, r := l.openStream(agentproto.Message{Op: agentproto.OpConnect, Host: "example.com", Port: 443}, egressWait)
	if st != nil || r.Error == "" {
		t.Fatalf("got a stream %v, response %+v", st, r)
	}
	if d := time.Since(start); d > egressWait/4 {
		t.Fatalf("the request failed after %v", d)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) != 0 || len(l.waiting) != 0 {
		t.Fatalf("left behind: %d pending, %d waiting", len(l.pending), len(l.waiting))
	}
}

// pipeStream is how pipeServer serves one stream: it holds its first
// response until it has read hold requests, and closes the stream once it
// has answered closeAfter, when that is not 0.
type pipeStream struct{ hold, closeAfter int }

// pipeServer serves HTTP/1.1 as a server that takes pipelined requests
// does: it reads them as they come, beside its responses, and answers them
// in order, each delay after it was read. It says on seen what happens:
// "METHOD URI body" for a request read, "sent URI" before its response,
// "EOF" when the stream ends. Stream i is served as streams[i] says.
func pipeServer(seen chan<- string, delay time.Duration, streams ...pipeStream) func(st *agentproto.Stream) {
	var mu sync.Mutex
	n := 0
	return func(st *agentproto.Stream) {
		defer st.Close()
		mu.Lock()
		var cfg pipeStream
		if n < len(streams) {
			cfg = streams[n]
		}
		n++
		mu.Unlock()
		type read struct {
			req *http.Request
			at  time.Time
		}
		reqs := make(chan read, 64)
		go func() {
			defer close(reqs)
			br := bufio.NewReader(st)
			for {
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				b, _ := io.ReadAll(req.Body)
				seen <- req.Method + " " + req.RequestURI + " " + string(b)
				reqs <- read{req, time.Now()}
			}
		}()
		var held []read
		for len(held) < cfg.hold {
			r, ok := <-reqs
			if !ok {
				break
			}
			held = append(held, r)
		}
		for answered := 0; ; {
			var r read
			if len(held) > 0 {
				r, held = held[0], held[1:]
			} else if next, ok := <-reqs; ok {
				r = next
			} else {
				seen <- "EOF"
				return
			}
			time.Sleep(time.Until(r.at.Add(delay)))
			seen <- "sent " + r.req.RequestURI
			switch r.req.URL.Path {
			case "/head":
				io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n")
			case "/close":
				io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nConnection: close\r\n\r\nbye")
				return
			case "/cut":
				io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
				return
			default:
				out := "got:" + r.req.RequestURI
				fmt.Fprintf(st, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(out), out)
			}
			if answered++; answered == cfg.closeAfter {
				return
			}
		}
	}
}

// startPipe runs the proxy with pipeServer behind it, and says on seen
// too each host:port the host is asked for, as "connect host:port".
func startPipe(t *testing.T, delay time.Duration, streams ...pipeStream) (*egressHost, chan string) {
	t.Helper()
	seen := make(chan string, 256)
	h := startEgress(t, func(m agentproto.Message) (string, string) {
		seen <- fmt.Sprint("connect ", m.Host, ":", m.Port)
		return "", ""
	}, pipeServer(seen, delay, streams...))
	return h, seen
}

// until reads seen up to last, and says what came.
func until(t *testing.T, seen <-chan string, last string) []string {
	t.Helper()
	var got []string
	for {
		select {
		case s := <-seen:
			got = append(got, s)
			if s == last {
				return got
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q in %q", last, got)
		}
	}
}

// before fails unless a came before b in got, both there.
func before(t *testing.T, got []string, a, b string) {
	t.Helper()
	i, j := slices.Index(got, a), slices.Index(got, b)
	if i < 0 || j < 0 || i > j {
		t.Fatalf("want %q before %q in %q", a, b, got)
	}
}

func get(path string) string {
	return "GET http://example.com" + path + " HTTP/1.1\r\nHost: example.com\r\n\r\n"
}

// noProxyLeft waits for every proxy client's goroutines to be gone.
func noProxyLeft(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		buf := make([]byte, 1<<20)
		s := string(buf[:runtime.Stack(buf, true)])
		if !strings.Contains(s, "agent.(*pipeline)") && !strings.Contains(s, "agent.(*Link).proxyClient") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a proxy client is left running:\n%s", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Requests sent together go on before the first is answered, a HEAD among
// them, and are answered in order on one stream.
func TestEgressPipelined(t *testing.T) {
	h, seen := startPipe(t, 0, pipeStream{hold: 3})
	k := dialKeep(t, h.proxy)
	k.send(get("/1") + "HEAD http://example.com/head HTTP/1.1\r\nHost: example.com\r\n\r\n" + get("/3"))
	if _, body := k.read("GET"); body != "got:/1" {
		t.Fatalf("first: %q", body)
	}
	if resp, body := k.read("HEAD"); resp.ContentLength != 100 || body != "" || resp.Close {
		t.Fatalf("HEAD: length %d %q close=%v", resp.ContentLength, body, resp.Close)
	}
	if _, body := k.read("GET"); body != "got:/3" {
		t.Fatalf("third: %q", body)
	}
	// The server read all three before it answered the first.
	expectSeen(t, seen, "connect example.com:80", "GET /1 ", "HEAD /head ", "GET /3 ", "sent /1", "sent /head", "sent /3")
	k.c.Close()
	expectSeen(t, seen, "EOF")
	if r := h.requests(); len(r) != 1 {
		t.Fatalf("%d streams to the host, want 1", len(r))
	}
	noProxyLeft(t)
}

// A request for another host and port waits for those before it to be
// answered, then goes on its own stream, and pipelines there.
func TestEgressPipelineHostSwitch(t *testing.T) {
	h, seen := startPipe(t, 0, pipeStream{hold: 2}, pipeStream{hold: 2})
	k := dialKeep(t, h.proxy)
	var all string
	for _, u := range []string{"http://a.example/1", "http://a.example/2", "http://b.example/3", "http://b.example/4"} {
		all += "GET " + u + " HTTP/1.1\r\nHost: x\r\n\r\n"
	}
	k.send(all)
	for _, want := range []string{"got:/1", "got:/2", "got:/3", "got:/4"} {
		if _, body := k.read("GET"); body != want {
			t.Fatalf("got %q, want %q", body, want)
		}
	}
	got := until(t, seen, "sent /4")
	before(t, got, "sent /2", "connect b.example:80")
	before(t, got, "connect b.example:80", "GET /3 ")
	if slices.Index(got, "GET /4 ") > slices.Index(got, "sent /3") {
		t.Fatalf("not pipelined on the second stream: %q", got)
	}
	k.c.Close()
	noProxyLeft(t)
}

// A request with a body waits for those before it to be answered, and
// those after it for it.
func TestEgressPipelineBodyWaits(t *testing.T) {
	h, seen := startPipe(t, 0, pipeStream{hold: 2})
	k := dialKeep(t, h.proxy)
	k.send(get("/1") + get("/2") +
		"POST http://example.com/3 HTTP/1.1\r\nHost: example.com\r\nContent-Length: 4\r\n\r\nbody" + get("/4") + get("/5"))
	for _, want := range []string{"got:/1", "got:/2", "got:/3", "got:/4", "got:/5"} {
		if _, body := k.read("GET"); body != want {
			t.Fatalf("got %q, want %q", body, want)
		}
	}
	got := until(t, seen, "sent /5")
	before(t, got, "sent /2", "POST /3 body")
	before(t, got, "sent /3", "GET /4 ")
	if r := h.requests(); len(r) != 1 {
		t.Fatalf("%d streams to the host, want 1", len(r))
	}
	k.c.Close()
	noProxyLeft(t)
}

// A server that closes the stream with requests unanswered, as one that
// takes so many per connection does, has them sent again on a new stream,
// in order.
func TestEgressPipelineServerCloses(t *testing.T) {
	h, seen := startPipe(t, 0, pipeStream{hold: 5, closeAfter: 2})
	k := dialKeep(t, h.proxy)
	k.send(get("/1") + get("/2") + get("/3") + get("/4") + get("/5"))
	for _, want := range []string{"got:/1", "got:/2", "got:/3", "got:/4", "got:/5"} {
		if resp, body := k.read("GET"); body != want || resp.Close {
			t.Fatalf("got %q close=%v, want %q", body, resp.Close, want)
		}
	}
	got := until(t, seen, "sent /5")
	again := slices.DeleteFunc(got[slices.Index(got, "sent /2")+1:], func(s string) bool {
		return !strings.HasPrefix(s, "connect ") && !strings.HasPrefix(s, "GET ")
	})
	if want := []string{"connect example.com:80", "GET /3 ", "GET /4 ", "GET /5 "}; !slices.Equal(again, want) {
		t.Fatalf("after the close: %q, want %q", again, want)
	}
	if r := h.requests(); len(r) != 2 {
		t.Fatalf("%d streams to the host, want 2", len(r))
	}
	// The connection goes on.
	k.send(get("/6"))
	if _, body := k.read("GET"); body != "got:/6" {
		t.Fatalf("after: %q", body)
	}
	k.c.Close()
	noProxyLeft(t)
}

// A client's Connection: close among pipelined requests is the last sent:
// the connection ends after its response, and what came after is dropped.
func TestEgressPipelineClientClose(t *testing.T) {
	h, seen := startPipe(t, 0, pipeStream{hold: 2})
	k := dialKeep(t, h.proxy)
	k.send(get("/1") + "GET http://example.com/2 HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n" + get("/3"))
	if resp, body := k.read("GET"); body != "got:/1" || resp.Close {
		t.Fatalf("first: %q close=%v", body, resp.Close)
	}
	if resp, body := k.read("GET"); body != "got:/2" || !resp.Close {
		t.Fatalf("second: %q close=%v", body, resp.Close)
	}
	if !k.closed() {
		t.Fatal("the connection stayed open")
	}
	got := until(t, seen, "EOF")
	if slices.Contains(got, "GET /3 ") {
		t.Fatalf("sent after the close: %q", got)
	}
	noProxyLeft(t)
}

// A response that ends the connection among pipelined requests ends the
// client's, and the requests after it are dropped.
func TestEgressPipelineServerEnds(t *testing.T) {
	h, _ := startPipe(t, 0, pipeStream{hold: 3})
	k := dialKeep(t, h.proxy)
	k.send(get("/1") + get("/close") + get("/3"))
	k.read("GET")
	if resp, body := k.read("GET"); body != "bye" || !resp.Close {
		t.Fatalf("the closing response: %q close=%v", body, resp.Close)
	}
	if !k.closed() {
		t.Fatal("the connection stayed open")
	}
	noProxyLeft(t)
}

// A server gone in the middle of a response ends the client's connection
// there.
func TestEgressPipelineCutShort(t *testing.T) {
	h, _ := startPipe(t, 0, pipeStream{hold: 3})
	k := dialKeep(t, h.proxy)
	k.send(get("/1") + get("/cut") + get("/3"))
	k.read("GET")
	resp, err := http.ReadResponse(k.br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(resp.Body); err == nil || string(b) != "short" {
		t.Fatalf("a body cut short read as %q, %v", b, err)
	}
	k.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := k.br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection stayed open: %v", err)
	}
	noProxyLeft(t)
}

// Ten small requests sent together take about one round trip, not ten.
func TestEgressPipelineLatency(t *testing.T) {
	const delay, n = 20 * time.Millisecond, 10
	h, _ := startPipe(t, delay)
	k := dialKeep(t, h.proxy)
	// The stream is open before the clock starts.
	k.send(get("/0"))
	k.read("GET")
	var all string
	for i := range n {
		all += get(fmt.Sprint("/", i))
	}
	start := time.Now()
	k.send(all)
	for range n {
		k.read("GET")
	}
	d := time.Since(start)
	t.Logf("%d requests, each answered %v after it reached the server: %v", n, delay, d.Round(time.Millisecond))
	if d >= n*delay/2 {
		t.Fatalf("%d requests took %v, as if one at a time", n, d)
	}
}

// A server that closes every stream after a few responses, under requests
// sent in bursts of any size, has every one answered, in order.
func TestEgressPipelineServerClosesOften(t *testing.T) {
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	streams := make([]pipeStream, 200)
	for i := range streams {
		streams[i].closeAfter = 1 + rng.Intn(4)
	}
	h, seen := startPipe(t, 0, streams...)
	go func() {
		for range seen {
		}
	}()
	k := dialKeep(t, h.proxy)
	const n = 80
	sizes := make([]int, 0, n)
	for left := n; left > 0; {
		s := min(left, 1+rng.Intn(12))
		sizes = append(sizes, s)
		left -= s
	}
	pauses := make([]time.Duration, len(sizes))
	for i := range pauses {
		pauses[i] = time.Duration(rng.Intn(3)) * time.Millisecond
	}
	go func() {
		i := 0
		for b, s := range sizes {
			var burst string
			for range s {
				burst += get(fmt.Sprint("/", i))
				i++
			}
			if _, err := io.WriteString(k.c, burst); err != nil {
				return
			}
			time.Sleep(pauses[b])
		}
	}()
	for i := range n {
		resp, err := http.ReadResponse(k.br, nil)
		if err != nil {
			t.Fatalf("seed %d: response %d: %v", seed, i, err)
		}
		b, err := io.ReadAll(resp.Body)
		if want := fmt.Sprint("got:/", i); err != nil || string(b) != want {
			t.Fatalf("seed %d: response %d: %q %v, want %q", seed, i, b, err, want)
		}
	}
	k.c.Close()
	noProxyLeft(t)
}

// A request whose method may change something on the server goes alone,
// even without a body, and is never sent twice: a kept stream the server
// closed answers it 502.
func TestEgressPipelineUnsafeMethod(t *testing.T) {
	post := "POST http://example.com/%d HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n\r\n"
	h, seen := startPipe(t, 0, pipeStream{hold: 2})
	k := dialKeep(t, h.proxy)
	k.send(get("/1") + get("/2") + fmt.Sprintf(post, 3) + get("/4"))
	for _, want := range []string{"got:/1", "got:/2", "got:/3", "got:/4"} {
		if _, body := k.read("GET"); body != want {
			t.Fatalf("got %q, want %q", body, want)
		}
	}
	got := until(t, seen, "sent /4")
	before(t, got, "sent /2", "POST /3 ")
	before(t, got, "sent /3", "GET /4 ")
	k.c.Close()

	h, seen = startPipe(t, 0, pipeStream{closeAfter: 1})
	k = dialKeep(t, h.proxy)
	k.send(get("/1") + fmt.Sprintf(post, 2))
	k.read("GET")
	if resp, _ := k.read("POST"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a POST on a closed stream: %d", resp.StatusCode)
	}
	if r := h.requests(); len(r) != 1 {
		t.Fatalf("%d streams to the host, want 1", len(r))
	}
	time.Sleep(100 * time.Millisecond)
	for posts := 0; ; {
		select {
		case s := <-seen:
			if strings.HasPrefix(s, "POST ") {
				if posts++; posts > 1 {
					t.Fatal("the POST was sent twice")
				}
			}
			continue
		default:
		}
		break
	}
	noProxyLeft(t)
}

// A client gone with a response still to come takes its connection's
// goroutines with it, while the server says nothing.
func TestEgressPipelineClientReset(t *testing.T) {
	h, seen := startPipe(t, 0, pipeStream{hold: 99})
	k := dialKeep(t, h.proxy)
	k.send(get("/1"))
	expectSeen(t, seen, "connect example.com:80", "GET /1 ")
	k.c.(*net.TCPConn).SetLinger(0)
	k.c.Close()
	noProxyLeft(t)
}
