package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// proc adds process pid to the fake /proc under root.
func proc(t *testing.T, root string, pid int, comm string, ppid int) {
	t.Helper()
	d := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) S %d %d 0 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 1000 100 0\n", pid, comm, ppid, pid)
	if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAbove(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 10, "claude", 1)
	proc(t, root, 11, "bash", 10)
	proc(t, root, 12, "sh -c (x) y", 11) // a comm with spaces and parentheses
	proc(t, root, 13, "node", 12)
	if got := claudeAbove(root, 13); got != 10 {
		t.Fatalf("found %d, want 10", got)
	}
	if got := claudeAbove(root, 10); got != 10 {
		t.Fatalf("a claude parent itself: %d", got)
	}
	// A comm that says claude inside parentheses is not claude.
	proc(t, root, 20, "x) S 1 (claude", 1)
	proc(t, root, 21, "bash", 20)
	if got := claudeAbove(root, 21); got != 0 {
		t.Fatalf("parsed the comm wrongly: %d", got)
	}
	// Five levels and no more.
	proc(t, root, 30, "claude", 1)
	for i := 31; i <= 36; i++ {
		proc(t, root, i, "sh", i-1)
	}
	if got := claudeAbove(root, 34); got != 30 { // 34, 33, 32, 31, 30: five
		t.Fatalf("five levels: %d", got)
	}
	if got := claudeAbove(root, 35); got != 0 { // six
		t.Fatalf("six levels reached %d", got)
	}
	if got := claudeAbove(root, 999); got != 0 {
		t.Fatalf("no such process: %d", got)
	}
	// A loop of parents ends.
	proc(t, root, 40, "sh", 41)
	proc(t, root, 41, "sh", 40)
	if got := claudeAbove(root, 40); got != 0 {
		t.Fatalf("loop: %d", got)
	}
}

func TestParseStatGarbage(t *testing.T) {
	for _, s := range []string{"", "no parens", ") (", "1 (a) S", "1 (a) S x"} {
		if _, _, ok := parseStat([]byte(s)); ok {
			t.Errorf("%q parsed", s)
		}
	}
}

func TestClaudeProcsCounts(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 10, "claude", 1)
	proc(t, root, 11, "claude", 1)
	proc(t, root, 12, "claude-helper", 1)
	proc(t, root, 13, "bash", 1)
	os.MkdirAll(filepath.Join(root, "net"), 0o755)
	got := claudeProcs(root)
	if len(got) != 2 || !got[10] || !got[11] {
		t.Fatalf("%v", got)
	}
}

type stepClock struct{ t time.Time }

func (c *stepClock) now() time.Time { return c.t }

func TestPresenceReports(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 10, "claude", 1)
	proc(t, root, 11, "claude", 1)
	clk := &stepClock{t: time.Unix(1000, 0)}
	p := newPresence(root, clk.now, 5*time.Second)

	info, send := p.due()
	if !send || info != (agentproto.ModsInfo{Sessions: 2}) {
		t.Fatalf("first: %+v %v", info, send)
	}
	clk.t = clk.t.Add(time.Minute)
	if _, send := p.due(); send {
		t.Fatal("sent without a change")
	}

	leave := p.join(10)
	info, send = p.due()
	if !send || info != (agentproto.ModsInfo{Sessions: 2, WithMod: 1}) {
		t.Fatalf("join: %+v %v", info, send)
	}

	// A change inside the gap waits, and goes out when the gap has passed.
	proc(t, root, 12, "claude", 1)
	clk.t = clk.t.Add(time.Second)
	if _, send := p.due(); send {
		t.Fatal("sent twice within 5 s")
	}
	clk.t = clk.t.Add(4 * time.Second)
	info, send = p.due()
	if !send || info.Sessions != 3 {
		t.Fatalf("after the gap: %+v %v", info, send)
	}

	// The wait ends: present for 30 s more, then not.
	leave()
	clk.t = clk.t.Add(29 * time.Second)
	if _, send := p.due(); send {
		t.Fatal("the mod left within its grace")
	}
	clk.t = clk.t.Add(2 * time.Second)
	info, send = p.due()
	if !send || info != (agentproto.ModsInfo{Sessions: 3}) {
		t.Fatalf("after grace: %+v %v", info, send)
	}
	if len(p.waiters) != 0 {
		t.Fatalf("waiters kept: %v", p.waiters)
	}
}

func TestPresenceBounds(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 10, "claude", 1)
	p := newPresence(root, nil, 0)
	var leaves []func()
	for pid := 1; pid <= maxWaiters+50; pid++ {
		leaves = append(leaves, p.join(pid))
	}
	if len(p.waiters) != maxWaiters {
		t.Fatalf("%d waiters", len(p.waiters))
	}
	p.join(0)()
	p.join(-3)()
	// 256 waiters, and only pid 10 of them is a claude process.
	if info := p.count(); info.WithMod != 1 || info.Sessions != 1 {
		t.Fatalf("%+v", info)
	}
	for _, l := range leaves {
		l()
		l() // leaving twice counts once
	}
}

// The link tells the host the count: once, and again when a mod's wait
// registers its Claude Code process.
func TestLinkReportsMods(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 100, "claude", 1)
	proc(t, root, 101, "bash", 100)
	h := startBrokerLink(t, true, root, func(m agentproto.Message) agentproto.Message {
		return agentproto.Message{OK: true, Cursor: 1}
	})
	wait := func(want agentproto.ModsInfo) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case got := <-h.mods:
				if got == want {
					return
				}
			case <-deadline:
				t.Fatalf("never told %+v", want)
			}
		}
	}
	wait(agentproto.ModsInfo{Sessions: 1})
	if code, _, errb := runRequestsAs(t, h.socket, root, 101, "--mod", "--wait", "0"); code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	wait(agentproto.ModsInfo{Sessions: 1, WithMod: 1})
}

// No broker offered: the host is never told.
func TestLinkSilentWithoutBroker(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 100, "claude", 1)
	h := startBrokerLink(t, false, root, nil)
	select {
	case m := <-h.mods:
		t.Fatalf("told a host that offers no requests: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}
