package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/syncagent"
)

// caboose sync status fetches from the remote, and under vm the fetch's
// ssh goes through the outbound proxy the link serves: so with the VM
// running, it starts the link and waits for the proxy before the fetch,
// as caboose sync does, whichever command brought the VM up. Under docker
// and gvisor there is no proxy to wait for.
func TestSyncStatusStartsTheLinkBeforeTheFetch(t *testing.T) {
	waitProxy := []string{AgentPath, "wait-proxy"}
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			b := newBoxApp(t, iso, box)
			repo := filepath.Join(b.data, statesync.Dir)
			box.SandboxMounts = append(box.SandboxMounts, mount(repo, statesync.ContainerDir))
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			conf := "[remote \"origin\"]\n\turl = git@example.com:me/state.git\n"
			if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
			box.Exec = func(s backend.ExecSpec) *exec.Cmd {
				// The status, the fetch in it: no remote branch yet.
				if isSyncExec(s, syncagent.OpStatus) {
					return fakeAgent(`{"type":"status","status":{"remote":true}}`)
				}
				return nil
			}
			if err := b.SyncStatus(nil); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			index := func(match func([]string) bool) int {
				return slices.IndexFunc(box.Execs, func(s backend.ExecSpec) bool { return match(s.Argv) })
			}
			wait := index(func(argv []string) bool {
				return len(argv) >= 2 && slices.Equal(argv[:2], waitProxy)
			})
			fetch := index(func(argv []string) bool { return slices.Equal(argv, []string{AgentPath, "sync", syncagent.OpStatus}) })
			if fetch < 0 {
				t.Fatalf("no fetch in %+v", box.Execs)
			}
			linked := slices.ContainsFunc(b.spawned, func(argv []string) bool { return slices.Contains(argv, "link") })
			if iso != isolationVM {
				if wait >= 0 || linked {
					t.Errorf("under %s, waited for a proxy (%d) or started a link (%v): %+v", iso, wait, b.spawned, box.Execs)
				}
				return
			}
			if !linked {
				t.Errorf("the link was not started: spawned %v", b.spawned)
			}
			if wait < 0 || wait > fetch {
				t.Errorf("wait-proxy at %d, fetch at %d: %+v", wait, fetch, box.Execs)
			}
		})
	}
}

// A sync that committed here but never pushed leaves its files in the
// repo, so "to send" does not count them: sync status lists them apart,
// even when the fetch fails as that push did.
func TestSyncStatusShowsUnsentCommits(t *testing.T) {
	here, _ := pair(t)
	here.sync()
	here.write(filepath.Join(here.data, "home/.claude/agents/a.md"), "a\n")
	here.write(filepath.Join(here.data, statesync.Dir, "home/.claude/agents/a.md"), "a\n")
	here.git("add", "-A")
	here.git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "unsent")
	here.write(filepath.Join(here.ctl, "hostkey"), "")
	if err := here.a.SyncStatus(nil); err != nil {
		t.Fatal(err)
	}
	out := here.stdout.String()
	for _, want := range []string{
		"to send : 0 files\n",
		"unsent (1 commit never pushed): 1 file\n  M home/.claude/agents/a.md\n          'caboose sync' sends it\n",
		"to take : not checked: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}
