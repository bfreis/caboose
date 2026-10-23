package hostlink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// fakeCheckpointer is a schedule's take: it records what it is asked
// for, and names each checkpoint after its path and a count.
type fakeCheckpointer struct {
	mu     sync.Mutex
	paths  []string
	err    error
	asked  [][]string
	reason string // when set, every take fails with it
	// begun, when set, is told each take as it begins, which then waits
	// for release.
	begun   chan struct{}
	release chan struct{}
}

func (f *fakeCheckpointer) CheckpointPaths() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths), f.err
}

func (f *fakeCheckpointer) TakeCheckpoints(_ context.Context, paths []string) []agentproto.CheckpointTaken {
	f.mu.Lock()
	begun, release := f.begun, f.release
	f.mu.Unlock()
	if begun != nil {
		begun <- struct{}{}
		<-release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slices.Clone(paths))
	var out []agentproto.CheckpointTaken
	for _, p := range paths {
		if f.reason != "" {
			out = append(out, agentproto.CheckpointTaken{Path: p, Reason: f.reason})
			continue
		}
		out = append(out, agentproto.CheckpointTaken{Path: p, ID: fmt.Sprintf("k%d@%d", len(f.asked), len(out))})
	}
	return out
}

func (f *fakeCheckpointer) takes() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// cpClock is a clock that moves only when told.
type cpClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *cpClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *cpClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var cpT0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func cpConfig(f *fakeCheckpointer, c *cpClock) Config {
	return Config{Ports: PortSet{}, Checkpoints: f, CheckpointLimits: NewCheckpointLimits(c.now),
		CheckpointRequests: config.CheckpointRequestsAllow}
}

// The whole way: caboose-agent's client asks the agent's link, which asks
// the host, which takes the checkpoint through its Checkpointer and
// answers with its ID; no path is every configured one.
func TestCheckpointEndToEnd(t *testing.T) {
	f := &fakeCheckpointer{paths: []string{"/work/p", "~/.claude"}}
	clock := &cpClock{t: cpT0}
	socket := linked(t, cpConfig(f, clock))
	got, err := agent.Checkpoint(socket, []string{"/work/p"})
	if err != nil || len(got) != 1 || got[0].Path != "/work/p" || got[0].ID != "k1@0" {
		t.Fatalf("Checkpoint = %+v, %v", got, err)
	}
	clock.add(CheckpointPathEvery)
	got, err = agent.Checkpoint(socket, nil)
	if err != nil || len(got) != 2 || got[0].ID == "" || got[1].Path != "~/.claude" || got[1].ID == "" {
		t.Fatalf("Checkpoint of all = %+v, %v", got, err)
	}
	// /home/agent/... is the same path as ~/...: within the minute, it
	// is limited.
	got, err = agent.Checkpoint(socket, []string{"/home/agent/.claude"})
	if err != nil || len(got) != 1 || got[0].ID != "" || !strings.Contains(got[0].Reason, "less than") {
		t.Fatalf("Checkpoint again = %+v, %v", got, err)
	}
	if takes := f.takes(); !slices.EqualFunc(takes, [][]string{{"/work/p"}, {"/work/p", "~/.claude"}}, slices.Equal) {
		t.Fatalf("takes %v", takes)
	}
}

// A host that offers none says so in its hello, and the agent says so
// without asking it.
func TestCheckpointNotOffered(t *testing.T) {
	if a := startExecHost(t, Config{}); a.hello.CheckpointsOn {
		t.Errorf("no Checkpointer, offered: %+v", a.hello)
	}
	f := &fakeCheckpointer{paths: []string{"/work/p"}}
	if a := startExecHost(t, cpConfig(f, &cpClock{t: cpT0})); !a.hello.CheckpointsOn {
		t.Errorf("a Checkpointer, not offered: %+v", a.hello)
	}
	socket := linked(t, Config{Ports: PortSet{}})
	_, err := agent.Checkpoint(socket, nil)
	var r *agent.Refused
	if !errors.As(err, &r) || r.Reason != agentproto.CheckpointUnavailable || !strings.Contains(err.Error(), "takes no checkpoints") {
		t.Fatalf("not offered: %v", err)
	}
}

