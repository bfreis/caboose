package agent

import (
	"bytes"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// execPair is a connection to an ExecServer serving the other end, over a
// socketpair as vsock will be.
func execPair(t *testing.T, e *ExecServer) net.Conn {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "socketpair")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	host, guest := conn(fds[0]), conn(fds[1])
	go func() { _ = e.ServeConn(guest) }()
	return host
}

type execResult struct {
	code        int
	out, errOut string
}

func runExec(t *testing.T, e *ExecServer, req agentproto.ExecRequest, stdin string, resize chan [2]uint16) execResult {
	t.Helper()
	var out, errb bytes.Buffer
	stdio := agentproto.ExecIO{Stdout: &out, Stderr: &errb, Resize: resize}
	if stdin != "" {
		stdio.Stdin = strings.NewReader(stdin)
	}
	done := make(chan execResult, 1)
	go func() {
		code, err := agentproto.Exec(execPair(t, e), req, stdio)
		if err != nil {
			t.Error(err)
		}
		done <- execResult{code, out.String(), errb.String()}
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%q did not end", req.Argv)
	}
	return execResult{}
}

func TestExecStreams(t *testing.T) {
	e := &ExecServer{Env: []string{"PATH=" + os.Getenv("PATH"), "FOO=base"}, Dir: "/"}
	r := runExec(t, e, agentproto.ExecRequest{Argv: []string{"sh", "-c", `echo "out $FOO $(pwd)"; echo err >&2; exit 3`},
		Env: []string{"FOO=over"}, Dir: os.TempDir()}, "", nil)
	if r.code != 3 || r.out != "out over "+os.TempDir()+"\n" || r.errOut != "err\n" {
		t.Errorf("%+v", r)
	}
	r = runExec(t, e, agentproto.ExecRequest{Argv: []string{"sh", "-c", "pwd"}}, "", nil)
	if r.out != "/\n" {
		t.Errorf("default dir: %+v", r)
	}
}

// A large stdin goes through whole, and its end is the command's EOF.
func TestExecStdin(t *testing.T) {
	e := &ExecServer{Env: []string{"PATH=" + os.Getenv("PATH")}}
	in := strings.Repeat("0123456789abcdef", 64<<10)
	r := runExec(t, e, agentproto.ExecRequest{Argv: []string{"cat"}, Stdin: true}, in, nil)
	if r.code != 0 || r.out != in {
		t.Errorf("code %d, %d bytes out of %d", r.code, len(r.out), len(in))
	}
}

// A command without stdin sees EOF at once, as under docker exec without -i.
func TestExecNoStdin(t *testing.T) {
	e := &ExecServer{Env: []string{"PATH=" + os.Getenv("PATH")}}
	if r := runExec(t, e, agentproto.ExecRequest{Argv: []string{"cat"}}, "", nil); r.code != 0 || r.out != "" {
		t.Errorf("%+v", r)
	}
}

func TestExecFailures(t *testing.T) {
	e := &ExecServer{Env: []string{"PATH=" + os.Getenv("PATH")}}
	r := runExec(t, e, agentproto.ExecRequest{Argv: []string{"no-such-command-here"}}, "", nil)
	if r.code != agentproto.ExitNotFound || !strings.Contains(r.errOut, "not found") {
		t.Errorf("missing command: %+v", r)
	}
	r = runExec(t, e, agentproto.ExecRequest{Argv: []string{"sh", "-c", "kill -TERM $$"}}, "", nil)
	if r.code != 128+int(syscall.SIGTERM) {
		t.Errorf("killed: %+v", r)
	}
	r = runExec(t, e, agentproto.ExecRequest{Argv: []string{"true"}, User: "agent"}, "", nil)
	if r.code != agentproto.ExitCannotRun || !strings.Contains(r.errOut, "UID") {
		t.Errorf("named user: %+v", r)
	}
}

// With a terminal: its size at the start and after a resize, and the
// command sees a tty on all three streams.
func TestExecTTY(t *testing.T) {
	e := &ExecServer{Env: []string{"PATH=" + os.Getenv("PATH")}}
	resize := make(chan [2]uint16, 1)
	req := agentproto.ExecRequest{
		Argv: []string{"sh", "-c", `stty size; test -t 0 && test -t 1 && test -t 2 && echo tty; read x; stty size; exit 5`},
		TTY:  true, Stdin: true, Rows: 24, Cols: 80,
	}
	pr, pw := io.Pipe()
	out := &syncWriter{}
	done := make(chan int, 1)
	go func() {
		code, err := agentproto.Exec(execPair(t, e), req, agentproto.ExecIO{Stdin: pr, Stdout: out, Resize: resize})
		if err != nil {
			t.Error(err)
		}
		done <- code
	}()
	waitFor(t, out, "tty")
	resize <- [2]uint16{50, 132}
	time.Sleep(200 * time.Millisecond)
	pw.Write([]byte("go\n"))
	select {
	case code := <-done:
		got := strings.ReplaceAll(out.String(), "\r", "")
		if code != 5 || !strings.Contains(got, "24 80\ntty\n") || !strings.Contains(got, "50 132\n") {
			t.Errorf("code %d, output %q", code, got)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("did not end; output %q", out.String())
	}
}

func TestParseUser(t *testing.T) {
	for u, want := range map[string][2]uint32{"0": {0, 0}, "1000:100": {1000, 100}} {
		c, err := parseUser(u)
		if err != nil || c.Uid != want[0] || c.Gid != want[1] {
			t.Errorf("%s: %+v %v", u, c, err)
		}
	}
	for _, u := range []string{"", "root", "1:x", "-1"} {
		if _, err := parseUser(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

// syncWriter is a buffer written by one goroutine and read by another.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func waitFor(t *testing.T, w *syncWriter, s string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(w.String(), s) {
			return
		}
	}
	t.Fatalf("no %q in %q", s, w.String())
}

// A process the command leaves behind on its terminal does not hold the
// exec open after the command ends.
func TestExecTTYLeftBehind(t *testing.T) {
	e := &ExecServer{Env: []string{"PATH=" + os.Getenv("PATH")}}
	start := time.Now()
	r := runExec(t, e, agentproto.ExecRequest{Argv: []string{"sh", "-c", "sleep 30 & echo x"}, TTY: true}, "", nil)
	if r.code != 0 || !strings.Contains(r.out, "x") || time.Since(start) > 5*time.Second {
		t.Errorf("%+v after %v", r, time.Since(start))
	}
}
