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
	if !strings.Contains(agentproto.HostExecOff, "host_exec = true") || !strings.Contains(agentproto.HostExecOff, "caboose link --restart") {
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
	for _, args := range [][]string{{"host"}, {"host", "-C"}, {"host", "--"}, {"host", "-x", "ls"}} {
		var errb bytes.Buffer
		if code := Main(args, strings.NewReader(""), nopCloser{&bytes.Buffer{}}, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
			t.Errorf("%q: %d %q", args, code, errb.String())
		}
	}
}