// A path that is not one of [checkpoints] paths is "unknown path", and
// nothing is taken of it; the answer names nothing it was not given.
func TestCheckpointUnknownPath(t *testing.T) {
	f := &fakeCheckpointer{paths: []string{"/work/p"}}
	a := startExecHost(t, cpConfig(f, &cpClock{t: cpT0}))
	r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint, Paths: []string{"/work/q", "/work/p/sub"}})
	if !r.OK || len(r.Checkpoints) != 2 {
		t.Fatalf("got %+v", r)
	}
	for _, c := range r.Checkpoints {
		if c.ID != "" || c.Reason != unknownCheckpoint {
			t.Errorf("%+v", c)
		}
	}
	if takes := f.takes(); len(takes) != 0 {
		t.Fatalf("took %v", takes)
	}
}

// What the sandbox sends is bounded and checked before anything else:
// too many paths, one too long, an unprintable, relative, unclean one, or
// one that is the home or "/", is refused whole, quoting nothing
// unprintable.
func TestCheckpointHostileInput(t *testing.T) {
	many := make([]string, agentproto.MaxCheckpointPaths+1)
	for i := range many {
		many[i] = fmt.Sprintf("/work/p%d", i)
	}
	for _, paths := range [][]string{
		many,
		{"/work/" + strings.Repeat("a", agentproto.MaxCheckpointPath)},
		{"/work/p\x1b[31m"},
		{"/work/p\u202e"},
		{"/work/p\n/etc"},
		{"work/p"},
		{"./p"},
		{"../etc"},
		{"/work/../etc"},
		{"/work/p/"},
		{"~/../etc"},
		{"~root/x"},
		{"~"},
		{"/"},
		{""},
		{"/home"},
	} {
		_, err := CheckCheckpointPaths(paths)
		if egressReason(err) != agentproto.CheckpointBad {
			t.Errorf("%q: %v", paths, err)
			continue
		}
		if msg := err.Error(); strings.ContainsAny(msg, "\x1b\n\u202e") || len(msg) > 400 {
			t.Errorf("%q: refusal %q", paths, msg)
		}
	}
	got, err := CheckCheckpointPaths([]string{"/home/agent/.claude", "~/.claude", "/work/p"})
	if err != nil || !slices.Equal(got, []string{"~/.claude", "/work/p"}) {
		t.Fatalf("good paths: %q, %v", got, err)
	}
	if agentproto.MaxCheckpointPaths != config.MaxCheckpointPaths {
		t.Errorf("MaxCheckpointPaths %d, config's %d", agentproto.MaxCheckpointPaths, config.MaxCheckpointPaths)
	}

	// Sent past the agent's own checks, the host refuses it all the same.
	f := &fakeCheckpointer{paths: []string{"/work/p"}}
	a := startExecHost(t, cpConfig(f, &cpClock{t: cpT0}))
	r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint, Paths: []string{"/work/p\x1b]0;x\x07"}})
	if r.OK || r.Reason != agentproto.CheckpointBad || strings.ContainsAny(r.Error, "\x1b\x07") {
		t.Fatalf("got %+v", r)
	}
	r = a.request(agentproto.Message{Op: agentproto.OpCheckpoint, Paths: many})
	if r.OK || r.Reason != agentproto.CheckpointBad {
		t.Fatalf("many: %+v", r)
	}
	if takes := f.takes(); len(takes) != 0 {
		t.Fatalf("took %v", takes)
	}
}

