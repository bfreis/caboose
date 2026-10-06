package hostlink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
)

// execAgent is the agent's side of a link to a real Host, as far as host
// exec needs one: it asks, takes the stream the host opens, and runs the
// exec protocol's client on it. Portable: no /proc, no agent.
type execAgent struct {
	t     *testing.T
	sess  *agentproto.Session
	hello agentproto.Message

	mu      sync.Mutex
	nextID  uint64
	answers map[uint64]chan agentproto.Message
	streams map[uint64]chan *agentproto.Stream
}

// startExecHost runs a Host with cfg against an execAgent.
func startExecHost(t *testing.T, cfg Config) *execAgent {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	hs := agentproto.NewSession(hr, hw, true)
	as := agentproto.NewSession(ar, aw, false)
	t.Cleanup(func() { hs.Close(); as.Close() })
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	go Run(hs, cfg)
	if err := as.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
		t.Fatal(err)
	}
	a := &execAgent{t: t, sess: as, answers: map[uint64]chan agentproto.Message{}, streams: map[uint64]chan *agentproto.Stream{}}
	select {
	case b := <-as.Control():
		m, err := agentproto.Decode(b)
		if err != nil || m.Type != agentproto.TypeHello {
			t.Fatalf("first message %s, want a hello", b)
		}
		a.hello = m
	case <-time.After(5 * time.Second):
		t.Fatal("no hello from the host")
	}
	go func() {
		for b := range as.Control() {
			m, _ := agentproto.Decode(b)
			if m.Type == agentproto.TypeResponse {
				a.chans(m.ID).answer <- m
			}
		}
	}()
	go func() {
		for {
			st, err := as.Accept()
			if err != nil {
				return
			}
			var h agentproto.StreamHeader
			_ = json.Unmarshal(st.Header(), &h)
			a.chans(h.Request).stream <- st
		}
	}()
	return a
}

type execChans struct {
	answer chan agentproto.Message
	stream chan *agentproto.Stream
}

func (a *execAgent) chans(id uint64) execChans {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.answers[id] == nil {
		a.answers[id] = make(chan agentproto.Message, 1)
		a.streams[id] = make(chan *agentproto.Stream, 1)
	}
	return execChans{a.answers[id], a.streams[id]}
}

// open asks for a host exec, and returns the host's stream for it, or its
// refusal.
func (a *execAgent) open() (*agentproto.Stream, error) {
	a.t.Helper()
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	a.mu.Unlock()
	c := a.chans(id)
	if err := a.sess.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: id, Op: agentproto.OpHostExec}); err != nil {
		a.t.Fatal(err)
	}
	var r agentproto.Message
	select {
	case r = <-c.answer:
	case <-time.After(5 * time.Second):
		a.t.Fatal("no answer to a host exec")
	}
	if !r.OK {
		return nil, errors.New(r.Error)
	}
	select {
	case st := <-c.stream:
		return st, nil
	case <-time.After(5 * time.Second):
		a.t.Fatal("the host said yes, but opened no stream")
	}
	return nil, nil
}

type execResult struct {
	code           int
	stdout, stderr string
}

// run runs req on the host, with stdin, and returns how it went.
func (a *execAgent) run(req agentproto.ExecRequest, stdin string) execResult {
	a.t.Helper()
	st, err := a.open()
	if err != nil {
		a.t.Fatalf("host exec refused: %v", err)
	}
	var out, errs bytes.Buffer
	if stdin != "" {
		req.Stdin = true
	}
	code, err := agentproto.Exec(st, req, agentproto.ExecIO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errs})
	if err != nil {
		a.t.Fatalf("exec %v: %v", req.Argv, err)
	}
	return execResult{code, out.String(), errs.String()}
}

// execRoot is a host directory mounted at /work, with a sub directory.
func execRoot(t *testing.T) (string, Config) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, Config{HostExec: &HostExec{Roots: []config.Root{{Host: dir, Container: "/work"}}}}
}

