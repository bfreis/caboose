package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/saferm"
)

// brokerHost is a fake host behind a real link: it answers the link's
// requests with handle, and passes on the mods the link reports.
type brokerHost struct {
	*fakeHost
	mods chan agentproto.ModsInfo
	mu   sync.Mutex
	seen []agentproto.Message
}

func startBrokerLink(t *testing.T, broker bool, procRoot string, handle func(agentproto.Message) agentproto.Message) *brokerHost {
	t.Helper()
	return startHelloLink(t, agentproto.Message{Broker: broker}, procRoot, handle)
}

// startHelloLink is startBrokerLink with the host's hello offering what
// hello does.
func startHelloLink(t *testing.T, hello agentproto.Message, procRoot string, handle func(agentproto.Message) agentproto.Message) *brokerHost {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	dir, err := os.MkdirTemp("", "cbb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { saferm.RemoveRoot(dir) })
	h := &brokerHost{mods: make(chan agentproto.ModsInfo, 64), fakeHost: &fakeHost{
		sess:   agentproto.NewSession(hr, hw, true),
		socket: filepath.Join(dir, "s"),
		done:   make(chan error, 1),
	}}
	go func() {
		h.done <- RunLink(ar, aw, Config{Socket: h.socket, ProcRoot: procRoot, Interval: time.Hour,
			ModsEvery: 10 * time.Millisecond, ScanEvery: 10 * time.Millisecond})
	}()
	t.Cleanup(func() { h.sess.Close() })
	if m := h.next(t); m.Type != agentproto.TypeHello {
		t.Fatalf("first message %+v, want a hello", m)
	}
	hello.Type, hello.Version = agentproto.TypeHello, agentproto.Version
	h.sess.Send(hello)
	go func() {
		for b := range h.sess.Control() {
			m, err := agentproto.Decode(b)
			if err != nil {
				continue
			}
			switch m.Type {
			case agentproto.TypeMods:
				if m.Mods != nil {
					h.mods <- *m.Mods
				}
			case agentproto.TypeRequest:
				h.mu.Lock()
				h.seen = append(h.seen, m)
				h.mu.Unlock()
				go func() {
					r := handle(m)
					r.Type, r.ID = agentproto.TypeResponse, m.ID
					h.sess.Send(r)
				}()
			}
		}
	}()
	waitSocket(t, h.socket)
	return h
}

func (h *brokerHost) last(t *testing.T) agentproto.Message {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) == 0 {
		t.Fatal("the host saw no request")
	}
	return h.seen[len(h.seen)-1]
}

func verdicts(m agentproto.Message) agentproto.Message {
	switch m.Ask.Host {
	case "allowed.example":
		return agentproto.Message{OK: true, Verdict: agentproto.VerdictAllowed}
	case "pending.example":
		return agentproto.Message{Verdict: agentproto.VerdictPending, Reason: agentproto.ReasonPending, RequestID: 7, Error: "pending #7: waits for a person"}
	case "evil.example":
		return agentproto.Message{Verdict: agentproto.VerdictDenied, Error: "no \x1b[31mred\x07 text\nsecond line"}
	}
	return agentproto.Message{Error: "unknown host"}
}

func runRequest(t *testing.T, socket string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := requestWith(socket, func(k string) string {
		if k == sessionVar {
			return "sess-from-env"
		}
		return ""
	}, args, &out, &errb, func() int { return 2 })
	return code, out.String(), errb.String()
}

func TestRequestExitCodes(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), verdicts)
	code, out, _ := runRequest(t, h.socket, "egress", "--host", "allowed.example", "--port", "443", "--context", "why")
	if code != 0 || strings.TrimSpace(out) != "allowed" {
		t.Fatalf("allowed: %d %q", code, out)
	}
	if a := h.last(t).Ask; a.Session != "sess-from-env" || a.Kind != "egress" || a.Port != 443 || a.Context != "why" {
		t.Fatalf("the host saw %+v", a)
	}
	code, out, _ = runRequest(t, h.socket, "egress", "--host=pending.example", "--port=443", "--session", "mine")
	if code != ExitPending || strings.TrimSpace(out) != "pending #7" {
		t.Fatalf("pending: %d %q", code, out)
	}
	if h.last(t).Ask.Session != "mine" {
		t.Fatalf("--session lost: %+v", h.last(t).Ask)
	}
	code, out, errb := runRequest(t, h.socket, "egress", "--host", "evil.example", "--port", "443")
	if code != ExitDenied || out != "" {
		t.Fatalf("denied: %d %q", code, out)
	}
	for _, c := range errb {
		if c < ' ' && c != '\n' {
			t.Fatalf("control character in %q", errb)
		}
	}
	if strings.Count(errb, "\n") != 1 || !strings.Contains(errb, "denied: no [31mred text second line") {
		t.Fatalf("denied text %q", errb)
	}
	code, out, _ = runRequest(t, h.socket, "egress", "--host", "pending.example", "--port", "443", "--json")
	var res requestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != ExitPending || res.Verdict != "pending" || res.Request != 7 {
		t.Fatalf("json: %d %q %v", code, out, err)
	}
	// Anything else the host says is an error, not a verdict.
	code, _, errb = runRequest(t, h.socket, "egress", "--host", "other.example", "--port", "443")
	if code != 1 || !strings.Contains(errb, "unknown host") {
		t.Fatalf("error: %d %q", code, errb)
	}
}

