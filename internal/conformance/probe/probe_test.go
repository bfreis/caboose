package probe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
)

const nonce = "0123456789abcdef0123456789abcdef"

// socketDir is a short directory of the test's own for sockets (a Unix
// socket's path is bounded), removed by its exact path.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cprobe")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(tmp, real); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Fatalf("socket dir %s is not inside the temp dir %s", real, tmp)
	}
	t.Cleanup(func() {
		if err := saferm.RemoveRoot(dir); err != nil {
			t.Errorf("removing %s: %v", dir, err)
		}
	})
	return dir
}

// work makes a /work the gate accepts, in a temp dir.
func work(t *testing.T) string {
	t.Helper()
	w := filepath.Join(t.TempDir(), "work")
	for _, r := range []string{"conf-01234567-ro", "conf-01234567-dev"} {
		if err := os.MkdirAll(filepath.Join(w, r), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w, r, ".conformance-root"), []byte(nonce+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(w, "conf-01234567-dev", "part"), 0o755); err != nil {
		t.Fatal(err)
	}
	return w
}

// fakeLink serves a link socket the way the sandbox's end of the link
// does (one line in, one line out), answering by answer.
type fakeLink struct {
	path string
	mu   sync.Mutex
	conn int
	ops  []string
}

// answer returns the reply line for one request; "" closes unanswered.
type answer func(m agentproto.Message) string

func ok() string { return `{"type":"response","ok":true}` }
func refuse(e string) string {
	b, _ := json.Marshal(agentproto.Message{Type: "response", Error: e})
	return string(b)
}

// enforcing answers the status itself and refuses the rest as the host
// does an op it does not know.
func enforcing(m agentproto.Message) string {
	if m.Op == Control {
		return ok()
	}
	return refuse("unknown request " + `"` + m.Op + `"`)
}

func newLink(t *testing.T, a answer) *fakeLink {
	t.Helper()
	l := &fakeLink{path: filepath.Join(socketDir(t), "l.sock")}
	ln, err := net.Listen("unix", l.path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			l.conn++
			l.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				line, err := bufio.NewReader(io.LimitReader(c, agentproto.MaxPayload)).ReadBytes('\n')
				if err != nil {
					l.record("<dropped>")
					return
				}
				var m agentproto.Message
				if json.Unmarshal(line, &m) != nil {
					_, _ = c.Write([]byte(refuse("bad request") + "\n"))
					return
				}
				l.record(m.Op)
				if r := a(m); r != "" {
					_, _ = c.Write([]byte(r + "\n"))
				}
			}()
		}
	}()
	return l
}

func (l *fakeLink) record(op string) {
	l.mu.Lock()
	l.ops = append(l.ops, op)
	l.mu.Unlock()
}

func (l *fakeLink) seen() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn, slices.Clone(l.ops)
}

func config(t *testing.T, link string) Config {
	return Config{Work: work(t), Nonce: nonce, LinkSocket: link, Timeout: 3 * time.Second}
}

func byName(out []Outcome) map[string]Outcome {
	m := map[string]Outcome{}
	for _, o := range out {
		m[o.Message] = o
	}
	return m
}

func TestGateAccepts(t *testing.T) {
	if err := Gate(work(t), nonce); err != nil {
		t.Fatal(err)
	}
}

