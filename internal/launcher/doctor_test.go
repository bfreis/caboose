package launcher

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/statesync"
)

// rows renders c's rows alone, one line each, as doctor prints them into
// a pipe.
func (c *checkup) String() string {
	var b bytes.Buffer
	(&checkup{rows: c.rows}).render(newUI(&b, 0), "")
	out, _, _ := strings.Cut(b.String(), "\n\n")
	return out + "\n"
}

// doctorSync is the sync's part of doctor on e, as the report has it.
func (e *autoEnv) doctorSync(remoteWhy string) *checkup {
	e.t.Helper()
	e.ranGit() // from here on, only what doctor runs
	c := &checkup{}
	e.a.doctorSync(c, remoteWhy)
	return c
}

// wantLines checks every line of want is a line of got, in order.
func wantLines(t *testing.T, got *checkup, want ...string) {
	t.Helper()
	s := got.String()
	rest := s
	for _, w := range want {
		i := strings.Index(rest, w+"\n")
		if i < 0 {
			t.Fatalf("no line %q (in order) in:\n%s", w, s)
		}
		rest = rest[i+len(w)+1:]
	}
}

func TestDoctorSyncNotSetUp(t *testing.T) {
	e := newAutoEnv(t, newBare(t))
	e.a.Cfg.AutoSync = ""
	wantLines(t, e.doctorSync(""), "  ✓ sync  not set up ('caboose sync --remote URL' starts it)")
	e.a.Cfg.AutoSync = "1"
	c := e.doctorSync("")
	wantLines(t, c, "  ! sync  auto_sync is on, but no remote is set; 'caboose sync --remote URL' sets one")
	if e.ranGit() {
		t.Error("ran git with no remote to ask")
	}
}

func TestDoctorSyncInStep(t *testing.T) {
	here, _ := pair(t)
	if _, err := here.syncer().Sync(); err != nil {
		t.Fatal(err)
	}
	c := here.doctorSync("")
	wantLines(t, c,
		"  ✓ sync  "+here.remote+", auto_sync on",
		"  ✓ sync  nothing here waiting to be sent",
		"  ✓ sync  nothing new on the remote")
	if n := c.count(levelProblem) + c.count(levelNote); n > 0 {
		t.Errorf("%d problems and notes:\n%s", n, c)
	}
}

func TestDoctorSyncBothWays(t *testing.T) {
	here, there := pair(t)
	here.a.Cfg.AutoSync = ""
	if _, err := here.syncer().Sync(); err != nil {
		t.Fatal(err)
	}
	here.write(filepath.Join(here.data, memRel("p", "mine.md")), "mine\n")
	here.write(filepath.Join(here.data, memRel("p", "more.md")), "more\n")
	there.write(filepath.Join(there.data, memRel("p", "theirs.md")), "theirs\n")
	if _, err := there.syncer().Sync(); err != nil {
		t.Fatal(err)
	}
	head := here.git("rev-parse", "HEAD")
	c := here.doctorSync("")
	key := statesync.ProjectKey("/work/p")
	wantLines(t, c,
		"  ! sync  2 changes here not sent yet (home/.claude/projects/"+key+"/memory/mine.md, home/.claude/projects/"+key+
			"/memory/more.md); 'caboose sync' syncs",
		"  ! sync  the remote has changes not taken yet; 'caboose sync' syncs")
	// Nothing taken, nothing sent: a fetch is all doctor does.
	if got := here.git("rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved: %s -> %s", head, got)
	}
	if got := here.read(memRel("p", "theirs.md")); !strings.HasPrefix(got, "<") {
		t.Errorf("took the remote's file: %q", got)
	}
	if out, err := exec.Command("git", "-C", here.remote, "ls-tree", "-r", "--name-only", statesync.Branch).CombinedOutput(); err != nil ||
		strings.Contains(string(out), "mine.md") {
		t.Errorf("sent this machine's changes (%v):\n%s", err, out)
	}
}