func TestRequestUsage(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), verdicts)
	for _, args := range [][]string{
		{"nonsense"},
		{"egress", "--host", "a.example"},
		{"egress", "--host", "a.example", "--port", "99999"},
		{"open_url"},
		{"alias", "--alias", "Bad Name"},
		{"alias", "--alias", "x", "--arg", "novalue"},
		{"egress", "--host", "a", "--port", "1", "--context", strings.Repeat("x", agentproto.MaxAskContext+1)},
		{"egress", "--bogus"},
		{"egress", "--host", "a", "--host", "b", "--port", "1"},
	} {
		if code, _, errb := runRequest(t, h.socket, args...); code != 2 {
			t.Errorf("%v: exit %d, %q", args, code, errb)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) != 0 {
		t.Fatalf("a refused command still asked the host: %+v", h.seen)
	}
}

// What a command sends is bounded again by the link: a client of the
// socket need not be this command.
func TestLinkBoundsWhatItSends(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), verdicts)
	opts := []string{"a", "b", "c", "d", "e", "f", strings.Repeat("é", 500)}
	args := map[string]string{}
	for i := 0; i < 100; i++ {
		args["k"+strconv.Itoa(i)] = "v"
	}
	_, err := askCtx(context.Background(), h.socket, agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &agentproto.Ask{
		Kind: "egress", Host: "allowed.example", Port: 1, Context: strings.Repeat("é", 5000), Options: opts,
		Recommendation: strings.Repeat("r", 9000), Meanwhile: strings.Repeat("m", 9000), Session: strings.Repeat("s", 900), Args: args,
	}}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a := h.last(t).Ask
	if len(a.Context) > agentproto.MaxAskContext || len(a.Options) != agentproto.MaxAskOptions || len(a.Recommendation) > agentproto.MaxAskRecommendation ||
		len(a.Meanwhile) > agentproto.MaxAskMeanwhile || len(a.Session) > agentproto.MaxAskSession || len(a.Args) > agentproto.MaxAskArgs {
		t.Fatalf("not bounded: %d %d %d %d %d %d", len(a.Context), len(a.Options), len(a.Recommendation), len(a.Meanwhile), len(a.Session), len(a.Args))
	}
	for _, o := range a.Options {
		if len(o) > agentproto.MaxAskOption {
			t.Fatalf("option of %d bytes", len(o))
		}
	}
	if !strings.HasSuffix(a.Context, "é") {
		t.Fatalf("cut inside a character: %q", a.Context[len(a.Context)-3:])
	}
}

func TestNoBrokerRefusal(t *testing.T) {
	h := startBrokerLink(t, false, t.TempDir(), verdicts)
	code, _, errb := runRequest(t, h.socket, "egress", "--host", "allowed.example", "--port", "443")
	if code != 1 || !strings.Contains(errb, "files no requests") {
		t.Fatalf("%d %q", code, errb)
	}
	var out, e bytes.Buffer
	if code := requestsWith(h.socket, t.TempDir(), 2, nil, &out, &e, func() int { return 2 }); code != 1 || !strings.Contains(e.String(), "files no requests") {
		t.Fatalf("requests: %d %q", code, e.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) != 0 {
		t.Fatal("asked a host that offered no broker")
	}
}

func listing(m agentproto.Message) agentproto.Message {
	return agentproto.Message{OK: true, Cursor: 42, Requests: []agentproto.RequestInfo{
		{ID: 12, Kind: "egress", State: "allowed", Effect: "connect to api.example.com:443\x1b[2J\nnext", Reason: "ok\x07", Session: "s1"},
	}}
}

func runRequests(t *testing.T, socket string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := requestsWith(socket, t.TempDir(), 2, args, &out, &errb, func() int { return 2 })
	return code, out.String(), errb.String()
}

func TestRequestsListing(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), listing)
	code, out, errb := runRequests(t, h.socket, "--all", "--since", "5", "--session", "s1", "--json")
	if code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	if m := h.last(t); m.Op != agentproto.OpBrokerRequests || !m.All || m.Since != 5 || m.Session != "s1" || m.Wait != 0 {
		t.Fatalf("the host saw %+v", m)
	}
	var res requestsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if res.Cursor != 42 || len(res.Requests) != 1 || res.Requests[0].ID != 12 {
		t.Fatalf("%+v", res)
	}
	if strings.ContainsAny(res.Requests[0].Effect+res.Requests[0].Reason, "\x1b\x07\n") {
		t.Fatalf("control characters kept: %+v", res.Requests[0])
	}
	if !strings.Contains(string(out), `"requests":[`) {
		t.Fatalf("shape: %s", out)
	}
	// Human form, and an empty list is still a list in JSON.
	_, out, _ = runRequests(t, h.socket)
	if !strings.Contains(out, "#12  allowed  egress  connect to api.example.com:443[2J next") || strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("human output %q", out)
	}
	e := startBrokerLink(t, true, t.TempDir(), func(agentproto.Message) agentproto.Message { return agentproto.Message{OK: true, Cursor: 3} })
	_, out, _ = runRequests(t, e.socket, "--json")
	if strings.TrimSpace(out) != `{"cursor":3,"requests":[]}` {
		t.Fatalf("empty: %q", out)
	}
}