func TestGateRefuses(t *testing.T) {
	ro, dev := "conf-01234567-ro", "conf-01234567-dev"
	cases := map[string]struct {
		nonce string
		edit  func(t *testing.T, w string) string // returns the work dir to pass
	}{
		"short nonce":        {nonce: nonce[:31]},
		"upper-case nonce":   {nonce: strings.ToUpper(nonce)},
		"other nonce":        {nonce: "f" + nonce[1:]},
		"same nonce8, other": {nonce: nonce[:8] + strings.Repeat("0", 24)},
		"relative work": {edit: func(t *testing.T, w string) string {
			rel, _ := filepath.Rel("/", w)
			return rel
		}},
		"unclean work": {edit: func(t *testing.T, w string) string { return w + "/." }},
		"work missing": {edit: func(t *testing.T, w string) string { return w + "-gone" }},
		"work is a symlink": {edit: func(t *testing.T, w string) string {
			l := w + "-link"
			mustDo(t, os.Symlink(w, l))
			return l
		}},
		"an extra entry": {edit: func(t *testing.T, w string) string {
			mustDo(t, os.Mkdir(filepath.Join(w, "dev"), 0o755))
			return w
		}},
		"an extra hidden file": {edit: func(t *testing.T, w string) string {
			mustDo(t, os.WriteFile(filepath.Join(w, ".x"), nil, 0o644))
			return w
		}},
		"ro missing": {edit: func(t *testing.T, w string) string {
			mustDo(t, saferm.RemoveAll(w, filepath.Join(w, ro)))
			return w
		}},
		"plain names": {edit: func(t *testing.T, w string) string {
			w2 := filepath.Join(filepath.Dir(w), "plain")
			for _, r := range []string{"ro", "dev"} {
				mustDo(t, os.MkdirAll(filepath.Join(w2, r, "part"), 0o755))
				mustDo(t, os.WriteFile(filepath.Join(w2, r, ".conformance-root"), []byte(nonce+"\n"), 0o644))
			}
			return w2
		}},
		"a root is a symlink": {edit: func(t *testing.T, w string) string {
			mustDo(t, os.Rename(filepath.Join(w, ro), filepath.Join(filepath.Dir(w), "elsewhere")))
			mustDo(t, os.Symlink(filepath.Join(filepath.Dir(w), "elsewhere"), filepath.Join(w, ro)))
			return w
		}},
		"marker missing": {edit: func(t *testing.T, w string) string {
			mustDo(t, saferm.Remove(w, filepath.Join(w, dev, ".conformance-root")))
			return w
		}},
		"marker is a symlink to a good one": {edit: func(t *testing.T, w string) string {
			good := filepath.Join(filepath.Dir(w), "good")
			mustDo(t, os.WriteFile(good, []byte(nonce+"\n"), 0o644))
			p := filepath.Join(w, ro, ".conformance-root")
			mustDo(t, saferm.Remove(w, p))
			mustDo(t, os.Symlink(good, p))
			return w
		}},
		"marker is a directory": {edit: func(t *testing.T, w string) string {
			p := filepath.Join(w, ro, ".conformance-root")
			mustDo(t, saferm.Remove(w, p))
			mustDo(t, os.Mkdir(p, 0o755))
			return w
		}},
		"marker without newline": {edit: func(t *testing.T, w string) string {
			mustDo(t, os.WriteFile(filepath.Join(w, dev, ".conformance-root"), []byte(nonce), 0o644))
			return w
		}},
		"marker with more": {edit: func(t *testing.T, w string) string {
			mustDo(t, os.WriteFile(filepath.Join(w, dev, ".conformance-root"), []byte(nonce+"\nx\n"), 0o644))
			return w
		}},
		"marker of another nonce": {edit: func(t *testing.T, w string) string {
			mustDo(t, os.WriteFile(filepath.Join(w, ro, ".conformance-root"), []byte("f"+nonce[1:]+"\n"), 0o644))
			return w
		}},
		"part missing": {edit: func(t *testing.T, w string) string {
			mustDo(t, saferm.RemoveAll(w, filepath.Join(w, dev, "part")))
			return w
		}},
		"part is a file": {edit: func(t *testing.T, w string) string {
			p := filepath.Join(w, dev, "part")
			mustDo(t, saferm.RemoveAll(w, p))
			mustDo(t, os.WriteFile(p, nil, 0o644))
			return w
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := work(t)
			n := nonce
			if c.nonce != "" {
				n = c.nonce
			}
			if c.edit != nil {
				w = c.edit(t, w)
			}
			if err := Gate(w, n); err == nil {
				t.Fatal("the gate passed")
			}
			// And nothing is sent: the probe never dials past a refusal.
			l := newLink(t, enforcing)
			out, err := Run(Config{Work: w, Nonce: n, LinkSocket: l.path, Timeout: time.Second})
			if _, isGate := err.(*GateError); !isGate || out != nil {
				t.Fatalf("Run = %v, %v; want a GateError and no outcomes", out, err)
			}
			var stdout, stderr bytes.Buffer
			if code := Main(w, []string{"--nonce", n, "--link", l.path}, &stdout, &stderr); code != ExitGate || stdout.Len() != 0 {
				t.Fatalf("Main = %d, stdout %q; want %d and nothing", code, stdout.String(), ExitGate)
			}
			if conns, _ := l.seen(); conns != 0 {
				t.Fatalf("the link got %d connections past a refused gate", conns)
			}
		})
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestEnforcingHolds(t *testing.T) {
	l := newLink(t, enforcing)
	out, err := Run(config(t, l.path))
	if err != nil {
		t.Fatal(err)
	}
	want := Messages()
	want = want[:len(want)-2] // no daemon socket given: no daemon-hello, no daemon-no-hello
	var got []string
	for _, o := range out {
		got = append(got, o.Message)
		if o.Verdict != Held {
			t.Errorf("%s: %+v, want held", o.Message, o)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if code := Exit(out); code != ExitHeld {
		t.Fatalf("Exit = %d", code)
	}
	if o := byName(out)[Oversize]; o.Outcome != Closed {
		t.Errorf("oversize: %+v, want closed", o)
	}
	// What the link saw: each op once, in order, the oversize line dropped.
	_, ops := l.seen()
	wantOps := []string{"status", "answer", "approve", "grant", "config", "trust", "daemon", "caboose-conformance-unknown", "<dropped>"}
	if !slices.Equal(ops, wantOps) {
		t.Fatalf("the link saw %v, want %v", ops, wantOps)
	}
}

// A host that accepts any one of the messages is reported, and the run
// exits ExitNotHeld: never held.
func TestAcceptedIsNotHeld(t *testing.T) {
	for _, lm := range linkMessages {
		t.Run(lm.name, func(t *testing.T) {
			l := newLink(t, func(m agentproto.Message) string {
				if m.Op == lm.msg.Op {
					return ok()
				}
				return enforcing(m)
			})
			out, err := Run(config(t, l.path))
			if err != nil {
				t.Fatal(err)
			}
			o := byName(out)[lm.name]
			if o.Outcome != Accepted || o.Verdict != NotHeld {
				t.Fatalf("%+v, want accepted, not held", o)
			}
			if code := Exit(out); code != ExitNotHeld {
				t.Fatalf("Exit = %d, want %d", code, ExitNotHeld)
			}
			var stdout, stderr bytes.Buffer
			w := work(t)
			if code := Main(w, []string{"--nonce", nonce, "--link", l.path, "--timeout", "3", "--interval", "0"}, &stdout, &stderr); code != ExitNotHeld {
				t.Fatalf("Main = %d, %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), `"message":"`+lm.name+`","outcome":"accepted","verdict":"not-held"`) {
				t.Fatalf("stdout does not report it:\n%s", stdout.String())
			}
		})
	}
}

// A link that reads the whole oversize line and answers it has not held,
// refused or not answered differently.
func TestOversize(t *testing.T) {
	serve := func(t *testing.T, reply string) string {
		path := filepath.Join(socketDir(t), "o.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		t.Cleanup(func() { ln.Close(); <-done })
		go func() {
			defer close(done)
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := bufio.NewReaderSize(c, 2*agentproto.MaxPayload).ReadBytes('\n'); err != nil {
				return
			}
			_, _ = c.Write([]byte(reply + "\n"))
		}()
		return path
	}
	for name, c := range map[string]struct {
		reply   string
		outcome string
		verdict string
	}{
		"accepted": {ok(), Accepted, NotHeld},
		"refused":  {refuse("too long"), Refused, Held},
		"no host":  {refuse("the host did not answer"), NoHost, Unproven},
	} {
		t.Run(name, func(t *testing.T) {
			o := oversize(serve(t, c.reply), 3*time.Second)
			if o.Outcome != c.outcome || o.Verdict != c.verdict {
				t.Fatalf("%+v, want %s %s", o, c.outcome, c.verdict)
			}
		})
	}
}

// Answers the sandbox's end of the link gives for itself prove nothing.
func TestNoHostIsUnproven(t *testing.T) {
	for _, e := range noHost {
		t.Run(e, func(t *testing.T) {
			l := newLink(t, func(m agentproto.Message) string {
				if m.Op == Control {
					return ok()
				}
				return refuse(e + ": details")
			})
			out, err := Run(config(t, l.path))
			if err != nil {
				t.Fatal(err)
			}
			if o := byName(out)["answer"]; o.Outcome != NoHost || o.Verdict != Unproven {
				t.Fatalf("%+v", o)
			}
			if code := Exit(out); code != ExitError {
				t.Fatalf("Exit = %d, want %d", code, ExitError)
			}
		})
	}
}

// Answers that say nothing, or not JSON, or no answer in time: unproven.
func TestBadAnswersAreUnproven(t *testing.T) {
	for name, reply := range map[string]string{
		"empty refusal": `{"type":"response"}`,
		"not json":      `ok`,
		"no answer":     "",
	} {
		t.Run(name, func(t *testing.T) {
			l := newLink(t, func(m agentproto.Message) string {
				if m.Op == Control {
					return ok()
				}
				return reply
			})
			c := config(t, l.path)
			c.Only = []string{"grant"}
			out, err := Run(c)
			if err != nil {
				t.Fatal(err)
			}
			if o := byName(out)["grant"]; o.Verdict != Unproven {
				t.Fatalf("%+v, want unproven", o)
			}
			if code := Exit(out); code != ExitError {
				t.Fatalf("Exit = %d", code)
			}
		})
	}
}

// A peer that never answers is bounded by the timeout.
func TestSilentPeerTimesOut(t *testing.T) {
	path := filepath.Join(socketDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	var mu sync.Mutex
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	start := time.Now()
	o := control(path, 300*time.Millisecond)
	if o.Verdict != Unproven || o.Outcome != Failed {
		t.Fatalf("%+v", o)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

// When the positive control fails nothing else is sent, and the run proves
// nothing: never held.
func TestControlFailureSkipsTheRest(t *testing.T) {
	for name, a := range map[string]answer{
		"refused": func(agentproto.Message) string { return refuse("unknown request") },
		"no host": func(agentproto.Message) string { return refuse("the host link is down") },
	} {
		t.Run(name, func(t *testing.T) {
			l := newLink(t, a)
			out, err := Run(config(t, l.path))
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range out[1:] {
				if o.Outcome != Skipped || o.Verdict != Unproven {
					t.Errorf("%+v, want skipped", o)
				}
			}
			if code := Exit(out); code != ExitError {
				t.Fatalf("Exit = %d, want %d", code, ExitError)
			}
			if _, ops := l.seen(); !slices.Equal(ops, []string{"status"}) {
				t.Fatalf("the link saw %v", ops)
			}
		})
	}
}

func TestNoLinkIsUnproven(t *testing.T) {
	c := config(t, filepath.Join(socketDir(t), "none.sock"))
	out, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Outcome != Unreachable || out[0].Verdict != Unproven {
		t.Fatalf("%+v", out[0])
	}
	if code := Exit(out); code != ExitError {
		t.Fatalf("Exit = %d", code)
	}
}

// staleSocket is a socket file no one listens on any more: a dial is
// refused (ECONNREFUSED).
func staleSocket(t *testing.T) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "stale.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the stale socket is not there: %v", err)
	}
	return path
}

// plainFile is a regular file where a socket was expected: ECONNREFUSED
// on Linux, but no socket.
func plainFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "plain.sock")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeDaemon answers a hello with refuse (a refusal) or a daemon hello.
func fakeDaemon(t *testing.T, accept bool) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { ln.Close(); <-done })
	go func() {
		defer close(done)
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		c := daemonproto.NewConn(nc)
		defer c.Close()
		if accept {
			_, _, _ = daemonproto.ServerHandshake(c, "test", 3*time.Second)
			return
		}
		if _, err := c.Read(3 * time.Second); err != nil {
			return
		}
		_ = c.Write(daemonproto.Failure(0, daemonproto.NewError(daemonproto.KindBadRequest, "not this user")), 3*time.Second)
	}()
	return path
}

// rawServer answers whatever it is sent with reply, then closes.
func rawServer(reply string) func(t *testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(socketDir(t), "r.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		t.Cleanup(func() { ln.Close(); <-done })
		go func() {
			defer close(done)
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			defer nc.Close()
			_ = nc.SetDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 512)
			_, _ = nc.Read(buf)
			_, _ = nc.Write([]byte(reply))
		}()
		return path
	}
}

func TestDaemonHello(t *testing.T) {
	for name, c := range map[string]struct {
		socket  func(t *testing.T) string
		outcome string
		verdict string
	}{
		// Only a socket that is there and refuses holds: a path that is
		// gone or no socket (a mistyped one) proves nothing.
		"no socket":    {func(t *testing.T) string { return filepath.Join(socketDir(t), "none.sock") }, Unreachable, Unproven},
		"plain file":   {plainFile, Unreachable, Unproven},
		"a directory":  {socketDir, Unreachable, Unproven},
		"stale socket": {staleSocket, Unreachable, Held},
		"refused":      {func(t *testing.T) string { return fakeDaemon(t, false) }, Refused, NotHeld},
		"accepted":     {func(t *testing.T) string { return fakeDaemon(t, true) }, Accepted, NotHeld},
		// A listener of another protocol (dockerd's HTTP) is not the
		// daemon; anything that answers as JSON may be, whatever it says.
		"http":          {rawServer("HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain\r\n\r\n"), Foreign, Held},
		"bad json":      {rawServer(`{"type":"nonsense"}` + "\n"), Closed, NotHeld},
		"closed silent": {rawServer(""), Closed, NotHeld},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config(t, filepath.Join(socketDir(t), "unused.sock"))
			cfg.DaemonSocket = c.socket(t)
			cfg.Only = []string{DaemonHello}
			out, err := Run(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 || out[0].Outcome != c.outcome || out[0].Verdict != c.verdict {
				t.Fatalf("%+v, want %s %s", out, c.outcome, c.verdict)
			}
			want := ExitHeld
			switch c.verdict {
			case NotHeld:
				want = ExitNotHeld
			case Unproven:
				want = ExitError
			}
			if code := Exit(out); code != want {
				t.Fatalf("Exit = %d, want %d", code, want)
			}
		})
	}
}

// The witness's message: a request before the hello. A close or silence
// holds (the host's log judges it), another protocol holds, anything in
// the daemon's protocol does not; and the daemon gets no hello first.
func TestDaemonNoHello(t *testing.T) {
	for name, c := range map[string]struct {
		socket  func(t *testing.T) string
		outcome string
		verdict string
	}{
		"stale socket":  {staleSocket, Unreachable, Held},
		"no socket":     {func(t *testing.T) string { return filepath.Join(socketDir(t), "none.sock") }, Unreachable, Unproven},
		"refused":       {func(t *testing.T) string { return fakeDaemon(t, false) }, Refused, NotHeld},
		"http":          {rawServer("HTTP/1.1 400 Bad Request\r\n\r\n"), Foreign, Held},
		"closed silent": {rawServer(""), Closed, Held},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config(t, filepath.Join(socketDir(t), "unused.sock"))
			cfg.DaemonSocket = c.socket(t)
			cfg.Only = []string{DaemonNoHello}
			out, err := Run(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 || out[0].Message != DaemonNoHello || out[0].Outcome != c.outcome || out[0].Verdict != c.verdict {
				t.Fatalf("%+v, want %s %s", out, c.outcome, c.verdict)
			}
		})
	}
	// What the daemon reads first is a request, not a hello.
	dir := socketDir(t)
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			got <- err.Error()
			return
		}
		defer nc.Close()
		f, err := daemonproto.NewReader(bufio.NewReader(nc)).Read()
		if err != nil {
			got <- err.Error()
			return
		}
		got <- f.Type
	}()
	cfg := config(t, filepath.Join(dir, "unused.sock"))
	cfg.DaemonSocket, cfg.Only = path, []string{DaemonNoHello}
	if _, err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	if typ := <-got; typ == daemonproto.TypeHello {
		t.Fatalf("the first frame was a hello")
	}
}