// git runs the host's git in e's sync repo, for a test to look at it.
func (e *autoEnv) git(args ...string) string {
	e.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", filepath.Join(e.data, statesync.Dir)}, args...)...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestDoctorSyncManyChanges(t *testing.T) {
	here, _ := pair(t)
	here.a.Cfg.AutoSync = "1"
	if _, err := here.syncer().Sync(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		here.write(filepath.Join(here.data, memRel("p", f)), f)
	}
	wantLines(t, here.doctorSync("--offline"),
		"  ! sync  5 changes here not sent yet (home/.claude/projects/"+statesync.ProjectKey("/work/p")+
			"/memory/a.md, home/.claude/projects/"+statesync.ProjectKey("/work/p")+"/memory/b.md, home/.claude/projects/"+
			statesync.ProjectKey("/work/p")+"/memory/c.md, and 2 more); the next launch with nothing running syncs, or 'caboose sync'",
		"  – sync  not checked: the remote: --offline")
	if here.ranGit() {
		t.Error("ran git offline")
	}
}

func TestDoctorSyncEmptyRemoteAndUnsent(t *testing.T) {
	e := newAutoEnv(t, newBare(t))
	e.setRemote()
	wantLines(t, e.doctorSync(""), "  ! sync  the remote is empty; 'caboose sync' sends this machine's state")

	here, _ := pair(t)
	if _, err := here.syncer().Sync(); err != nil {
		t.Fatal(err)
	}
	// A sync whose push never got through: its files are in the repo, so
	// nothing is "waiting to be sent", and yet the remote lacks them.
	here.write(filepath.Join(here.data, "home/.claude/agents/a.md"), "a\n")
	here.write(filepath.Join(here.data, statesync.Dir, "home/.claude/agents/a.md"), "a\n")
	here.git("add", "-A")
	here.git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "unsent")
	const unsent = "  ! sync  1 file in 1 commit synced here never reached the remote (a push that failed); 'caboose sync' sends it"
	wantLines(t, here.doctorSync(""), "  ✓ sync  nothing here waiting to be sent", unsent)

	// And when the fetch fails as that push did, it is said all the same.
	here.write(filepath.Join(here.ctl, "hostkey"), "")
	wantLines(t, here.doctorSync(""), unsent, "  ✗ sync  the remote's SSH host key is not accepted yet")
}

func TestDoctorSyncProblems(t *testing.T) {
	budget := doctorBudget
	doctorBudget = time.Second
	defer func() { doctorBudget = budget }()
	for _, tc := range []struct {
		name, ctl, want, fix string
	}{
		{"host key", "hostkey", "  ✗ sync  the remote's SSH host key is not accepted yet",
			"caboose sync, from a terminal: it asks once to accept the key"},
		{"no answer", "slow", "  ✗ sync  no answer from the remote in 1s",
			"check the network (or the remote), then try again"},
		{"no mount", "nomount", "  ✗ sync  the container has no sync mount for this data dir",
			"caboose restart" + endsSessions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			here, _ := pair(t)
			here.write(filepath.Join(here.ctl, tc.ctl), "")
			start := time.Now()
			c := here.doctorSync("")
			if took := time.Since(start); took > 4*time.Second {
				t.Errorf("took %v", took)
			}
			wantLines(t, c, tc.want)
			if c.count(levelProblem) != 1 || c.rows[len(c.rows)-1].fix != tc.fix {
				t.Errorf("fix %q, want %q:\n%s", c.rows[len(c.rows)-1].fix, tc.fix, c)
			}
		})
	}
	t.Run("other failure", func(t *testing.T) {
		e := newAutoEnv(t, filepath.Join(t.TempDir(), "gone.git"))
		e.setRemote()
		c := e.doctorSync("")
		if c.count(levelProblem) != 1 || !strings.Contains(c.String(), "  ✗ sync  cannot fetch from the remote: ") ||
			strings.Contains(c.String(), "git fetch -q origin:") {
			t.Errorf("\n%s", c)
		}
	})
}

