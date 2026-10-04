package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bfreis/caboose/internal/backend"
)

// A session with a tmux client on it is one someone is looking at, and a
// launch steps past it (session.FirstFree). That relies on the client going
// away when its terminal closes, which under runc it does: the exec's pty
// hangs up. Under gVisor it does not -- the client has no tty the sandbox's
// kernel can hang up, and lingers -- so a reopened terminal would never
// land back on the session it left. The container cannot tell; the host
// can: the launch that attaches execs docker, keeping its PID, and that
// process ends with its terminal. So each attach leaves a record here, the
// PID by name and the session inside, and a session whose clients are all
// accounted for by records of ended processes is taken back.
//
// attachedDir is in the data dir but in none of its mounts: the host's
// alone, so the sandbox cannot plant a record.
const attachedDir = "attached"

// processAlive reports whether pid is a running process: a PID reused by
// another counts as alive, which keeps its session busy -- the safe side.
var processAlive = func(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// recordAttach records that this process, about to become docker, attaches
// a terminal to session name, and drops the records of ended processes.
// A record that cannot be written costs only the taking back.
func (a *App) recordAttach(name string) {
	dir := filepath.Join(a.Cfg.DataDir, attachedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	for pid := range a.attachRecords() {
		if !processAlive(pid) {
			os.Remove(filepath.Join(dir, strconv.Itoa(pid)))
		}
	}
	_ = os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), []byte(name+"\n"), 0o600)
}

// attachRecords are the records there are, session by PID.
func (a *App) attachRecords() map[int]string {
	dir := filepath.Join(a.Cfg.DataDir, attachedDir)
	entries, _ := os.ReadDir(dir)
	recs := map[int]string{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		recs[pid] = strings.TrimSpace(string(b))
	}
	return recs
}

// orphaned reports whether a session's clients were all left by terminals
// that closed: at least as many records of ended processes as clients, and
// none of a running one. A client with no record (a launcher from before
// these, a docker exec by hand) keeps it busy.
func orphaned(clients, live, ended int) bool {
	return clients > 0 && live == 0 && ended >= clients
}

// clientsInUse is clientsOn for session.FirstFree, but a session left
// attached by closed terminals is detached, and counts as free.
func (a *App) clientsInUse(name string) int {
	n := a.clientsOn(name)
	if n == 0 {
		return 0
	}
	var live, ended []int
	for pid, s := range a.attachRecords() {
		switch {
		case s != name:
		case processAlive(pid):
			live = append(live, pid)
		default:
			ended = append(ended, pid)
		}
	}
	if !orphaned(n, len(live), len(ended)) {
		return n
	}
	if backend.Quiet(a.box(), "tmux", "detach-client", "-s", "="+name) != nil {
		return n
	}
	a.Note("'%s' was still attached to a terminal that has closed; taking it back", name)
	for _, pid := range ended {
		os.Remove(filepath.Join(a.Cfg.DataDir, attachedDir, strconv.Itoa(pid)))
	}
	return 0
}