// With a daemon socket the whole list is sent, daemon-hello last.
func TestFullListWithDaemon(t *testing.T) {
	l := newLink(t, enforcing)
	c := config(t, l.path)
	c.DaemonSocket = staleSocket(t)
	out, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range out {
		got = append(got, o.Message)
	}
	if !slices.Equal(got, Messages()) || Exit(out) != ExitHeld {
		t.Fatalf("%+v", out)
	}
}

func TestPlan(t *testing.T) {
	for name, c := range map[string]struct {
		only   []string
		daemon string
		want   []string
		bad    bool
	}{
		"one link message brings the control": {only: []string{"trust"}, want: []string{"status", "trust"}},
		"order is fixed, duplicates once":     {only: []string{"oversize", "answer", "answer"}, want: []string{"status", "answer", "oversize"}},
		"daemon alone":                        {only: []string{DaemonHello}, daemon: "/d.sock", want: []string{DaemonHello}},
		"daemon without socket":               {only: []string{DaemonHello}, bad: true},
		"no-hello alone":                      {only: []string{DaemonNoHello}, daemon: "/d.sock", want: []string{DaemonNoHello}},
		"no-hello without socket":             {only: []string{DaemonNoHello}, bad: true},
		"unknown name":                        {only: []string{"rm"}, bad: true},
		"empty name":                          {only: []string{""}, bad: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Config{Only: c.only, DaemonSocket: c.daemon}.plan()
			if (err != nil) != c.bad || (!c.bad && !slices.Equal(got, c.want)) {
				t.Fatalf("plan = %v, %v; want %v (bad %v)", got, err, c.want, c.bad)
			}
		})
	}
}