func TestDoctorSyncSecretsAndLinks(t *testing.T) {
	here, _ := pair(t)
	here.write(filepath.Join(here.data, memRel("p", "leak.md")), "token ghp_"+strings.Repeat("a", 36)+"\n")
	dir := filepath.Join(here.data, filepath.Dir(memRel("q", "x")))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	c := here.doctorSync("--offline")
	wantLines(t, c,
		"  ✗ sync  refusing to sync; these look like they hold a credential: home/.claude/projects/"+
			statesync.ProjectKey("/work/p")+"/memory/leak.md (remove it and sync again; nothing was committed)",
		"  ! sync  not synced, being symlinks or hard links (never followed): home/.claude/projects/"+
			statesync.ProjectKey("/work/q")+"/memory")
}

func TestDoctorSyncWhileSyncing(t *testing.T) {
	here, _ := pair(t)
	unlock, err := here.syncer().Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	c := here.doctorSync("")
	if time.Since(start) > time.Second {
		t.Error("waited for the lock")
	}
	if c.String() != "  ! sync  a sync is running; not checked\n" || here.ranGit() {
		t.Errorf("\n%s", c)
	}
}

// signingEnv is a host whose global git config is global, and a data dir
// whose sandbox git config is sandbox ("" for none).
func signingEnv(t *testing.T, global, sandbox string) *App {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	g := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(g, []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	data := t.TempDir()
	if sandbox != "" {
		p := filepath.Join(data, datadir.GitConfig)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(sandbox), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &App{Cfg: &config.Config{Env: "work", Container: "box", DataDir: data}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
}

func TestDoctorSigning(t *testing.T) {
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHv0pK3l me@example.invalid"
	hostSSH := "[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = " + pub + "\n"
	sandbox := func(sign string) string {
		return "[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = key::" + pub + "\n[commit]\n\tgpgsign = " + sign + "\n"
	}
	agent := []string{"ssh-rsa AAAAother other", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHv0pK3l agent-comment"}
	for _, tc := range []struct {
		name, global, sandbox string
		keys                  []string
		why, want             string
	}{
		{"not set up", "", "", nil, "", "  ✓ signing  not set up; commits in the sandbox are not signed"},
		{"host signs, the sandbox does not", hostSSH, "", nil, "",
			"  ! signing  the host signs commits with SSH, the sandbox does not; 'caboose -e work setup git' sets it up"},
		{"host key cannot come over", "[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = /nonexistent/id_ed25519\n", "", nil, "",
			"  ! signing  the host signs commits with SSH, but the sandbox will not: user.signingkey /nonexistent/id_ed25519 cannot be read"},
		{"in the agent", hostSSH, sandbox("true"), agent, "",
			"  ✓ signing  SSH, with ssh-ed25519 me@example.invalid, which the agent holds"},
		{"not in the agent, signing every commit", hostSSH, sandbox("true"), agent[:1], "",
			"  ✗ signing  the sandbox signs every commit with ssh-ed25519 me@example.invalid, which the forwarded agent does not hold: commits fail"},
		{"not in the agent, signing on request", hostSSH, sandbox("false"), agent[:1], "",
			"  ! signing  user.signingkey ssh-ed25519 me@example.invalid is not in the forwarded agent: a signed commit (git commit -S) fails"},
		{"agent unknown", hostSSH, sandbox("true"), nil, "the container is not running",
			"  – signing  not checked: whether the agent holds ssh-ed25519 me@example.invalid: the container is not running"},
		{"a key file", "", "[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = ~/.ssh/id.pub\n", agent, "",
			"  – signing  not checked: the sandbox signs with the key file ~/.ssh/id.pub, which only the sandbox can read"},
		{"gpg", "", "[user]\n\tsigningkey = ABCD1234\n", agent, "",
			"  – signing  not checked: the sandbox signs with gpg.format 'openpgp', which doctor does not check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := signingEnv(t, tc.global, tc.sandbox)
			c := &checkup{}
			a.doctorSigning(c, tc.keys, tc.why)
			if got := c.String(); got != tc.want+"\n" {
				t.Errorf("got\n%swant\n%s", got, tc.want)
			}
		})
	}
}

func TestHolds(t *testing.T) {
	for _, tc := range []struct {
		keys []string
		pub  string
		want bool
	}{
		{[]string{"ssh-ed25519 AAAA x"}, "ssh-ed25519 AAAA y", true},
		{[]string{"ssh-ed25519 AAAB x"}, "ssh-ed25519 AAAA x", false},
		{[]string{"ssh-rsa AAAA x"}, "ssh-ed25519 AAAA x", false},
		{[]string{""}, "ssh-ed25519", false},
		{nil, "ssh-ed25519 AAAA", false},
	} {
		if got := holds(tc.keys, tc.pub); got != tc.want {
			t.Errorf("holds(%q, %q) = %v", tc.keys, tc.pub, got)
		}
	}
}

func TestPrintable(t *testing.T) {
	if got := printable("url\x1b]0;x\x07 \u009bok é"); got != "url]0;x ok é" {
		t.Errorf("printable = %q", got)
	}
}

// The report's text is stripped of control characters, a fix's included:
// some of it comes from files the container writes.
func TestDoctorRenderIsPrintable(t *testing.T) {
	c := &checkup{}
	c.ok("sync", "git@example.invalid:x.git\x1b]0;owned\x07")
	c.problem("sync", "run\x1b[2J this", "cannot fetch: \x1b[31mred")
	var b bytes.Buffer
	c.render(newUI(&b, 0), "")
	want := "  ✓ sync  git@example.invalid:x.git]0;owned\n" +
		"  ✗ sync  cannot fetch: [31mred\n\n" +
		"  ✗ 1 problem. To fix:\n" +
		"    sync  run[2J this\n"
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
}

// On a terminal, text wraps under its column, a repeated label shows once,
// and home is ~; the fixes keep every label.
func TestDoctorRenderOnATerminal(t *testing.T) {
	c := &checkup{}
	c.ok("data dir", "/home/me/.caboose/envs/default/data")
	c.note("data dir", "shared with /home/me/.caboose/envs/work, by CABOOSE_DATA_DIR; both environments see the same login")
	c.unchecked("claude", "the container is not running")
	c.problem("sync", "caboose sync", "cannot fetch")
	var b bytes.Buffer
	c.render(newUI(&b, 60), "/home/me")
	want := "  ✓ data dir  ~/.caboose/envs/default/data\n" +
		"  !           shared with ~/.caboose/envs/work, by\n" +
		"              CABOOSE_DATA_DIR; both environments see the\n" +
		"              same login\n" +
		"  – claude    not checked: the container is not running\n" +
		"  ✗ sync      cannot fetch\n\n" +
		"  ✗ 1 problem. To fix:\n" +
		"    sync      caboose sync\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestDoctorIdentity(t *testing.T) {
	for _, tc := range []struct{ name, sandbox, want, fix string }{
		{"none", "", "  ✗ git  no git identity in the sandbox (user.name and user.email unset): commits in it fail",
			"caboose -e work setup git"},
		{"no email", "[user]\n\tname = Me\n", "  ✗ git  no git identity in the sandbox (user.email unset): commits in it fail",
			"caboose -e work setup git"},
		{"both", "[user]\n\tname = Me\n\temail = me@example.invalid\n", "  ✓ git  Me <me@example.invalid>", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The host's identity is no stand-in for the sandbox's.
			a := signingEnv(t, "[user]\n\tname = Host\n\temail = host@example.invalid\n", tc.sandbox)
			c := &checkup{}
			a.doctorIdentity(c)
			if got := c.String(); got != tc.want+"\n" {
				t.Errorf("got\n%swant\n%s", got, tc.want)
			}
			if len(c.rows) != 1 || c.rows[0].fix != tc.fix {
				t.Errorf("fix %+v, want %q", c.rows, tc.fix)
			}
		})
	}
}
