package launcher

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/syncagent"
)

// isSyncExec reports whether s runs caboose-agent sync op.
func isSyncExec(s backend.ExecSpec, op string) bool {
	return slices.Equal(s.Argv, []string{AgentPath, "sync", op}) && s.Stdin && !s.TTY
}

// fakeAgent is a caboose-agent sync that says hello, takes the request,
// and then says lines, one message each.
func fakeAgent(lines ...string) *exec.Cmd {
	return fakeAgentSaving("", lines...)
}

// fakeAgentSaving is fakeAgent, keeping the request it took in file
// (unless "").
func fakeAgentSaving(file string, lines ...string) *exec.Cmd {
	out := fmt.Sprintf(`{"type":"hello","version":%d}`, syncagent.Version) + "\n"
	for _, l := range lines {
		out += l + "\n"
	}
	keep := "/dev/null"
	if file != "" {
		keep = file
	}
	return exec.Command("sh", "-c", `printf '%s\n' "$(printf '%s' "$1" | head -n 1)"; IFS= read -r req; printf '%s' "$req" > "$2"; printf '%s' "$1" | tail -n +2`,
		"agent", out, keep)
}

// syncBox is a running sandbox of each isolation, for a host with a sync
// remote set, whose sandbox answers what caboose sync runs before the sync
// itself, and the sync with agent's answer.
func syncBox(t *testing.T, iso string, agent func(op string) *exec.Cmd) *boxApp {
	t.Helper()
	box := runningBox(iso)
	b := newBoxApp(t, iso, box)
	repo := filepath.Join(b.data, statesync.Dir)
	home := filepath.Join(b.data, "home")
	box.SandboxMounts = append(box.SandboxMounts,
		mount(repo, statesync.ContainerDir),
		mount(home+"/.claude", "/home/agent/.claude"),
		mount(home+"/.claude.json", "/home/agent/.claude.json"),
		mount(home+"/.config/caboose", "/home/agent/.config/caboose"),
		mount(b.tmp, "/work"))
	b.write2(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = git@example.com:me/state.git\n")
	b.write2(t, filepath.Join(home, ".claude", "projects", "-work-p", "memory", "m.md"), "memory\n")
	b.write2(t, filepath.Join(repo, "home", ".claude", "projects", "-work-p", "memory", "m.md"), "old\n")
	box.Exec = func(s backend.ExecSpec) *exec.Cmd {
		switch {
		case isSyncExec(s, syncagent.OpRun):
			return agent(syncagent.OpRun)
		case isSyncExec(s, syncagent.OpStatus):
			return agent(syncagent.OpStatus)
		case len(s.Argv) > 1 && s.Argv[0] == "git" && s.Argv[1] == "--version":
			return backendtest.Reply("git version 2.43.0\n", "", 0)
		case len(s.Argv) > 0 && s.Argv[0] == "tmux":
			return backendtest.Fail()
		case len(s.Argv) > 2 && s.Argv[0] == "bash" && s.Argv[2] == processesScript:
			return backendtest.Reply("1 0 sleep\n", "", 0)
		}
		return nil
	}
	return b
}

func (b *boxApp) write2(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot is every file and directory under dir: its mode and contents.
// Not its time: what a launch sets up before the sync (datadir.EnsureLayout)
// touches the file mounts, which it has to, whatever the sync does.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		data := ""
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			data = string(b)
		}
		m[p] = fmt.Sprintf("%v %q", fi.Mode(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The sync runs in the sandbox: under every isolation, caboose sync, sync
// status and a launch's sync write nothing in the data dir's home or sync
// repo from the host -- a host write there is what the sandbox's view of
// it lags behind -- and tell the sandbox which keep entries it mounts.
func TestSyncWritesNothingOnTheHost(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			saved := filepath.Join(t.TempDir(), "request")
			report := `{"type":"report","report":{"Committed":true,"Pushed":true,"Applied":[".claude/x.md"]}}`
			b := syncBox(t, iso, func(op string) *exec.Cmd {
				if op == syncagent.OpStatus {
					return fakeAgentSaving(saved, `{"type":"status","status":{"remote":true,"changed":["home/.claude/x.md"]}}`)
				}
				return fakeAgentSaving(saved, report)
			})
			b.Cfg.AutoSync = true
			// What every launch sets up in the home, done once.
			if err := b.ensureRunning(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			dirs := []string{filepath.Join(b.data, "home"), filepath.Join(b.data, statesync.Dir)}
			snap := func() map[string]string {
				m := map[string]string{}
				for _, d := range dirs {
					maps.Copy(m, snapshot(t, d))
				}
				return m
			}
			before := snap()
			check := func(what string) {
				t.Helper()
				after := snap()
				if !maps.Equal(before, after) {
					for k, v := range after {
						if before[k] != v {
							t.Errorf("%s wrote %s", what, k)
						}
					}
					for k := range before {
						if _, ok := after[k]; !ok {
							t.Errorf("%s removed %s", what, k)
						}
					}
				}
				data, err := os.ReadFile(saved)
				if err != nil {
					t.Fatalf("%s: no request: %v", what, err)
				}
				var req syncagent.Request
				if err := json.Unmarshal(data, &req); err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				if want := []string{".caboose-sync", ".claude", ".claude.json", ".config/caboose"}; !slices.Equal(slices.Sorted(slices.Values(req.Mounted)), want) {
					t.Errorf("%s: mounted %q, want %q", what, req.Mounted, want)
				}
				if req.Version != syncagent.Version || req.Host == "" || len(req.Sandbox) == 0 {
					t.Errorf("%s: request %+v", what, req)
				}
				os.Remove(saved)
				before = after
			}
			if err := b.Sync(nil); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			check("caboose sync")
			if !strings.Contains(b.errb.String(), "caboose: sent this machine's changes\ncaboose: pushed\n") {
				t.Errorf("said:\n%s", b.errb)
			}
			if err := b.SyncStatus(nil); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			check("caboose sync status")
			if !strings.Contains(b.out.String(), "to send : 1 file\n  M home/.claude/x.md\n") {
				t.Errorf("status said:\n%s", b.out)
			}
			b.errb.Reset()
			b.beforeAttach(true)
			check("a launch")
			if got := b.errb.String(); got != "caboose: synced: sent this machine's\n" {
				t.Errorf("the launch said %q", got)
			}
		})
	}
}

// What the sandbox sends is shown printable, whatever it holds.
func TestSyncShowsTheSandboxsWordsPrintable(t *testing.T) {
	b := syncBox(t, isolationContainer, func(string) *exec.Cmd {
		return fakeAgent(`{"type":"report","report":{"Merged":true,"Applied":[".claude/a\u001b[2Jb.md"],"Unmounted":[".tool"]}}`,
			`{"type":"error","error":{"kind":"sync","message":"git push: \u001b]0;owned\u0007 refused"}}`)
	})
	err := b.Sync(nil)
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07") || !strings.Contains(err.Error(), "sync failed: git push: ") {
		t.Errorf("err = %q", err)
	}
	said := b.errb.String()
	if strings.Contains(said, "\x1b") || !strings.Contains(said, `~/.claude/a\u001B[2Jb.md`) {
		t.Errorf("said %q", said)
	}
	if !strings.Contains(said, "~/.tool, which the container does not mount yet: they sync after 'caboose restart'") {
		t.Errorf("unmounted entries not said: %q", said)
	}
}