// The command line has no default socket: without --link nothing runs.
func TestMainUsage(t *testing.T) {
	w := work(t)
	l := newLink(t, enforcing)
	for name, args := range map[string][]string{
		"nothing":            nil,
		"no link":            {"--nonce", nonce},
		"no nonce":           {"--link", l.path},
		"empty link":         {"--nonce", nonce, "--link", ""},
		"relative link":      {"--nonce", nonce, "--link", "l.sock"},
		"long link":          {"--nonce", nonce, "--link", "/" + strings.Repeat("x", 120)},
		"relative daemon":    {"--nonce", nonce, "--link", l.path, "--daemon", "d.sock"},
		"unknown flag":       {"--nonce", nonce, "--link", l.path, "--force", "1"},
		"dangling flag":      {"--nonce", nonce, "--link"},
		"bad timeout":        {"--nonce", nonce, "--link", l.path, "--timeout", "0"},
		"big timeout":        {"--nonce", nonce, "--link", l.path, "--timeout", "601"},
		"odd timeout":        {"--nonce", nonce, "--link", l.path, "--timeout", "1s"},
		"negative interval":  {"--nonce", nonce, "--link", l.path, "--interval", "-1"},
		"big interval":       {"--nonce", nonce, "--link", l.path, "--interval", "10001"},
		"unknown message":    {"--nonce", nonce, "--link", l.path, "--only", "status,rm"},
		"daemon-hello alone": {"--nonce", nonce, "--link", l.path, "--only", DaemonHello},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Main(w, args, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 {
				t.Fatalf("Main = %d, stdout %q", code, stdout.String())
			}
		})
	}
	if conns, _ := l.seen(); conns != 0 {
		t.Fatalf("the link got %d connections from bad command lines", conns)
	}
}