func TestRequestsSinceNowAndWaitCap(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), listing)
	code, out, _ := runRequests(t, h.socket, "--since", "now", "--wait", "100", "--json")
	if code != 0 || strings.TrimSpace(out) != `{"cursor":42,"requests":[]}` {
		t.Fatalf("%d %q", code, out)
	}
	if m := h.last(t); m.Since != math.MaxUint64 || m.Wait != 0 {
		t.Fatalf("since now asked %+v", m)
	}
	for wait, want := range map[string]int{"9999": agentproto.MaxWait, "-5": 0, "7": 7} {
		if code, _, errb := runRequests(t, h.socket, "--wait", wait); code != 0 {
			t.Fatalf("wait %s: %d %q", wait, code, errb)
		}
		if m := h.last(t); m.Wait != want {
			t.Fatalf("--wait %s reached the host as %d, want %d", wait, m.Wait, want)
		}
	}
	for _, args := range [][]string{{"--since", "x"}, {"--wait", "x"}, {"--wait"}, {"extra"}, {"--session", strings.Repeat("s", 101)}} {
		if code, _, _ := runRequests(t, h.socket, args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// A wait ends when its client goes away, without the host answering.
func TestWaitEndsWithItsClient(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), func(agentproto.Message) agentproto.Message {
		time.Sleep(20 * time.Second)
		return agentproto.Message{OK: true}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := askCtx(ctx, h.socket, agentproto.Message{Op: agentproto.OpBrokerRequests, Wait: 200, Since: 1}, time.Minute)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled wait returned an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client's wait outlived its context")
	}
}

func TestAskTimeout(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), func(agentproto.Message) agentproto.Message {
		time.Sleep(10 * time.Second)
		return agentproto.Message{OK: true}
	})
	start := time.Now()
	_, err := askCtx(context.Background(), h.socket, agentproto.Message{Op: agentproto.OpBrokerRequests, Wait: 1}, 300*time.Millisecond)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("no client-side timeout: %v after %v", err, time.Since(start))
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("a\x1b[0mb\u202ec\nd\x00", 100); got != "a[0mbc d" {
		t.Fatalf("%q", got)
	}
	if got := sanitize(strings.Repeat("é", 400), 300); len([]rune(got)) != 303 {
		t.Fatalf("%d runes", len([]rune(got)))
	}
}

func TestMainKnowsTheCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"request"}, strings.NewReader(""), nopCloser{&out}, &errb); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "request KIND") || !strings.Contains(errb.String(), "requests [--all]") {
		t.Fatalf("usage lacks the commands:\n%s", errb.String())
	}
}

// runRequestsAs runs `requests` as a child of process parent in the fake
// proc root.
func runRequestsAs(t *testing.T, socket, root string, parent int, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := requestsWith(socket, root, parent, args, &out, &errb, func() int { return 2 })
	return code, out.String(), errb.String()
}

// Any process in the sandbox can talk to the link's socket: a wait it asks
// for reaches the host within the cap, whatever it sent.
func TestLinkCapsARawClientsWait(t *testing.T) {
	h := startBrokerLink(t, true, t.TempDir(), func(agentproto.Message) agentproto.Message {
		return agentproto.Message{OK: true}
	})
	for sent, want := range map[int]int{1 << 30: agentproto.MaxWait, -7: 0, 9: 9} {
		if _, err := askCtx(context.Background(), h.socket, agentproto.Message{Op: agentproto.OpBrokerRequests, Wait: sent}, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if m := h.last(t); m.Wait != want {
			t.Errorf("a raw wait of %d reached the host as %d, want %d", sent, m.Wait, want)
		}
	}
}

// The command keeps --wait within the cap itself, before the link does:
// what it sends the socket, read off a bare listener.
func TestRequestsCommandCapsItsWait(t *testing.T) {
	dir, err := os.MkdirTemp("", "cbw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { saferm.RemoveRoot(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan agentproto.Message, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, err := readLine(c, agentproto.MaxPayload)
			var m agentproto.Message
			if err == nil && json.Unmarshal(line, &m) == nil {
				got <- m
			}
			c.Write([]byte(`{"type":"response","ok":true}` + "\n"))
			c.Close()
		}
	}()
	if code, _, errb := runRequests(t, filepath.Join(dir, "s"), "--wait", "99999"); code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	if m := <-got; m.Wait != agentproto.MaxWait {
		t.Fatalf("the command sent a wait of %d, want %d", m.Wait, agentproto.MaxWait)
	}
}