// The policy decides it: checkpoint_requests = "off" refuses it, and so
// does a link that does not know the setting; the broker's word is the
// decision when there is one.
func TestCheckpointDecided(t *testing.T) {
	f := &fakeCheckpointer{paths: []string{"/work/p"}}
	for _, v := range []string{config.CheckpointRequestsOff, ""} {
		cfg := cpConfig(f, &cpClock{t: cpT0})
		cfg.CheckpointRequests = v
		a := startExecHost(t, cfg)
		r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint})
		if r.OK || r.Reason != agentproto.CheckpointOff || !strings.Contains(r.Error, "checkpoint_requests") {
			t.Errorf("%q: %+v", v, r)
		}
	}
	for _, trust := range []string{config.TrustFallible, config.TrustHostile} {
		cfg := cpConfig(f, &cpClock{t: cpT0})
		cfg.Trust = trust
		a := startExecHost(t, cfg)
		if r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint}); !r.OK || len(r.Checkpoints) != 1 || r.Checkpoints[0].ID == "" {
			t.Errorf("trust %s: %+v", trust, r)
		}
	}
	fd := newFakeDecider()
	cfg := cpConfig(f, &cpClock{t: cpT0})
	cfg.Decide = fd
	a := startExecHost(t, cfg)
	if r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint}); r.OK || r.Reason != agentproto.CheckpointOff {
		t.Errorf("the broker denies: %+v", r)
	}
	fd.set(policy.KindCheckpoint, broker.Outcome{Verdict: broker.Allowed})
	if r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint}); !r.OK || r.Checkpoints[0].ID == "" {
		t.Errorf("the broker allows: %+v", r)
	}
	if n := len(f.takes()); n != 3 {
		t.Errorf("%d takes", n)
	}
}

// With no [checkpoints] paths, or none readable, nothing is taken, and the
// refusal says why.
func TestCheckpointUnavailable(t *testing.T) {
	f := &fakeCheckpointer{}
	a := startExecHost(t, cpConfig(f, &cpClock{t: cpT0}))
	if r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint}); r.OK || r.Reason != agentproto.CheckpointUnavailable || !strings.Contains(r.Error, "no [checkpoints] paths") {
		t.Errorf("none configured: %+v", r)
	}
	f.mu.Lock()
	f.err = errors.New("config.toml: bad\x1b")
	f.mu.Unlock()
	if r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint}); r.OK || r.Reason != agentproto.CheckpointUnavailable || strings.Contains(r.Error, "\x1b") {
		t.Errorf("unreadable: %+v", r)
	}
	f.mu.Lock()
	f.err, f.paths, f.reason = nil, []string{"/work/p"}, "the store's lock is held\x07"
	f.mu.Unlock()
	r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint})
	if !r.OK || r.Checkpoints[0].ID != "" || r.Checkpoints[0].Reason != `the store's lock is held\u0007` {
		t.Errorf("take failed: %+v", r)
	}
}

// At most one checkpoint of a path a minute, and CheckpointRequestsPerHour
// requests an hour, on the environment's clock; a request with nothing to
// take counts for nothing, and a clock set back limits rather than frees.
func TestCheckpointLimits(t *testing.T) {
	clock := &cpClock{t: cpT0}
	l := NewCheckpointLimits(clock.now)
	if ok, full := l.admit([]string{"/a", "/b"}); full || !slices.Equal(ok, []string{"/a", "/b"}) {
		t.Fatalf("first: %v %v", ok, full)
	}
	clock.add(CheckpointPathEvery - time.Second)
	if ok, full := l.admit([]string{"/a"}); full || len(ok) != 0 {
		t.Fatalf("within the minute: %v %v", ok, full)
	}
	clock.add(time.Second)
	if ok, _ := l.admit([]string{"/a"}); !slices.Equal(ok, []string{"/a"}) {
		t.Fatalf("after the minute: %v", ok)
	}
	// 2 accepted so far; unknown-only and limited-only requests do not count.
	if ok, _ := l.admit(nil); ok != nil {
		t.Fatal(ok)
	}
	for i := 2; i < CheckpointRequestsPerHour; i++ {
		clock.add(CheckpointPathEvery)
		if ok, full := l.admit([]string{"/a"}); full || len(ok) != 1 {
			t.Fatalf("request %d: %v %v", i+1, ok, full)
		}
	}
	clock.add(CheckpointPathEvery)
	if ok, full := l.admit([]string{"/c"}); !full || ok != nil {
		t.Fatalf("past the hour's cap: %v %v", ok, full)
	}
	// The first was accepted at cpT0: an hour later, one more goes.
	clock.t = cpT0.Add(time.Hour)
	if ok, full := l.admit([]string{"/c"}); full || len(ok) != 1 {
		t.Fatalf("an hour on: %v %v", ok, full)
	}
	clock.t = cpT0.Add(-time.Hour)
	if ok, full := l.admit([]string{"/d"}); !full || ok != nil {
		t.Fatalf("clock set back: %v %v", ok, full)
	}
}

