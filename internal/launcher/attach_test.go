package launcher

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
)

func TestOrphaned(t *testing.T) {
	for _, tc := range []struct {
		clients, live, ended int
		want                 bool
	}{
		{0, 0, 0, false},
		{0, 0, 3, false},
		{1, 0, 1, true},
		{1, 0, 2, true},
		{2, 0, 1, false}, // a client no record accounts for
		{1, 1, 0, false},
		{1, 1, 1, false}, // someone is still looking at it
		{1, 0, 0, false}, // a client with no record at all
	} {
		if got := orphaned(tc.clients, tc.live, tc.ended); got != tc.want {
			t.Errorf("orphaned(%d clients, %d live, %d ended) = %v", tc.clients, tc.live, tc.ended, got)
		}
	}
}

// attachFake is a sandbox, under gVisor where closed terminals' clients
// linger, whose tmux has clients clients on every session.
func attachFake(t *testing.T, clients int) (a *App, box *backendtest.Fake) {
	t.Helper()
	box = runningBox(isolationGVisor)
	box.Exec = func(s backend.ExecSpec) *exec.Cmd {
		if len(s.Argv) > 1 && s.Argv[0] == "tmux" && s.Argv[1] == "list-clients" {
			return backendtest.Reply(strings.Repeat("/dev/pts/0\n", clients), "", 0)
		}
		return nil
	}
	a = &App{Cfg: &config.Config{Container: "box", Isolation: isolationGVisor, DataDir: filepath.Join(t.TempDir(), "data")},
		Backend: box, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	return a, box
}

// plant writes a record of pid attaching session.
func plant(t *testing.T, a *App, pid int, session string) {
	t.Helper()
	dir := filepath.Join(a.Cfg.DataDir, attachedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)), []byte(session+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// PIDs below 1000 are running, the rest have ended.
func fakeAlive(t *testing.T) {
	old := processAlive
	processAlive = func(pid int) bool { return pid < 1000 }
	t.Cleanup(func() { processAlive = old })
}

func TestClientsInUse(t *testing.T) {
	fakeAlive(t)
	for _, tc := range []struct {
		name    string
		clients int
		records map[int]string
		want    int
	}{
		{"no clients", 0, map[int]string{2000: "s"}, 0},
		{"a closed terminal", 1, map[int]string{2000: "s"}, 0},
		{"an open terminal", 1, map[int]string{500: "s"}, 1},
		{"one open, one closed", 2, map[int]string{500: "s", 2000: "s"}, 2},
		{"no record", 1, nil, 1},
		{"another session's record", 1, map[int]string{2000: "other"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, box := attachFake(t, tc.clients)
			for pid, s := range tc.records {
				plant(t, a, pid, s)
			}
			if got := a.clientsInUse("s"); got != tc.want {
				t.Errorf("clientsInUse = %d, want %d", got, tc.want)
			}
			detached := box.Ran("tmux", "detach-client", "-s", "=s")
			if taken := tc.clients > 0 && tc.want == 0; detached != taken {
				t.Errorf("detached: %v, want %v", detached, taken)
			}
			if _, err := os.Stat(filepath.Join(a.Cfg.DataDir, attachedDir, "2000")); tc.records[2000] == "s" && tc.clients > 0 && tc.want == 0 && err == nil {
				t.Error("the ended record is still there")
			}
		})
	}
}

// Attaching records this process, and drops the records of ended ones.
func TestRecordAttach(t *testing.T) {
	fakeAlive(t)
	a, _ := attachFake(t, 0)
	plant(t, a, 500, "a")
	plant(t, a, 2000, "b")
	a.recordAttach("mine")
	recs := a.attachRecords()
	if recs[os.Getpid()] != "mine" || recs[500] != "a" {
		t.Errorf("records %v", recs)
	}
	if _, ok := recs[2000]; ok {
		t.Errorf("ended record kept: %v", recs)
	}
}
