package launcher

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
)

const (
	hostKey   = "ssh-ed25519 AAAAC3NzaHostKey host@example.invalid"
	agentKey  = "ssh-ed25519 AAAAC3NzaAgentKey agent@example.invalid"
	pastedKey = "ssh-ed25519 AAAAC3NzaPastedKey pasted@example.invalid"
)

// setupEnv is an environment for caboose setup: a host whose global git
// config is hostGit, a CABOOSE_HOME under a temp dir, and a docker that
// says the container is running when running is set, its agent holding
// agentKeys.
type setupEnv struct {
	t    *testing.T
	a    *App
	errb *bytes.Buffer
}

func newSetupEnv(t *testing.T, env, hostGit string, running bool, agentKeys ...string) *setupEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	g := filepath.Join(tmp, "host.gitconfig")
	if err := os.WriteFile(g, []byte(hostGit), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	state := "exit 1"
	if running {
		state = "echo running; exit 0"
	}
	keys := "echo 'The agent has no identities.'; exit 1"
	if len(agentKeys) > 0 {
		keys = "printf '%s\\n' '" + strings.Join(agentKeys, "' '") + "'; exit 0"
	}
	script := "#!/bin/sh\ncase \"$1\" in\n  inspect) " + state + " ;;\n  exec) " + keys + " ;;\nesac\nexit 1\n"
	bin := filepath.Join(tmp, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	userHome := filepath.Join(tmp, "home")
	if err := os.MkdirAll(filepath.Join(userHome, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, ".caboose")
	envDir := config.EnvDirFor(home, env)
	var errb bytes.Buffer
	return &setupEnv{t: t, errb: &errb, a: &App{
		Cfg: &config.Config{Env: env, CabooseHome: home, EnvDir: envDir, DataDir: envDir + "/data",
			Container: config.ContainerFor(env), Image: config.ContainerFor(env), Home: userHome,
			Getenv: func(k string) string { return map[string]string{"HOME": userHome, "CABOOSE_HOME": home}[k] }},
		Docker: &docker.CLI{Path: bin},
		Stdout: &bytes.Buffer{}, Stderr: &errb,
	}}
}

// run runs caboose setup with args, answering with answers.
func (e *setupEnv) run(answers string, args ...string) error {
	e.t.Helper()
	e.errb.Reset()
	e.a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(answers)), nil }
	return e.a.Setup(args)
}

func (e *setupEnv) gitConfig() string { return filepath.Join(e.a.Cfg.DataDir, datadir.GitConfig) }

// get reads key from the sandbox's git config.
func (e *setupEnv) get(key string) string {
	out, _ := exec.Command("git", "config", "--file", e.gitConfig(), "--get", key).Output()
	return strings.TrimSpace(string(out))
}

func (e *setupEnv) writeSandbox(data string) {
	e.t.Helper()
	p := e.gitConfig()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *setupEnv) wantKeys(want map[string]string) {
	e.t.Helper()
	for k, v := range want {
		if got := e.get(k); got != v {
			e.t.Errorf("%s = %q, want %q\n%s", k, got, v, e.errb)
		}
	}
}

const hostSigns = "[user]\n\tname = Host Name\n\temail = host@example.invalid\n\tsigningkey = " + hostKey +
	"\n[gpg]\n\tformat = ssh\n[commit]\n\tgpgsign = true\n"