// Through the host: the hour's cap is a refusal of the whole (Reason
// limit), and the limits are the environment's, shared by its links.
func TestCheckpointLimitedThroughHost(t *testing.T) {
	f := &fakeCheckpointer{paths: []string{"/work/p"}}
	clock := &cpClock{t: cpT0}
	cfg := cpConfig(f, clock)
	for i := range CheckpointRequestsPerHour {
		cfg.CheckpointLimits.admit([]string{fmt.Sprintf("/work/x%d", i)})
		clock.add(time.Second)
	}
	a := startExecHost(t, cfg)
	r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint})
	if r.OK || r.Reason != agentproto.CheckpointLimit || !strings.Contains(r.Error, "an hour") {
		t.Fatalf("got %+v", r)
	}
	if takes := f.takes(); len(takes) != 0 {
		t.Fatalf("took %v", takes)
	}
}

// What is logged of checkpoint requests is bounded per minute, and the
// rest counted once the minute is over.
func TestCheckpointLogBounded(t *testing.T) {
	clock := &cpClock{t: cpT0}
	l := NewCheckpointLimits(clock.now)
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	for range 100 {
		l.logf(lg, "line")
	}
	if n := strings.Count(buf.String(), "line\n"); n != checkpointLogMax {
		t.Fatalf("%d lines logged in a minute", n)
	}
	clock.add(checkpointLogEvery)
	l.logf(lg, "line")
	if !strings.Contains(buf.String(), fmt.Sprintf("%d more lines not logged", 100-checkpointLogMax)) {
		t.Fatalf("log %q", buf.String())
	}
}

// One checkpoint request at a time per link: a second, while the first is
// being taken, is refused (Reason limit) and takes nothing; once the first
// is done, the next is served.
func TestCheckpointOneAtATime(t *testing.T) {
	f := &fakeCheckpointer{paths: []string{"/work/p", "/work/q"}, begun: make(chan struct{}, 1), release: make(chan struct{})}
	a := startExecHost(t, cpConfig(f, &cpClock{t: cpT0}))
	first := a.send(agentproto.Message{Op: agentproto.OpCheckpoint, Paths: []string{"/work/p"}})
	<-f.begun
	r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint, Paths: []string{"/work/q"}})
	if r.OK || r.Reason != agentproto.CheckpointLimit || !strings.Contains(r.Error, "still being taken") {
		t.Fatalf("second while the first runs: %+v", r)
	}
	close(f.release)
	if r, ok := a.answer(first, 5*time.Second); !ok || !r.OK || r.Checkpoints[0].ID == "" {
		t.Fatalf("first: %+v %v", r, ok)
	}
	f.mu.Lock()
	f.begun = nil
	f.mu.Unlock()
	if r := a.request(agentproto.Message{Op: agentproto.OpCheckpoint, Paths: []string{"/work/q"}}); !r.OK || r.Checkpoints[0].ID == "" {
		t.Fatalf("after the first: %+v", r)
	}
	if takes := f.takes(); !slices.EqualFunc(takes, [][]string{{"/work/p"}, {"/work/q"}}, slices.Equal) {
		t.Fatalf("takes %v", takes)
	}
}
