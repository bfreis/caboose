package launcher

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/tty"
)

// prune --docker deletes the vm sandbox's docker disk for an empty one:
// it says the size, asks (CABOOSE_FORCE=1 does not), and stops a running VM
// first, listing its sessions. Under docker and gvisor there is no such
// disk, and it says what cleans up instead, touching nothing.
func TestPruneDocker(t *testing.T) {
	sessions := func(s backend.ExecSpec) *exec.Cmd {
		if len(s.Argv) > 1 && s.Argv[0] == "tmux" && s.Argv[1] == "list-sessions" {
			return backendtest.Reply("proj-abc123\n", "", 0)
		}
		return nil
	}
	force := func(k string) string { return map[string]string{"CABOOSE_FORCE": "1"}[k] }
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, runningBox(iso))
			b.box.Exec = sessions
			disk := b.dockerDisk()
			if err := os.MkdirAll(filepath.Dir(disk), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(disk, bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(b.vmImages().template()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(b.vmImages().template(), []byte("empty"), 0o600); err != nil {
				t.Fatal(err)
			}
			kept := func() {
				t.Helper()
				if got, err := os.ReadFile(disk); err != nil || len(got) != 1<<20 {
					t.Errorf("the disk was touched: %d bytes, %v", len(got), err)
				}
				if len(b.box.Calls) != 0 || b.box.State() != "running" {
					t.Errorf("the sandbox was touched: %q, %s", b.box.Calls, b.box.State())
				}
			}

			if iso != isolationVM {
				b.Cfg.Getenv = force
				err := b.Prune([]string{"--docker"})
				if err == nil || !strings.Contains(err.Error(), "under "+iso+" it has none") ||
					!strings.Contains(err.Error(), "'docker system prune' here") {
					t.Errorf("%v", err)
				}
				kept()
				return
			}

			if err := b.Prune([]string{"--dokcer"}); err == nil || !strings.Contains(err.Error(), "usage: caboose prune [--docker]") {
				t.Errorf("a stray argument: %v", err)
			}
			if !tty.IsTerminal(os.Stdin.Fd()) {
				err := b.Prune([]string{"--docker"})
				if err == nil || !strings.Contains(err.Error(), "refusing to delete it non-interactively (set CABOOSE_FORCE=1 to delete it)") {
					t.Errorf("no terminal: %v", err)
				}
				kept()
			}
			b.said()

			out, err := du(disk)
			if err != nil {
				t.Fatal(err)
			}
			size := strings.Fields(out)[0]
			b.Cfg.Getenv = force
			if err := b.Prune([]string{"--docker"}); err != nil {
				t.Fatal(err)
			}
			_, errs := b.said()
			for _, want := range []string{
				"caboose: this deletes " + disk + " (" + size + " on disk)",
				"caboose: these live session(s) end:\n  proj-abc123\n",
				"caboose: CABOOSE_FORCE=1 set, continuing.\n",
				"caboose: VM stopped\n",
				"caboose: deleted " + disk + ", freeing " + size + "\n",
				"caboose: 'caboose' starts the VM again, its dockerd empty\n",
			} {
				if !strings.Contains(errs, want) {
					t.Errorf("missing %q in:\n%s", want, errs)
				}
			}
			if got, err := os.ReadFile(disk); err != nil || string(got) != "empty" {
				t.Errorf("the disk is not the empty one: %q, %v", got, err)
			}
			if !slices.Equal(b.box.Calls, []string{"stop"}) || b.box.State() != "exited" {
				t.Errorf("calls %q, state %s", b.box.Calls, b.box.State())
			}

			// Stopped, and the disk already gone: nothing to stop or delete.
			if err := os.Remove(disk); err != nil {
				t.Fatal(err)
			}
			if err := b.Prune([]string{"--docker"}); err != nil {
				t.Fatal(err)
			}
			if _, errs := b.said(); !strings.Contains(errs, "nothing to delete") || len(b.box.Calls) != 1 {
				t.Errorf("calls %q:\n%s", b.box.Calls, errs)
			}
		})
	}
}