func TestHostExecRuns(t *testing.T) {
	dir, cfg := execRoot(t)
	var logged bytes.Buffer
	cfg.Log = log.New(&syncWriter{w: &logged}, "", 0)
	a := startExecHost(t, cfg)
	if !a.hello.HostExec {
		t.Fatal("the hello does not offer host exec")
	}
	r := a.run(agentproto.ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, Dir: "/work"}, "")
	if r.code != 3 || r.stdout != "out\n" || r.stderr != "err\n" {
		t.Errorf("got %+v, want 3, out, err", r)
	}
	r = a.run(agentproto.ExecRequest{Argv: []string{"cat"}, Dir: "/work"}, "through stdin")
	if r.code != 0 || r.stdout != "through stdin" {
		t.Errorf("cat: %+v", r)
	}
	// The host directory of the container one, with a ".." cleaned away.
	r = a.run(agentproto.ExecRequest{Argv: []string{"sh", "-c", "pwd -P"}, Dir: "/work/x/../sub"}, "")
	if want := filepath.Join(dir, "sub") + "\n"; r.code != 0 || r.stdout != want {
		t.Errorf("pwd: %+v, want %q", r, want)
	}
	// A path with a slash is the request's, relative to the directory.
	if err := os.WriteFile(filepath.Join(dir, "sub", "tool"), []byte("#!/bin/sh\necho tool \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r = a.run(agentproto.ExecRequest{Argv: []string{"./tool", "a b"}, Dir: "/work/sub"}, "")
	if r.code != 0 || r.stdout != "tool a b\n" {
		t.Errorf("./tool: %+v", r)
	}
	r = a.run(agentproto.ExecRequest{Argv: []string{"caboose-no-such-command"}, Dir: "/work"}, "")
	if r.code != agentproto.ExitNotFound || !strings.Contains(r.stderr, "not found in the host's PATH") {
		t.Errorf("a missing command: %+v", r)
	}
	r = a.run(agentproto.ExecRequest{Argv: []string{"sh", "-c", "kill -TERM $$"}, Dir: "/work"}, "")
	if r.code != 128+int(syscall.SIGTERM) {
		t.Errorf("a command killed by SIGTERM: %+v, want %d", r, 128+int(syscall.SIGTERM))
	}
	if !strings.Contains(logged.String(), "host exec sh -c echo out; echo err >&2; exit 3 in "+dir+": exit 3 after") {
		t.Errorf("the log does not say what ran:\n%s", logged.String())
	}
}

// syncWriter is a writer safe for the log's concurrent lines.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestHostExecRefuses(t *testing.T) {
	dir, cfg := execRoot(t)
	a := startExecHost(t, cfg)
	for _, c := range []struct {
		name string
		req  agentproto.ExecRequest
		want string
	}{
		{"tty", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "/work", TTY: true}, "no terminal"},
		{"user", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "/work", User: "0"}, "takes no other"},
		{"env", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "/work", Env: []string{"A=b"}}, "takes no variables"},
		{"relative dir", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "work"}, "not under a root"},
		{"no dir", agentproto.ExecRequest{Argv: []string{"true"}}, "not under a root"},
		{"dir outside the roots", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "/etc"}, "not under a root this machine shares with the sandbox (those are /work)"},
		{"a prefix that is no parent", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "/workx"}, "not under a root"},
		{"climbing out", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "/work/../etc"}, "not under a root"},
		{"missing dir", agentproto.ExecRequest{Argv: []string{"true"}, Dir: "/work/gone"}, "no directory on the host"},
		{"empty command", agentproto.ExecRequest{Argv: []string{""}, Dir: "/work"}, "empty command"},
		{"NUL", agentproto.ExecRequest{Argv: []string{"echo", "a\x00b"}, Dir: "/work"}, "NUL"},
		{"too many arguments", agentproto.ExecRequest{Argv: make([]string, maxHostExecArgs+1), Dir: "/work"}, "arguments"},
	} {
		if c.name == "too many arguments" {
			for i := range c.req.Argv {
				c.req.Argv[i] = "x"
			}
		}
		st, err := a.open()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var out, errs bytes.Buffer
		code, err := agentproto.Exec(st, c.req, agentproto.ExecIO{Stdout: &out, Stderr: &errs})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if code != agentproto.ExitCannotRun || !strings.Contains(errs.String(), c.want) || out.Len() != 0 {
			t.Errorf("%s: %d, %q, %q; want %d and %q", c.name, code, out.String(), errs.String(), agentproto.ExitCannotRun, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "gone")); err == nil {
		t.Fatal("the missing directory is there")
	}
}

