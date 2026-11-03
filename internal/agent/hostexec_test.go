package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// A host whose hello offers no host exec is refused here, with what turns
// it on, without asking the host.
func TestHostExecOffHere(t *testing.T) {
	h := startLink(t)
	waitSocket(t, h.socket)
	code, err := HostExec(context.Background(), h.socket, []string{"true"}, "/work", agentproto.ExecIO{})
	if code != agentproto.ExitCannotRun || err == nil || err.Error() != agentproto.HostExecOff {
		t.Fatalf("got %d %v", code, err)
	}
	if !strings.Contains(agentproto.HostExecOff, "host_exec = true") || !strings.Contains(agentproto.HostExecOff, "rereads") {
		t.Errorf("the refusal does not say what turns it on: %s", agentproto.HostExecOff)
	}
}

// With host exec offered, the link asks the host, answers the client, and
// splices it to the host's stream, where the client's exec protocol runs:
// the request as the command line gave it, and the exit status back.
func TestHostExecSpliced(t *testing.T) {
	h := startLinkWith(t, agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, HostExec: true}, procFixture(t))
	waitSocket(t, h.socket)
	got := make(chan agentproto.ExecRequest, 1)
	go func() {
		for b := range h.sess.Control() {
			m, _ := agentproto.Decode(b)
			if m.Type != agentproto.TypeRequest || m.Op != agentproto.OpHostExec {
				continue
			}
			hdr, _ := json.Marshal(agentproto.StreamHeader{Request: m.ID})
			st, err := h.sess.Open(hdr)
			if err != nil {
				return
			}
			h.sess.Send(agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, OK: true})
			go func() {
				s := agentproto.NewSession(st, st, false)
				defer s.Close()
				req, err := agentproto.ReadExec(s, 5*time.Second)
				if err != nil {
					return
				}
				got <- req
				s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
				streams, err := agentproto.AcceptExecStreams(s, req, 5*time.Second)
				if err != nil {
					return
				}
				in, _ := readAll(streams[agentproto.StreamStdin])
				streams[agentproto.StreamStdout].Write(bytes.ToUpper(in))
				streams[agentproto.StreamStdout].CloseWrite()
				streams[agentproto.StreamStderr].CloseWrite()
				s.Send(agentproto.Message{Type: agentproto.TypeExit, Code: 7})
				<-s.Done()
			}()
		}
	}()
	var out bytes.Buffer
	code, err := HostExec(context.Background(), h.socket, []string{"tool", "a b"}, "/work/p",
		agentproto.ExecIO{Stdin: strings.NewReader("shout"), Stdout: &out})
	if err != nil || code != 7 || out.String() != "SHOUT" {
		t.Fatalf("got %d %v %q", code, err, out.String())
	}
	req := <-got
	if strings.Join(req.Argv, "|") != "tool|a b" || req.Dir != "/work/p" || !req.Stdin || req.TTY || req.User != "" || len(req.Env) != 0 {
		t.Fatalf("the host was asked %+v", req)
	}
}

func readAll(st *agentproto.Stream) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(st)
	return b.Bytes(), err
}

// The answer is read up to its newline and no further: what follows is
// the exec protocol's.
func TestReadLineStopsAtTheNewline(t *testing.T) {
	r := strings.NewReader("{\"ok\":true}\nREST")
	line, err := readLine(r, 100)
	if err != nil || string(line) != `{"ok":true}` {
		t.Fatalf("got %q %v", line, err)
	}
	if rest, _ := readAllString(r); rest != "REST" {
		t.Fatalf("left %q", rest)
	}
	if _, err := readLine(strings.NewReader(strings.Repeat("x", 10)), 5); err == nil {
		t.Fatal("an answer over the limit was read")
	}
}

func readAllString(r *strings.Reader) (string, error) {
	b, err := io.ReadAll(r)
	return string(b), err
}

// The command line: a usage error for nothing to run or an unknown flag.
func TestHostUsage(t *testing.T) {
	for _, args := range [][]string{{"host"}, {"host", "-C"}, {"host", "--"}, {"host", "-x", "ls"}, {"host", "--host-dir"}, {"host", "--host-dir", "~"}} {
		var errb bytes.Buffer
		if code := Main(args, strings.NewReader(""), nopCloser{&bytes.Buffer{}}, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
			t.Errorf("%q: %d %q", args, code, errb.String())
		}
	}
}

// --host-dir is checked here before anything is sent, and never together
// with -C.
func TestHostDirFlagRefused(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"host", "-C", "/work", "--host-dir", "~", "ls"}, "cannot be given together"},
		{[]string{"host", "--host-dir", "sub", "ls"}, "takes an absolute path on the host"},
		{[]string{"host", "--host-dir=", "ls"}, "needs a directory on the host"},
		{[]string{"host", "--host-dir", "", "ls"}, "needs a directory on the host"},
	} {
		var errb bytes.Buffer
		if code := Main(tc.args, strings.NewReader(""), nopCloser{&bytes.Buffer{}}, &errb); code != agentproto.ExitCannotRun || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%q: %d %q, want %q", tc.args, code, errb.String(), tc.want)
		}
	}
}

// The host is asked for the host directory as written, and for no sandbox
// directory with it; without the flag the request is as it was, with no
// host_dir at all.
func TestHostExecInSendsHostDir(t *testing.T) {
	for _, hostDir := range []string{"~", "/Users/me/x", ""} {
		h := startLinkWith(t, agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, HostExec: true}, procFixture(t))
		waitSocket(t, h.socket)
		got := make(chan agentproto.ExecRequest, 1)
		go func() {
			for b := range h.sess.Control() {
				m, _ := agentproto.Decode(b)
				if m.Type != agentproto.TypeRequest || m.Op != agentproto.OpHostExec {
					continue
				}
				hdr, _ := json.Marshal(agentproto.StreamHeader{Request: m.ID})
				st, err := h.sess.Open(hdr)
				if err != nil {
					return
				}
				h.sess.Send(agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, OK: true})
				s := agentproto.NewSession(st, st, false)
				defer s.Close()
				req, err := agentproto.ReadExec(s, 5*time.Second)
				if err != nil {
					return
				}
				got <- req
				s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
				streams, err := agentproto.AcceptExecStreams(s, req, 5*time.Second)
				if err != nil {
					return
				}
				streams[agentproto.StreamStdout].CloseWrite()
				streams[agentproto.StreamStderr].CloseWrite()
				s.Send(agentproto.Message{Type: agentproto.TypeExit, Code: 0})
				<-s.Done()
			}
		}()
		dir := "/work/p"
		if hostDir != "" {
			dir = ""
		}
		if code, err := HostExecIn(context.Background(), h.socket, []string{"ls"}, dir, hostDir, agentproto.ExecIO{}); err != nil || code != 0 {
			t.Fatalf("%q: %d %v", hostDir, code, err)
		}
		req := <-got
		if req.HostDir != hostDir || req.Dir != dir {
			t.Errorf("host dir %q: asked %+v", hostDir, req)
		}
		b, _ := json.Marshal(req)
		if hostDir == "" && strings.Contains(string(b), "host_dir") {
			t.Errorf("host_dir sent when unset: %s", b)
		}
	}
}
