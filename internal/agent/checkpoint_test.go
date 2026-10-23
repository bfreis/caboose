package agent

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
)

func runCheckpoint(t *testing.T, socket string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := checkpointWith(socket, args, &out, &errb, func() int { return 2 })
	return code, out.String(), errb.String()
}

// fakeCheckpoints answers as a host does: /work/p is taken, anything else
// is not; "/work/off" is the policy's refusal.
func fakeCheckpoints(m agentproto.Message) agentproto.Message {
	if m.Op != agentproto.OpCheckpoint {
		return agentproto.Message{Error: "unexpected"}
	}
	if slices.Contains(m.Paths, "/work/limit") {
		return agentproto.Message{Reason: agentproto.CheckpointLimit, Error: "at most 10 checkpoint requests an hour"}
	}
	if slices.Contains(m.Paths, "/work/off") {
		return agentproto.Message{Reason: agentproto.CheckpointOff, Error: `checkpoint_requests = "off"`}
	}
	r := agentproto.Message{OK: true}
	paths := m.Paths
	if len(paths) == 0 {
		paths = []string{"/work/p"}
	}
	for _, p := range paths {
		if p == "/work/p" {
			r.Checkpoints = append(r.Checkpoints, agentproto.CheckpointTaken{Path: p, ID: "k@1"})
		} else {
			r.Checkpoints = append(r.Checkpoints, agentproto.CheckpointTaken{Path: p, Reason: "unknown path\x1b[31m"})
		}
	}
	return r
}

func TestCheckpointCommand(t *testing.T) {
	h := startHelloLink(t, agentproto.Message{CheckpointsOn: true}, t.TempDir(), fakeCheckpoints)
	code, out, _ := runCheckpoint(t, h.socket, "/work/x/../p/")
	if code != 0 || strings.TrimSpace(out) != "/work/p: checkpoint k@1" {
		t.Fatalf("taken: %d %q", code, out)
	}
	if p := h.last(t).Paths; !slices.Equal(p, []string{"/work/p"}) {
		t.Fatalf("sent %q", p)
	}
	code, out, _ = runCheckpoint(t, h.socket)
	if code != 0 || len(h.last(t).Paths) != 0 || !strings.Contains(out, "k@1") {
		t.Fatalf("all: %d %q %+v", code, out, h.last(t))
	}
	code, out, _ = runCheckpoint(t, h.socket, "/work/p", "/work/q")
	if code != 1 || !strings.Contains(out, "/work/q: not taken: unknown path") || strings.Contains(out, "\x1b") {
		t.Fatalf("some not taken: %d %q", code, out)
	}
	code, out, _ = runCheckpoint(t, h.socket, "--json", "/work/p")
	var list []agentproto.CheckpointTaken
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != 0 || len(list) != 1 || list[0].ID != "k@1" {
		t.Fatalf("json: %d %q %v", code, out, err)
	}
	if code, _, errb := runCheckpoint(t, h.socket, "/work/limit"); code != ExitCheckpointLimit || !strings.Contains(errb, "an hour") {
		t.Fatalf("limit: %d %q", code, errb)
	}
	code, _, errb := runCheckpoint(t, h.socket, "/work/off")
	if code != ExitCheckpointDenied || !strings.Contains(errb, "checkpoint_requests") {
		t.Fatalf("off: %d %q", code, errb)
	}
}

// The agent refuses what the host would, before asking it: relative
// paths, unprintable ones, too many, too long.
func TestCheckpointCommandRefuses(t *testing.T) {
	h := startHelloLink(t, agentproto.Message{CheckpointsOn: true}, t.TempDir(), fakeCheckpoints)
	many := make([]string, agentproto.MaxCheckpointPaths+1)
	for i := range many {
		many[i] = "/work/p"
	}
	for _, args := range [][]string{
		{"relative"}, {"../etc"}, {"/work/p\x1b[31m"}, {"/work/" + strings.Repeat("a", agentproto.MaxCheckpointPath)}, many, {"--force"},
	} {
		code, _, errb := runCheckpoint(t, h.socket, args...)
		if code != 2 || strings.Contains(errb, "\x1b") {
			t.Errorf("%.40q: %d %q", args, code, errb)
		}
	}
	h.mu.Lock()
	n := len(h.seen)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("the host was asked %d times", n)
	}
	// Past the command, the link checks again.
	r, err := ask(h.socket, agentproto.Message{Op: agentproto.OpCheckpoint, Paths: []string{"x"}})
	if err == nil || r.Reason != agentproto.CheckpointBad {
		t.Fatalf("link: %+v %v", r, err)
	}
}

// A host whose hello offers no checkpoints is never asked, and the agent
// says why.
func TestCheckpointNotOffered(t *testing.T) {
	h := startHelloLink(t, agentproto.Message{}, t.TempDir(), fakeCheckpoints)
	code, _, errb := runCheckpoint(t, h.socket, "/work/p")
	if code != ExitCheckpointUnavailable || !strings.Contains(errb, "takes no checkpoints") {
		t.Fatalf("%d %q", code, errb)
	}
	h.mu.Lock()
	n := len(h.seen)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("the host was asked %d times", n)
	}
}