func TestMainHeld(t *testing.T) {
	l := newLink(t, enforcing)
	var stdout, stderr bytes.Buffer
	if code := Main(work(t), []string{"--nonce", nonce, "--link", l.path, "--timeout", "3", "--interval", "0"}, &stdout, &stderr); code != ExitHeld {
		t.Fatalf("Main = %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(Messages())-2 {
		t.Fatalf("%d lines:\n%s", len(lines), stdout.String())
	}
	for _, ln := range lines {
		var o Outcome
		if err := json.Unmarshal([]byte(ln), &o); err != nil || o.Verdict != Held {
			t.Fatalf("line %q: %v", ln, err)
		}
	}
}

func TestExit(t *testing.T) {
	h := Outcome{Verdict: Held}
	n := Outcome{Verdict: NotHeld}
	u := Outcome{Verdict: Unproven}
	for _, c := range []struct {
		out  []Outcome
		want int
	}{
		{nil, ExitError},
		{[]Outcome{h, h}, ExitHeld},
		{[]Outcome{h, u}, ExitError},
		{[]Outcome{u, n}, ExitNotHeld},
		{[]Outcome{n, u}, ExitNotHeld},
		{[]Outcome{{}}, ExitError},
	} {
		if got := Exit(c.out); got != c.want {
			t.Errorf("Exit(%+v) = %d, want %d", c.out, got, c.want)
		}
	}
}

func TestClip(t *testing.T) {
	got := clip("a\x1b[31m\nb\u202e" + strings.Repeat("x", 300))
	if len(got) > 200 || strings.ContainsAny(got, "\x1b\n\u202e") {
		t.Fatalf("%q", got)
	}
}

// limiter refuses as the host's rate limit does: burst requests, then one
// per every.
func limiter(burst int, every time.Duration) answer {
	var mu sync.Mutex
	tokens, last := float64(burst), time.Now()
	return func(m agentproto.Message) string {
		if m.Op == Control {
			return ok() // answered in the sandbox, never limited
		}
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		tokens = min(float64(burst), tokens+float64(now.Sub(last))/float64(every))
		last = now
		if tokens < 1 {
			return refuse("too many requests; try again shortly")
		}
		tokens--
		return enforcing(m)
	}
}

// A refusal for coming too fast proves nothing of the op.
func TestRateLimitedIsUnproven(t *testing.T) {
	l := newLink(t, limiter(5, time.Hour))
	out, err := Run(config(t, l.path))
	if err != nil {
		t.Fatal(err)
	}
	limited := 0
	for _, o := range out {
		if o.Outcome == RateLimited {
			limited++
			if o.Verdict != Unproven {
				t.Errorf("%+v", o)
			}
		}
	}
	if limited == 0 {
		t.Fatalf("no request was limited: %+v", out)
	}
	if code := Exit(out); code != ExitError {
		t.Fatalf("Exit = %d, want %d", code, ExitError)
	}
}

// The interval keeps the requests under the limit.
func TestIntervalKeepsUnderTheLimit(t *testing.T) {
	l := newLink(t, limiter(1, 40*time.Millisecond))
	c := config(t, l.path)
	c.Interval = 60 * time.Millisecond
	out, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	if code := Exit(out); code != ExitHeld {
		t.Fatalf("Exit = %d: %+v", code, out)
	}
}

// The answers the probe reads as not the host's are the ones the link and
// the host give: a renamed one would turn into a refusal, and a pass.
func TestLocalAnswersAreTheProducts(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	link := read(filepath.Join("..", "..", "agent", "link.go"))
	for _, e := range noHost {
		if !strings.Contains(link, `"`+e) {
			t.Errorf("the link's source has no answer %q", e)
		}
	}
	if host := read(filepath.Join("..", "..", "hostlink", "host.go")); !strings.Contains(host, `"`+rateLimited) {
		t.Errorf("the host's source has no answer %q", rateLimited)
	}
}

// A dial's failure holds only for a socket that is there and refused it.
func TestUnreachableNeedsASocket(t *testing.T) {
	stale := staleSocket(t)
	missing := filepath.Join(socketDir(t), "gone.sock")
	refused := &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for name, c := range map[string]struct {
		path string
		err  error
		want string
	}{
		"refused on a socket":       {stale, refused, Held},
		"permission on a socket":    {stale, syscall.EACCES, Held},
		"refused where none is":     {missing, refused, Unproven},
		"refused on a plain file":   {plainFile(t), refused, Unproven},
		"no such file":              {missing, syscall.ENOENT, Unproven},
		"not a socket":              {stale, syscall.ENOTSOCK, Unproven},
		"timed out on a socket":     {stale, syscall.ETIMEDOUT, Unproven},
		"anything else on a socket": {stale, errors.New("odd"), Unproven},
	} {
		if o := unreachable(c.path, c.err); o.Verdict != c.want || o.Outcome != Unreachable {
			t.Errorf("%s: %+v, want %s", name, o, c.want)
		}
	}
}