// A sandbox whose caboose-agent predates the sync running there is told
// apart from a sync that failed: caboose restart is what fixes it.
func TestSyncInAnOldSandbox(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := syncBox(t, iso, func(string) *exec.Cmd {
				return backendtest.Reply("", "usage: caboose-agent COMMAND\n\n  open URL ...\n", 2)
			})
			err := b.Sync(nil)
			if err == nil || !strings.Contains(err.Error(), "cannot run this one's sync; run 'caboose restart'") {
				t.Errorf("caboose sync: %v", err)
			}
			b.Cfg.AutoSync = true
			b.errb.Reset()
			b.beforeAttach(true)
			if got := b.errb.String(); !strings.HasPrefix(got, "caboose: not synced: ") || !strings.HasSuffix(got, "'caboose restart' recreates it\n") {
				t.Errorf("a launch said %q", got)
			}
		})
	}
}

// The host, not the sandbox, decides whether an answer is shown as it is
// typed: only for the questions it knows to want no secret. The sandbox's
// prompt is labelled as such, line by line.
func TestAskText(t *testing.T) {
	for _, tc := range []struct {
		prompt string
		echo   bool
	}{
		{"Username for 'https://example.com': ", true},
		{"The authenticity of host 'example.com' can't be established.\nED25519 key fingerprint is SHA256:x.\n" +
			"Are you sure you want to continue connecting (yes/no/[fingerprint])? ", true},
		{"Password for 'https://me@example.com': ", false},
		{"Enter passphrase for key '/home/agent/.ssh/id': ", false},
		{"Username for 'x': \nPassword: ", false},
		{"Password (Username for 'x': )", false},
	} {
		text, echo := askText("container", tc.prompt)
		if echo != tc.echo {
			t.Errorf("%q: echo %v", tc.prompt, echo)
		}
		lines := strings.Split(text, "\n")
		if lines[0] != "caboose: the sync in the container asks:" {
			t.Errorf("%q: first line %q", tc.prompt, lines[0])
		}
		for _, l := range lines[1 : len(lines)-1] {
			if !strings.HasPrefix(l, "  | ") {
				t.Errorf("%q: unmarked line %q", tc.prompt, l)
			}
		}
	}
	text, _ := askText("VM", "x\ncaboose: synced\x1b[2J")
	if strings.Contains(text, "\ncaboose: synced") || strings.Contains(text, "\x1b") {
		t.Errorf("a line passes for caboose's: %q", text)
	}
}

// No line of what the sandbox says passes for one of caboose's own.
func TestShownIndents(t *testing.T) {
	if got := shown("git push: refused\ncaboose: synced\r\n"); got != "git push: refused\n  caboose: synced" {
		t.Errorf("shown = %q", got)
	}
}

// A launch's sync that does not finish -- whatever holds it up in the
// sandbox -- is stopped after the budget and a margin, and the launch goes
// on.
func TestAutoSyncStopsASyncThatHangs(t *testing.T) {
	budget, margin := syncBudget, syncMargin
	syncBudget, syncMargin = time.Second, time.Second
	defer func() { syncBudget, syncMargin = budget, margin }()
	b := syncBox(t, isolationContainer, func(string) *exec.Cmd {
		return exec.Command("sh", "-c", `echo '{"type":"hello","version":1}'; exec sleep 60`)
	})
	b.Cfg.AutoSync = true
	start := time.Now()
	b.beforeAttach(true)
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the launch waited %v", d)
	}
	if got := b.errb.String(); !strings.Contains(got, "caboose: not synced: the sync in the sandbox did not finish in time") {
		t.Errorf("said %q", got)
	}
}