func TestHostExecOff(t *testing.T) {
	a := startExecHost(t, Config{})
	if a.hello.HostExec {
		t.Error("a host without host_exec offers it")
	}
	if _, err := a.open(); err == nil || err.Error() != agentproto.HostExecOff {
		t.Fatalf("got %v, want the refusal saying host_exec", err)
	}
}

// gone waits until none of pids is running: no such process, or one that
// is only a zombie its new parent has not reaped yet.
func gone(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range pids {
		for {
			out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
			st := strings.TrimSpace(string(out))
			if err != nil || st == "" || strings.HasPrefix(st, "Z") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still running (%s)", pid, st)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// startSleepers runs a command that leaves two sleeps in its group, and
// returns their PIDs, the stream it runs on and its result, once it ends.
func startSleepers(t *testing.T, a *execAgent) ([]int, *agentproto.Stream, <-chan int) {
	t.Helper()
	st, err := a.open()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		code, _ := agentproto.Exec(st, agentproto.ExecRequest{
			Argv: []string{"sh", "-c", "sleep 30 & echo $!; sleep 30 & echo $!; echo $$; wait"}, Dir: "/work"},
			agentproto.ExecIO{Stdout: pw, Stderr: io.Discard})
		pw.Close()
		done <- code
	}()
	var pids []int
	sc := bufio.NewScanner(pr)
	for len(pids) < 3 && sc.Scan() {
		pid, err := strconv.Atoi(sc.Text())
		if err != nil {
			t.Fatalf("not a PID: %q", sc.Text())
		}
		pids = append(pids, pid)
	}
	if len(pids) < 3 {
		t.Fatal("the command did not say its PIDs")
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	return pids, st, done
}

// A client that goes away takes its command with it, and what that
// started.
func TestHostExecClientGoneKillsGroup(t *testing.T) {
	_, cfg := execRoot(t)
	a := startExecHost(t, cfg)
	pids, st, done := startSleepers(t, a)
	st.Close()
	gone(t, pids)
	<-done
}

// The link's own end, at a signal, ends every command.
func TestKillHostExecs(t *testing.T) {
	_, cfg := execRoot(t)
	a := startExecHost(t, cfg)
	pids, _, done := startSleepers(t, a)
	KillHostExecs()
	gone(t, pids)
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Errorf("exit %d, want %d", code, 128+int(syscall.SIGTERM))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit status")
	}
}

func TestHostExecLimit(t *testing.T) {
	_, cfg := execRoot(t)
	a := startExecHost(t, cfg)
	var streams []*agentproto.Stream
	for range maxHostExec {
		st, err := a.open()
		if err != nil {
			t.Fatalf("one of the first %d: %v", maxHostExec, err)
		}
		streams = append(streams, st)
	}
	if _, err := a.open(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("already %d commands running", maxHostExec)) {
		t.Fatalf("one over the limit: %v", err)
	}
	// One ended frees its slot.
	streams[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := a.open()
		if err == nil {
			streams = append(streams, st)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slot freed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, st := range streams {
		st.Close()
	}
}

// Through a real agent link: caboose-agent host's own client, which
// speaks to the agent's socket.
func TestHostExecThroughAgent(t *testing.T) {
	dir, cfg := execRoot(t)
	socket := linked(t, cfg)
	var out, errs bytes.Buffer
	code, err := agent.HostExec(context.Background(), socket, []string{"sh", "-c", "pwd -P; cat; echo e >&2; exit 5"}, "/work/sub",
		agentproto.ExecIO{Stdin: strings.NewReader("in\n"), Stdout: &out, Stderr: &errs})
	if err != nil || code != 5 || out.String() != filepath.Join(dir, "sub")+"\nin\n" || errs.String() != "e\n" {
		t.Fatalf("got %d %v, %q, %q", code, err, out.String(), errs.String())
	}

	off := linked(t, Config{})
	code, err = agent.HostExec(context.Background(), off, []string{"true"}, "/work", agentproto.ExecIO{})
	if code != agentproto.ExitCannotRun || err == nil || err.Error() != agentproto.HostExecOff {
		t.Fatalf("with host_exec off: %d %v", code, err)
	}
}
