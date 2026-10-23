package agent

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// Presence: which Claude Code sessions in the sandbox have caboose's mod
// loaded. The mod runs `caboose-agent requests --mod`, a long poll, as long
// as its session lives; the command finds the Claude Code process above it
// and tells the link, which counts that process as having the mod while the
// poll runs and a while after. The link counts the sandbox's `claude`
// processes itself and tells the host both numbers (agentproto.TypeMods)
// when they change. The sandbox's own word: any process can claim any PID,
// which only moves a number the host shows a person, never a decision.

const (
	// claudeComm is the name of a Claude Code process (its /proc comm).
	claudeComm = "claude"
	// modGrace is how long a mod counts as present after its last waiter
	// ended: a poll ends and the next one begins.
	modGrace = 30 * time.Second
	// modsEvery is the least time between two TypeMods; scanEvery is how
	// often /proc is read for them.
	modsEvery = 5 * time.Second
	scanEvery = 5 * time.Second
	// maxWaiters bounds the PIDs remembered; maxProcEntries bounds one scan.
	maxWaiters     = 256
	maxProcEntries = 1 << 16
	// maxWalk is how many processes up the command looks for claude.
	maxWalk = 5
)

// readComm is the comm of process pid under procRoot, "" when it cannot
// be read.
func readComm(procRoot string, pid int) string {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 64))
	return string(bytes.TrimRight(b, "\n"))
}

// statOf is the comm and parent of process pid, from procRoot/PID/stat:
// "pid (comm) S ppid ...", where comm may itself hold spaces and
// parentheses, so it ends at the last ')'.
func statOf(procRoot string, pid int) (comm string, ppid int, ok bool) {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return parseStat(b)
}

func parseStat(b []byte) (comm string, ppid int, ok bool) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return "", 0, false
	}
	// After the comm: " S ppid ...".
	rest := bytes.Fields(b[end+1:])
	if len(rest) < 2 {
		return "", 0, false
	}
	n, err := strconv.Atoi(string(rest[1]))
	if err != nil || n < 0 {
		return "", 0, false
	}
	return string(b[open+1 : end]), n, true
}

// claudeAbove walks up from process start (the command's parent), at
// most maxWalk processes, to the first whose comm is claude, and returns
// its PID, or 0 when there is none.
func claudeAbove(procRoot string, start int) int {
	pid := start
	for i := 0; i < maxWalk && pid > 1; i++ {
		comm, ppid, ok := statOf(procRoot, pid)
		if !ok {
			return 0
		}
		if comm == claudeComm {
			return pid
		}
		pid = ppid
	}
	return 0
}

// claudeProcs are the PIDs of the processes under procRoot whose comm is
// claude: at most maxProcEntries entries are looked at.
func claudeProcs(procRoot string) map[int]bool {
	out := map[int]bool{}
	d, err := os.Open(procRoot)
	if err != nil {
		return out
	}
	defer d.Close()
	names, _ := d.Readdirnames(maxProcEntries)
	for _, n := range names {
		pid, err := strconv.Atoi(n)
		if err != nil || pid <= 0 {
			continue
		}
		if readComm(procRoot, pid) == claudeComm {
			out[pid] = true
		}
	}
	return out
}

// waiter is a Claude Code process whose mod is, or was, waiting.
type waiter struct {
	active int
	ended  time.Time
}

// presence keeps the waiters and decides when the host is told.
type presence struct {
	procRoot string
	now      func() time.Time
	// grace and minGap are modGrace and modsEvery, other in tests.
	grace, minGap time.Duration

	mu       sync.Mutex
	waiters  map[int]*waiter
	last     *agentproto.ModsInfo
	lastSent time.Time
}

func newPresence(procRoot string, now func() time.Time, minGap time.Duration) *presence {
	if now == nil {
		now = time.Now
	}
	if minGap <= 0 {
		minGap = modsEvery
	}
	return &presence{procRoot: procRoot, now: now, grace: modGrace, minGap: minGap, waiters: map[int]*waiter{}}
}

// join registers pid as a process with a waiting mod, and returns the
// function that says the wait ended. A PID that is not positive, or one
// too many, registers nothing.
func (p *presence) join(pid int) (leave func()) {
	if pid <= 0 {
		return func() {}
	}
	p.mu.Lock()
	w := p.waiters[pid]
	if w == nil {
		if len(p.waiters) >= maxWaiters {
			p.mu.Unlock()
			return func() {}
		}
		w = &waiter{}
		p.waiters[pid] = w
	}
	w.active++
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			w.active--
			w.ended = p.now()
			p.mu.Unlock()
		})
	}
}

// count is the sessions now, and how many of them have a mod present.
func (p *presence) count() agentproto.ModsInfo {
	procs := claudeProcs(p.procRoot)
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	info := agentproto.ModsInfo{Sessions: len(procs)}
	for pid, w := range p.waiters {
		if w.active == 0 && now.Sub(w.ended) >= p.grace {
			delete(p.waiters, pid)
			continue
		}
		if procs[pid] {
			info.WithMod++
		}
	}
	return info
}

// due reads /proc and returns the count to send, if the host is to be
// told: the first time, then only when it changed and the last was sent at
// least minGap ago (a change that has to wait is sent by a later call).
func (p *presence) due() (agentproto.ModsInfo, bool) {
	info := p.count()
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last != nil && (*p.last == info || now.Sub(p.lastSent) < p.minGap) {
		return info, false
	}
	p.last, p.lastSent = &info, now
	return info, true
}