// A new environment: asked for, created, and filled in from the host's
// identity and key, which are only defaults.
func TestSetupCreatesAnEnvironment(t *testing.T) {
	e := newSetupEnv(t, "work", hostSigns, false)
	// create; name as offered; another email; the host's key (the
	// default); sign every commit as the host does.
	if err := e.run("y\n\nwork@example.invalid\n\n\n", "git"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantKeys(map[string]string{
		"user.name":       "Host Name",
		"user.email":      "work@example.invalid",
		"gpg.format":      "ssh",
		"user.signingkey": hostKey,
		"commit.gpgsign":  "true",
	})
	e.wantOut(
		"? There is no environment 'work'. Create it? [y/N] ",
		"✓ Created environment 'work' in "+e.a.Cfg.EnvDir,
		"Kept in "+e.gitConfig()+".",
		"? Name (user.name): [Host Name] ",
		"? Email (user.email): [host@example.invalid] ",
		"The forwarded agent's keys are not listed: the container is not running.",
		"1 The host's signing key: ssh-ed25519 host@example.invalid\n"+
			"2 Paste a public key, or the path of a .pub file on this host\n"+
			"3 None: commits are not signed\n"+
			"choose 1-3 [1]: ",
		"? Sign every commit? (commit.gpgsign) [Y/n] ",
		"✓ Wrote user.name, user.email, gpg.format, user.signingkey, commit.gpgsign",
		"✓ Environment 'work' is set up.",
		"use it caboose -e work (or CABOOSE_ENV=work)",
	)
	b, err := os.ReadFile(filepath.Join(e.a.Cfg.EnvDir, config.FileName))
	if err != nil || string(b) != config.Template {
		t.Errorf("config.toml: %v\n%s", err, b)
	}
}

func TestSetupDeclinedCreatesNothing(t *testing.T) {
	e := newSetupEnv(t, "wrok", hostSigns, false)
	for _, answers := range []string{"\n", "n\n", ""} {
		err := e.run(answers)
		if err == nil {
			t.Fatalf("%q: no error", answers)
		}
		if _, serr := os.Stat(e.a.Cfg.CabooseHome); serr == nil {
			t.Errorf("%q: created %s", answers, e.a.Cfg.CabooseHome)
		}
	}
	if err := e.run("n\n"); err == nil || err.Error() != "nothing was changed" {
		t.Errorf("err = %v", err)
	}
}

// A re-run left at its defaults writes nothing, and hand edits survive.
func TestSetupRerunKeepsWhatIsThere(t *testing.T) {
	e := newSetupEnv(t, "default", hostSigns, true, hostKey)
	e.writeSandbox("[user]\n\tname = Sandbox Me\n\temail = me@example.invalid\n\tsigningkey = key::" + hostKey +
		"\n[gpg]\n\tformat = ssh\n[commit]\n\tgpgsign = false\n[core]\n\teditor = vi # mine\n")
	before, _ := os.ReadFile(e.gitConfig())
	if err := e.run("\n\n\n\n", "git"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	after, _ := os.ReadFile(e.gitConfig())
	if !bytes.Equal(before, after) {
		t.Errorf("rewritten:\n%s", after)
	}
	e.wantOut(
		"? Name (user.name): [Sandbox Me] ", // the sandbox's, not the host's
		"1 Keep the current key: ssh-ed25519 host@example.invalid\n"+
			"2 Paste a public key", // the agent's key is that one: offered once
		"? Sign every commit? (commit.gpgsign) [y/N] ",
		"· Nothing changed",
		"✓ Environment 'default' is set up.",
		"settings "+filepath.Join(e.a.Cfg.EnvDir, config.FileName),
	)
	if e.said("Create it?") || e.said("are not listed") || e.said("use it") {
		t.Errorf("stderr:\n%s", e.errb)
	}
	// An existing config.toml is the user's.
	if err := os.WriteFile(filepath.Join(e.a.Cfg.EnvDir, config.FileName), []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.run("\n\n\n\n", "git"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.a.Cfg.EnvDir, config.FileName)); string(b) != "# mine\n" {
		t.Errorf("config.toml rewritten: %q", b)
	}
}

// Only what changed is written: a new email, a key from the agent, and
// signing every commit turned on.
func TestSetupChangesOnlyAnswers(t *testing.T) {
	e := newSetupEnv(t, "default", "", true, hostKey, agentKey)
	e.writeSandbox("[user]\n\tname = Me\n\temail = old@example.invalid\n")
	// name kept; email changed; the agent's second key (menu: 1 the
	// agent's first, 2 its second, 3 paste, 4 none); sign every commit.
	if err := e.run("\nnew@example.invalid\n2\ny\n", "git"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantKeys(map[string]string{
		"user.name":       "Me",
		"user.email":      "new@example.invalid",
		"gpg.format":      "ssh",
		"user.signingkey": agentKey,
		"commit.gpgsign":  "true",
	})
	e.wantOut("✓ Wrote user.email, gpg.format, user.signingkey, commit.gpgsign\n",
		"4 None: commits are not signed\nchoose 1-4 [4]: ")
}

func TestSetupSigningNone(t *testing.T) {
	e := newSetupEnv(t, "default", hostSigns, false)
	e.writeSandbox("[user]\n\tname = Me\n\temail = me@example.invalid\n\tsigningkey = " + agentKey +
		"\n[gpg]\n\tformat = ssh\n[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n")
	// keep, host key, paste, none: 4.
	if err := e.run("\n\n4\n", "git"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantKeys(map[string]string{"user.name": "Me", "user.signingkey": "", "gpg.format": "", "commit.gpgsign": "", "tag.gpgsign": ""})

	// None again, with nothing to turn off: nothing written.
	if err := e.run("\n\n3\n", "git"); err != nil {
		t.Fatal(err)
	}
	e.wantOut("· Nothing changed")
}

// A pasted key: nonsense is asked again, a private key file explained, an
// empty answer goes back to the list; and a key the agent does not hold is
// said.
func TestSetupPastedKey(t *testing.T) {
	e := newSetupEnv(t, "default", "", true, agentKey)
	priv := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.writeSandbox("[user]\n\tname = Me\n\temail = me@example.invalid\n")
	// menu: 1 agent, 2 paste, 3 none. paste, go back, paste again: junk,
	// a private key, then the key; then do not sign every commit.
	answers := "\n\n2\n\n2\nnot a key\n/nonexistent/k.pub\n" + priv + "\n" + pastedKey + "\nn\n"
	if err := e.run(answers, "git"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantKeys(map[string]string{"user.signingkey": pastedKey, "gpg.format": "ssh", "commit.gpgsign": ""})
	e.wantOut(
		"✗ not an SSH public key (ssh-ed25519 AAAA..., as in a .pub file), nor a path\n",
		"✗ /nonexistent/k.pub cannot be read\n",
		"✗ "+priv+" is a private key file; point it at the .pub, with the key in your agent\n",
		"! The forwarded agent does not hold ssh-ed25519 pasted@example.invalid, so signing fails until it does",
		"? Sign every commit? (commit.gpgsign) [Y/n] ",
	)
	if n := strings.Count(e.errb.String(), "? Sign commits made in the sandbox with\n"); n != 2 {
		t.Errorf("an empty paste did not go back to the list (%d):\n%s", n, e.errb)
	}
}

// Cut short, a section writes nothing, and the environment is not marked
// set up.
func TestSetupStoppedWritesNothing(t *testing.T) {
	e := newSetupEnv(t, "default", hostSigns, false)
	err := e.run("Someone\nsomeone@example.invalid\n", "git")
	if err == nil || !strings.Contains(err.Error(), "setup stopped") {
		t.Fatalf("err = %v", err)
	}
	if e.get("user.name") != "" {
		t.Error("the section was written")
	}
	if _, err := os.Stat(filepath.Join(e.a.Cfg.EnvDir, config.FileName)); err == nil {
		t.Error("config.toml written")
	}
}

// No identity left is said, not made up.
func TestSetupNoIdentity(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	if err := e.run("\n\n\n", "git"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("? Name (user.name): ", "! The sandbox has no user.name or no user.email, so commits in it fail")
	if e.said("Name (user.name): [") {
		t.Errorf("a default was made up:\n%s", e.errb)
	}
}

// A config the container made a link is refused, not written through.
func TestSetupRefusesALinkedConfig(t *testing.T) {
	e := newSetupEnv(t, "default", hostSigns, false)
	if err := datadir.EnsureLayout(e.a.Cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(host, []byte("[core]\n\tx = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.gitConfig()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, e.gitConfig()); err != nil {
		t.Fatal(err)
	}
	err := e.run("\n\n\n\n", "git")
	if err == nil || !strings.Contains(err.Error(), "is a symlink or a hard link") {
		t.Errorf("err = %v", err)
	}
	if b, _ := os.ReadFile(host); string(b) != "[core]\n\tx = 1\n" {
		t.Errorf("the host file is now %q", b)
	}
}

func TestSetupCommand(t *testing.T) {
	for _, tc := range []struct{ env, section, want string }{
		{"default", "", "caboose setup"},
		{"default", "git", "caboose setup git"},
		{"work", "git", "caboose -e work setup git"},
	} {
		if got := SetupCommand(tc.env, tc.section); got != tc.want {
			t.Errorf("SetupCommand(%q, %q) = %q", tc.env, tc.section, got)
		}
	}
}

func TestPrompter(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\nvalue\nmaybe\nY\n9\nx\n2\nlast"), &out)
	if v, err := p.ask("q", "def"); v != "def" || err != nil {
		t.Errorf("ask default = %q, %v", v, err)
	}
	if v, _ := p.ask("q", "def"); v != "value" {
		t.Errorf("ask = %q", v)
	}
	if v, _ := p.yesNo("ok?", false); !v {
		t.Error("yesNo: maybe was taken, or Y was not a yes")
	}
	if i, _ := p.choose("pick", []string{"a", "b"}, 0); i != 1 {
		t.Errorf("choose = %d", i)
	}
	if v, err := p.ask("q", ""); v != "last" || err != nil {
		t.Errorf("a last line without a newline = %q, %v", v, err)
	}
	if _, err := p.ask("q", "def"); err != errNoAnswer {
		t.Errorf("at EOF: %v", err)
	}
	if strings.Count(out.String(), "? ok? [y/N] ") != 2 || strings.Count(out.String(), "choose 1-2 [1]: ") != 3 {
		t.Errorf("out:\n%s", out.String())
	}
}

// A launch says, in one line, what is not set up -- and copies nothing from
// the host, whose identity is right there.
func TestNoteSetup(t *testing.T) {
	const id = "[user]\n\tname = Me\n\temail = me@example.invalid\n"
	for _, tc := range []struct {
		name    string
		setUp   bool
		sandbox string
		want    string
	}{
		{"neither", false, "", "caboose: environment 'work' is not set up, and the sandbox has no git identity to commit with: run 'caboose -e work setup'\n"},
		{"not set up", false, id, "caboose: environment 'work' is not set up, and runs on defaults: 'caboose -e work setup' asks for its settings\n"},
		{"no identity", true, "[user]\n\tname = Me\n", "caboose: the sandbox has no git identity (user.name, user.email), so commits in it fail: run 'caboose -e work setup git'\n"},
		{"both", true, id, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSetupEnv(t, "work", hostSigns, false)
			if tc.sandbox != "" {
				e.writeSandbox(tc.sandbox)
			}
			if tc.setUp {
				e.a.Cfg.File = &config.File{}
			}
			e.a.noteSetup()
			if got := e.errb.String(); got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
			if tc.sandbox == "" {
				if _, err := os.Lstat(e.gitConfig()); err == nil {
					t.Error("a launch wrote the sandbox's git config")
				}
			} else if b, _ := os.ReadFile(e.gitConfig()); string(b) != tc.sandbox {
				t.Errorf("a launch changed the sandbox's git config: %q", b)
			}
		})
	}
}

// commit.gpgsign with no key only makes commits fail: none clears it.
func TestSetupSigningNoneClearsAStrayGpgsign(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeSandbox("[user]\n\tname = Me\n\temail = me@example.invalid\n[commit]\n\tgpgsign = true\n")
	// paste, none: 2 (the default).
	if err := e.run("\n\n\n", "git"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantKeys(map[string]string{"commit.gpgsign": "", "user.name": "Me"})
}
