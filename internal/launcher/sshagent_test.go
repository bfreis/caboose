package launcher

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/docker"
)

// agentApp is an App on a fake docker whose script body is given, a fake
// ssh printing sshG for `ssh -G` (none when it is ""), and the environment
// env, whose HOME is the fake ssh's home.
func agentApp(t *testing.T, os_, body, sshG string, env map[string]string) (*App, *bytes.Buffer) {
	t.Helper()
	old := goos
	goos = os_
	t.Cleanup(func() { goos = old })
	bin := shortTempDir(t)
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("docker", body+"\nexit 1")
	t.Setenv("PATH", bin)
	var errb bytes.Buffer
	a := &App{
		Cfg:    &config.Config{Container: "box", Getenv: func(k string) string { return env[k] }},
		Docker: &docker.CLI{Path: filepath.Join(bin, "docker")},
		Stdout: &bytes.Buffer{}, Stderr: &errb,
	}
	if sshG != "" {
		a.SSH = fakeSSH(sshG, nil, env["HOME"], nil)
	}
	return a, &errb
}

// noSSH is the tests' machine: no ssh, and no ~/.ssh to read.
var noSSH = fakeSSH("", errors.New("no ssh on PATH"), "/nonexistent-home", nil)

// fakeSSH is an SSHLookup whose ssh -G prints out, or fails with gerr,
// in home, with files (by absolute path) the only files there are.
func fakeSSH(out string, gerr error, home string, files map[string]string) *SSHLookup {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[strings.TrimPrefix(name, "/")] = &fstest.MapFile{Data: []byte(body)}
	}
	return &SSHLookup{
		G: func(string) ([]byte, error) {
			if gerr != nil {
				return nil, gerr
			}
			return []byte(out + "\n"), nil
		},
		Home: func() (string, error) { return home, nil },
		FS:   fsys,
	}
}

// listen makes a real unix socket, as an agent's is.
func listen(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return path
}

const onePasswordSock = "/Users/me/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"

func info(os string) string {
	return `[ "$*" = "info --format {{.OperatingSystem}}" ] && { echo "` + os + `"; exit 0; }`
}

// On a Mac, OrbStack and Docker Desktop get their own forwarded socket,
// whatever the host's agent is -- 1Password's above all, which a direct
// mount turns into an empty directory.
func TestSSHAgentSourceMac(t *testing.T) {
	for _, engine := range []string{"OrbStack", "Docker Desktop"} {
		t.Run(engine, func(t *testing.T) {
			a, _ := agentApp(t, "darwin", info(engine), "", map[string]string{"SSH_AUTH_SOCK": onePasswordSock})
			if got := a.sshAgentSource(); got != hostServicesAgent {
				t.Errorf("args %q", got)
			}
		})
	}
	t.Run("another engine: the host's socket, if it is one", func(t *testing.T) {
		sock := listen(t, filepath.Join(shortTempDir(t), "a.sock"))
		a, _ := agentApp(t, "darwin", info("Ubuntu 24.04 LTS"), "", map[string]string{"SSH_AUTH_SOCK": sock})
		if got := a.sshAgentSource(); got != sock {
			t.Errorf("args %q", got)
		}
		a, _ = agentApp(t, "darwin", info("Ubuntu 24.04 LTS"), "", map[string]string{"SSH_AUTH_SOCK": onePasswordSock})
		if got := a.sshAgentSource(); got != "" {
			t.Errorf("mounted a path that is no socket: %q", got)
		}
	})
}

// On a Mac engine no agent asked for outright is no agent: ssh_agent =
// "none", or an IdentityAgent none for every host, leaves the engine's
// forwarding out. A socket named, or nothing said, still gets the engine's
// own, which is the only one it can forward.
func TestSSHAgentSourceMacDeclined(t *testing.T) {
	for _, tc := range []struct {
		name, setting, sshG string
		ssh                 *SSHLookup
		want                string
	}{
		{"ssh_agent none", "none", "identityagent " + onePasswordSock, nil, ""},
		{"unset", "", "user me", nil, hostServicesAgent},
		{"unset, ssh fails", "", "", noSSH, hostServicesAgent},
		{"a socket named", onePasswordSock, "identityagent none", nil, hostServicesAgent},
		{"a variable naming nothing", "$NOPE", "", noSSH, hostServicesAgent},
		{"IdentityAgent none", "", "identityagent none", nil, ""},
		{"IdentityAgent none, read directly", "", "", fakeSSH("", errors.New("no ssh on PATH"), "/h", map[string]string{"/h/.ssh/config": "IdentityAgent none\n"}), ""},
		{"IdentityAgent none for one host only", "", "user me", fakeSSH("", errors.New("no ssh on PATH"), "/h", map[string]string{"/h/.ssh/config": "Host github.com\n  IdentityAgent none\n"}), hostServicesAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := agentApp(t, "darwin", info("OrbStack"), tc.sshG, map[string]string{"SSH_AUTH_SOCK": onePasswordSock})
			if tc.ssh != nil {
				a.SSH = tc.ssh
			}
			a.Cfg.SSHAgent = tc.setting
			if tc.setting != "" {
				a.Cfg.File = &config.File{Path: "/e/config.toml", Vals: map[string]any{"link.ssh_agent": tc.setting}}
			}
			if got := a.sshAgentSource(); got != tc.want {
				t.Errorf("mounts %q, want %q", got, tc.want)
			}
		})
	}
}

// A container or gvisor sandbox on a Mac engine is created with the
// engine's agent, mounted and named by SSH_AUTH_SOCK, unless ssh_agent is
// "none", or unset with an IdentityAgent none for every host: then with
// neither. A socket ssh_agent names still gets the engine's.
func TestCreateContainerMacAgent(t *testing.T) {
	for _, iso := range []string{isolationContainer, isolationGVisor} {
		for _, tc := range []struct {
			name, setting, sshG string
			want                bool
		}{
			{"unset", "", "user me", true},
			{"none", "none", "user me", false},
			{"a socket", onePasswordSock, "user me", true},
			{"IdentityAgent none", "", "identityagent none", false},
			{"IdentityAgent none, beside ssh_agent naming a socket", onePasswordSock, "identityagent none", true},
		} {
			t.Run(iso+"/"+tc.name, func(t *testing.T) {
				b := creating(t, iso, currentLabels("linux-arm64"))
				pinHost(t, true)
				b.write(t, "os", "OrbStack")
				b.SSH = fakeSSH(tc.sshG, nil, "/h", nil)
				b.Cfg.SSHAgent = tc.setting
				if tc.setting != "" {
					b.Cfg.File = &config.File{Path: "/e/config.toml", Vals: map[string]any{"link.ssh_agent": tc.setting}}
				}
				if err := b.createContainer(false); err != nil {
					t.Fatalf("%v\n%s", err, b.errb)
				}
				spec, _ := b.box.Spec()
				mounted := slices.Contains(spec.Mounts, backend.Mount{Source: hostServicesAgent, Target: containerAgent})
				env := slices.Contains(spec.Env, "SSH_AUTH_SOCK="+containerAgent)
				if mounted != tc.want || env != tc.want {
					t.Errorf("mounted %v, SSH_AUTH_SOCK %v, want %v: mounts %v, env %q", mounted, env, tc.want, spec.Mounts, spec.Env)
				}
				for _, m := range spec.Mounts {
					if m.Target == containerAgent && !tc.want {
						t.Errorf("an agent mounted: %+v", m)
					}
				}
				for _, e := range spec.Env {
					if strings.HasPrefix(e, "SSH_AUTH_SOCK=") && !tc.want {
						t.Errorf("env %q", e)
					}
				}
			})
		}
	}
}

// On Linux the agent is mounted directly: the one ssh would use, which an
// IdentityAgent in ~/.ssh/config (1Password's documented setup) picks over
// $SSH_AUTH_SOCK.
func TestSSHAgentSourceLinux(t *testing.T) {
	home := shortTempDir(t)
	op := listen(t, filepath.Join(home, ".1password", "agent.sock"))
	env := listen(t, filepath.Join(shortTempDir(t), "env.sock"))
	for _, tc := range []struct {
		name, sshG string
		want       string
	}{
		{"IdentityAgent under ~", "identityagent ~/.1password/agent.sock", op},
		{"IdentityAgent quoted", `identityagent "` + op + `"`, op},
		{"IdentityAgent SSH_AUTH_SOCK", "identityagent SSH_AUTH_SOCK", env},
		{"IdentityAgent $SSH_AUTH_SOCK", "identityagent $SSH_AUTH_SOCK", env},
		{"IdentityAgent none", "identityagent none", ""},
		{"no IdentityAgent", "user me", env},
		{"no ssh at all", "", env},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := agentApp(t, "linux", "", tc.sshG, map[string]string{"SSH_AUTH_SOCK": env, "HOME": home})
			if got := a.sshAgentSource(); got != tc.want {
				t.Errorf("args %q, want %q", got, tc.want)
			}
		})
	}
}

// statusBody answers the container's SSH_AUTH_SOCK with sock, and
// `ssh-add -l` with its output and exit code.
func statusBody(sock, keys string, code int) string {
	return `case "$*" in
  "exec box bash -c printf '%s' \"\${SSH_AUTH_SOCK-}\"") printf '%s' "` + sock + `"; exit 0 ;;
  "exec box ssh-add -l") printf '%s' "` + keys + `"; exit ` + strconv.Itoa(code) + ` ;;
esac`
}

func TestAgentStatus(t *testing.T) {
	for _, tc := range []struct {
		name, sock, keys string
		code             int
		want             string
		usable           bool
	}{
		{"two keys", containerAgent, "256 SHA256:a me (ED25519)\n256 SHA256:b you (ED25519)\n", 0, "forwarded, 2 keys", true},
		{"one key", containerAgent, "256 SHA256:a me (ED25519)\n", 0, "forwarded, 1 key", true},
		{"empty", containerAgent, "The agent has no identities.\n", 1, "forwarded, but the agent holds no keys", false},
		{"unreachable", containerAgent, "", 2, "forwarded, but not reachable from the container", false},
		{"no ssh-add", containerAgent, "", 127, "forwarded (not checked: no ssh-add in the image)", true},
		{"none", "", "", 0, "not forwarded (the host had no agent when the container was created)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := agentApp(t, "linux", statusBody(tc.sock, tc.keys, tc.code), "", nil)
			got, usable := a.agentStatus()
			if got != tc.want || usable != tc.usable {
				t.Errorf("agentStatus = %q, %v; want %q, %v", got, usable, tc.want, tc.usable)
			}
		})
	}
}

// The warning at creation names the fix on a Mac, and says nothing when
// the agent works or there is none to forward.
func TestWarnIfAgentUnusable(t *testing.T) {
	a, errb := agentApp(t, "darwin", info("OrbStack")+"\n"+statusBody(containerAgent, "The agent has no identities.\n", 1), "", nil)
	a.warnIfAgentUnusable()
	for _, w := range []string{"SSH agent: forwarded, but the agent holds no keys.", "OrbStack forwards the agent it was started with",
		"Configure SSH_AUTH_SOCK globally", "then restart OrbStack"} {
		if !strings.Contains(errb.String(), w) {
			t.Errorf("lacks %q:\n%s", w, errb.String())
		}
	}
	for _, body := range []string{
		statusBody(containerAgent, "256 SHA256:a me (ED25519)\n", 0),
		statusBody("", "", 0),
	} {
		a, errb := agentApp(t, "darwin", info("OrbStack")+"\n"+body, "", nil)
		a.warnIfAgentUnusable()
		if errb.Len() != 0 {
			t.Errorf("warned:\n%s", errb.String())
		}
	}
}

// agentMount answers inspect with the forwarded agent's mount from src.
func agentMount(src string) string {
	return `[ "$1 $2" = "inspect --type=container" ] && { printf '%s\t%s\n' ` + containerAgent + ` "` + src + `"; exit 0; }`
}

// Docker Desktop's forwarded agent is root:root 0660: the launch opens it
// to the sandbox's user, and only when that user cannot write it already.
func TestOpenAgentSocket(t *testing.T) {
	const chmod = "exec -u 0 box chmod 0666 " + containerAgent
	for _, tc := range []struct {
		name, src string
		writable  bool
		want      bool
	}{
		{"closed", hostServicesAgent, false, true},
		{"already open", hostServicesAgent, true, false},
		{"the host's own socket", "/tmp/agent.sock", false, false},
		{"no agent mounted", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(shortTempDir(t), "log")
			body := `echo "$*" >> ` + log + "\n"
			if tc.src != "" {
				body += agentMount(tc.src) + "\n"
			}
			if tc.writable {
				body += `[ "$*" = "exec box test -w ` + containerAgent + `" ] && exit 0` + "\n"
			}
			body += `[ "$*" = "` + chmod + `" ] && exit 0`
			a, errb := agentApp(t, "darwin", body, "", nil)
			a.openAgentSocket()
			got, _ := os.ReadFile(log)
			if strings.Contains(string(got), chmod) != tc.want {
				t.Errorf("chmod run: %v, want %v; docker calls:\n%s", !tc.want, tc.want, got)
			}
			if errb.Len() != 0 {
				t.Errorf("said:\n%s", errb.String())
			}
		})
	}
}

// A chmod that fails is said, not swallowed: the agent stays unusable.
func TestOpenAgentSocketSaysWhenItCannot(t *testing.T) {
	a, errb := agentApp(t, "darwin", agentMount(hostServicesAgent)+"\necho 'chmod: Operation not permitted' >&2", "", nil)
	a.openAgentSocket()
	if !strings.Contains(errb.String(), "Operation not permitted") {
		t.Errorf("said:\n%s", errb.String())
	}
}

// Permission denied is its own status, with its own fix: the engine's
// SSH_AUTH_SOCK is not what is wrong.
func TestAgentStatusPermissionDenied(t *testing.T) {
	body := `[ "$*" = "exec box ssh-add -l" ] && { echo 'Error connecting to agent: Permission denied' >&2; exit 2; }` +
		"\n" + statusBody(containerAgent, "", 0)
	a, _ := agentApp(t, "darwin", info("Docker Desktop")+"\n"+body, "", nil)
	status, usable := a.agentStatus()
	if usable || !strings.Contains(status, "permission denied") {
		t.Fatalf("agentStatus = %q, %v", status, usable)
	}
	if fix := a.agentFix(status); strings.Contains(fix, "SSH_AUTH_SOCK") || !strings.Contains(fix, "a launch opens") {
		t.Errorf("fix %q", fix)
	}
}

// ssh_agent wins over what ssh would use -- an IdentityAgent, and an
// $SSH_AUTH_SOCK a work login may have taken over -- and "none" gives the
// sandbox none; each choice says what made it. On a Mac engine it does
// nothing: the engine forwards the agent it was started with.
func TestSSHAgentSetting(t *testing.T) {
	home := shortTempDir(t)
	op := listen(t, filepath.Join(home, ".1password", "agent.sock"))
	env := listen(t, filepath.Join(shortTempDir(t), "env.sock"))
	set := listen(t, filepath.Join(shortTempDir(t), "set.sock"))
	for _, tc := range []struct {
		name, sshG, setting string
		want, from          string
	}{
		{"unset: ssh's IdentityAgent", "identityagent " + op, "", op, "IdentityAgent in ~/.ssh/config"},
		{"unset: $SSH_AUTH_SOCK", "user me", "", env, "$SSH_AUTH_SOCK"},
		{"set: over IdentityAgent", "identityagent " + op, set, set, "ssh_agent in [link] of /e/config.toml"},
		{"set: over $SSH_AUTH_SOCK", "user me", set, set, "ssh_agent in [link] of /e/config.toml"},
		{"set: none", "identityagent " + op, "none", "", "ssh_agent in [link] of /e/config.toml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := agentApp(t, "linux", "", tc.sshG, map[string]string{"SSH_AUTH_SOCK": env, "HOME": home})
			a.Cfg.SSHAgent = tc.setting
			if tc.setting != "" {
				a.Cfg.File = &config.File{Path: "/e/config.toml", Vals: map[string]any{"link.ssh_agent": tc.setting}}
			}
			if sock, from := a.hostAgent(); sock != tc.want || from != tc.from {
				t.Errorf("hostAgent %q (%s), want %q (%s)", sock, from, tc.want, tc.from)
			}
			if got := a.sshAgentSource(); got != tc.want {
				t.Errorf("mounts %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("a Mac engine forwards its own", func(t *testing.T) {
		a, _ := agentApp(t, "darwin", info("Docker Desktop"), "", map[string]string{"SSH_AUTH_SOCK": env})
		a.Cfg.SSHAgent = set
		a.Cfg.File = &config.File{Path: "/e/config.toml", Vals: map[string]any{"link.ssh_agent": set}}
		if got := a.sshAgentSource(); got != hostServicesAgent {
			t.Errorf("mounts %q", got)
		}
		c := &checkup{}
		if said := a.agentSourceSaid(c); said != "" || !rowsSay(c, "ssh_agent in [link] of /e/config.toml is not used: Docker Desktop forwards the agent it was started with") {
			t.Errorf("said %q, rows %+v", said, c.rows)
		}
	})
}

// rowsSay is whether some finding of c says text.
func rowsSay(c *checkup, text string) bool {
	for _, r := range c.rows {
		if strings.Contains(r.text, text) {
			return true
		}
	}
	return false
}

// Under vm the daemon's link carries the agent it found through a login
// shell: the doctor row never names this terminal's SSH_AUTH_SOCK as the
// sandbox's, unless a detached helper (CABOOSE_DATA_DIR set) took it.
func TestAgentSourceUnderVMIsTheDaemons(t *testing.T) {
	const term = "/terminal/agent.sock"
	a, _ := agentApp(t, "linux", "", "", map[string]string{"SSH_AUTH_SOCK": term})
	a.Cfg.Isolation = isolationVM
	said := a.agentSourceSaid(&checkup{})
	if strings.Contains(said, term) || !strings.Contains(said, "the daemon's") || !strings.Contains(said, "here no ssh on PATH)") {
		t.Errorf("served by the daemon: %q", said)
	}
	a, _ = agentApp(t, "linux", "", "", map[string]string{"SSH_AUTH_SOCK": term, "CABOOSE_DATA_DIR": "/d"})
	a.Cfg.Isolation = isolationVM
	if said := a.agentSourceSaid(&checkup{}); !strings.Contains(said, term) {
		t.Errorf("a detached helper's: %q", said)
	}
}

// When ssh -G fails -- a Homebrew ssh first on PATH refusing the Mac's
// UseKeychain, say -- ~/.ssh/config is read for the IdentityAgent ssh
// would take by default, and what chose the agent says why.
func TestAgentFromConfigFallback(t *testing.T) {
	const (
		home = "/Users/me"
		cfg  = home + "/.ssh/config"
		env  = "/env.sock"
		bad  = "ssh -G caboose-default-agent.invalid failed: /Users/me/.ssh/config line 2: Bad configuration option: usekeychain"
		op1  = `IdentityAgent "~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"`
	)
	direct := agentFromConfig + ", read directly: " + bad
	for _, tc := range []struct {
		name       string
		files      map[string]string
		want, from string
	}{
		{"1Password under Host *", map[string]string{cfg: "Host *\n\tUseKeychain yes\n\t" + op1 + "\n"}, onePasswordSock, direct},
		{"before any Host", map[string]string{cfg: op1 + "\nHost other\n  IdentityAgent /o.sock\n"}, onePasswordSock, direct},
		{"OrbStack's Include first", map[string]string{
			cfg:                            "Include ~/.orbstack/ssh/config\n\nHost *\n  " + op1 + "\n",
			home + "/.orbstack/ssh/config": "Host orb\n  IdentityAgent /orb.sock\n",
		}, onePasswordSock, direct},
		{"an Include that applies", map[string]string{
			cfg:                       "Include conf.d/*\nHost *\n  IdentityAgent /all.sock\n",
			home + "/.ssh/conf.d/gh":  "Host caboose-default-agent.invalid\n  IdentityAgent /gh.sock\n",
			home + "/.ssh/conf.d/zzz": "IdentityAgent /later.sock\n",
		}, "/gh.sock", direct},
		{"an Include in a block that does not apply", map[string]string{
			cfg:                  "Host other\n  Include extra\nHost *\n  IdentityAgent /all.sock\n",
			home + "/.ssh/extra": "IdentityAgent /x.sock\n",
		}, "/all.sock", direct},
		{"negated, and key=value", map[string]string{cfg: "Host * !*.invalid\n  IdentityAgent /neg.sock\nhost *\n  identityagent=/all.sock\n"}, "/all.sock", direct},
		{"the first value wins", map[string]string{cfg: "Host caboose-default-agent.inval??\n  IdentityAgent /a.sock\nHost *\n  IdentityAgent /b.sock\n"}, "/a.sock", direct},
		{"a comment after it", map[string]string{cfg: "Host *\n  IdentityAgent /c.sock # 1Password\n"}, "/c.sock", direct},
		{"%d", map[string]string{cfg: "IdentityAgent %d/.agent.sock\n"}, home + "/.agent.sock", direct},
		{"none", map[string]string{cfg: "IdentityAgent none\n"}, "", agentFromNone + ", read directly: " + bad},
		{"a value before a Match", map[string]string{cfg: "Host *\n  IdentityAgent /first.sock\nMatch all\n  IdentityAgent /m.sock\n"}, "/first.sock", direct},
		{"a Match before any value", map[string]string{cfg: "Host *\n  User git\nMatch host github.com\n  IdentityAgent /m.sock\n"}, env,
			agentFromEnv + " (" + bad + "; ~/.ssh/config line 3: Match, which only ssh can decide)"},
		{"a token only ssh expands", map[string]string{cfg: "IdentityAgent /run/%h.sock\n"}, env,
			agentFromEnv + " (" + bad + `; ~/.ssh/config: IdentityAgent "/run/%h.sock" has a % token only ssh expands)`},
		{"no IdentityAgent", map[string]string{cfg: "Host *\n  User git\n"}, env, agentFromEnv + " (" + bad + ")"},
		{"a github.com block's none is github.com's", map[string]string{cfg: githubNoneConfig}, onePasswordSock, direct},
		{"IdentityAgent none for every host", map[string]string{cfg: "Host *\n  IdentityAgent none\n"}, "", agentFromNone + ", read directly: " + bad},
		{"Host patterns keep their case", map[string]string{cfg: "Host Caboose-Default-Agent.invalid\n  IdentityAgent /case.sock\nHost *\n  IdentityAgent /all.sock\n"}, "/all.sock", direct},
		{"an Include's * skips dot files", map[string]string{
			cfg:                           "Include conf.d/*\nHost *\n  IdentityAgent /all.sock\n",
			home + "/.ssh/conf.d/.hidden": "IdentityAgent /hidden.sock\n",
		}, "/all.sock", direct},
		{"%% is a literal %", map[string]string{cfg: "IdentityAgent /run/%%d.sock\n"}, "/run/%d.sock", direct},
		{"a file too large", map[string]string{cfg: strings.Repeat("# filler\n", 30000) + "IdentityAgent /big.sock\n"}, env,
			agentFromEnv + " (" + bad + "; ~/.ssh/config: larger than 256 KiB)"},
		{"no config", nil, env, agentFromEnv + " (" + bad + ")"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := agentApp(t, "darwin", "", "", map[string]string{"SSH_AUTH_SOCK": env})
			a.SSH = fakeSSH("", errors.New(bad), home, tc.files)
			if sock, from := a.hostAgent(); sock != tc.want || from != tc.from {
				t.Errorf("hostAgent %q (%s)\nwant      %q (%s)", sock, from, tc.want, tc.from)
			}
		})
	}
	// ssh -G answers: its word is final, its unquoted path with spaces
	// whole, and the file is not read.
	t.Run("ssh -G's answer", func(t *testing.T) {
		files := map[string]string{cfg: "IdentityAgent /file.sock\n"}
		a, _ := agentApp(t, "darwin", "", "", map[string]string{"SSH_AUTH_SOCK": env})
		a.SSH = fakeSSH("user me\nidentityagent "+onePasswordSock, nil, home, files)
		if sock, from := a.hostAgent(); sock != onePasswordSock || from != agentFromConfig {
			t.Errorf("hostAgent %q (%s)", sock, from)
		}
		a.SSH = fakeSSH("user me", nil, home, files)
		if sock, from := a.hostAgent(); sock != env || from != agentFromEnv {
			t.Errorf("hostAgent %q (%s)", sock, from)
		}
	})
}

// Doctor's ssh agent row keeps why ssh -G named no agent, with no
// socket found as with one.
func TestAgentSourceSaidReason(t *testing.T) {
	a, _ := agentApp(t, "linux", "", "", nil)
	if said := a.agentSourceSaid(&checkup{}); said != " (no ssh on PATH)" {
		t.Errorf("no agent: %q", said)
	}
	a, _ = agentApp(t, "linux", "", "", map[string]string{"SSH_AUTH_SOCK": "/e.sock"})
	if said := a.agentSourceSaid(&checkup{}); said != ", /e.sock ($SSH_AUTH_SOCK (no ssh on PATH))" {
		t.Errorf("$SSH_AUTH_SOCK: %q", said)
	}
	a.SSH = fakeSSH("user me", nil, "/h", nil)
	if said := a.agentSourceSaid(&checkup{}); said != ", /e.sock ($SSH_AUTH_SOCK)" {
		t.Errorf("ssh -G answered: %q", said)
	}
}

// runSSHG says why ssh -G gave nothing: no ssh, or ssh's first line of
// stderr.
func TestRunSSHG(t *testing.T) {
	bin := shortTempDir(t)
	t.Setenv("PATH", bin)
	if _, err := runSSHG(agentProbeHost); err == nil || err.Error() != "no ssh on PATH" {
		t.Errorf("no ssh: %v", err)
	}
	script := "#!/bin/sh\n[ \"$*\" = \"-G caboose-default-agent.invalid\" ] || exit 2\n" +
		"echo '/Users/me/.ssh/config: line 2: Bad configuration option: usekeychain' >&2\n" +
		"echo '/Users/me/.ssh/config: terminating, 1 bad configuration options' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(bin, "ssh") + " -G caboose-default-agent.invalid failed: /Users/me/.ssh/config: line 2: Bad configuration option: usekeychain"
	if _, err := runSSHG(agentProbeHost); err == nil || err.Error() != want {
		t.Errorf("failing ssh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\necho 'identityagent /a b.sock'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runSSHG(agentProbeHost)
	if v, ok := sshGIdentityAgent(out); err != nil || !ok || v != "/a b.sock" {
		t.Errorf("ssh -G: %q %v", out, err)
	}
}

// Under vm the link carries the agent: with the daemon not answering,
// status cannot tell whether the agent is fine, and says so, as its other
// lines do; doctor leaves the row unchecked rather than call the agent
// unreachable. With the link up, an unreachable agent is one.
func TestAgentStatusUnderVMWithoutDaemon(t *testing.T) {
	box := runningBox(isolationVM)
	box.Exec = func(s backend.ExecSpec) *exec.Cmd {
		switch {
		case len(s.Argv) == 3 && s.Argv[0] == "bash" && strings.Contains(s.Argv[2], "SSH_AUTH_SOCK"):
			return exec.Command("printf", "%s", containerAgent)
		case len(s.Argv) > 0 && s.Argv[0] == "ssh-add":
			return exec.Command("sh", "-c", "exit 2")
		}
		return nil
	}
	b := newBoxApp(t, isolationVM, box)
	b.daemon.Stat = nil // no daemon answers
	if status, usable := b.agentStatus(); status != agentUnknownNoDaemon || usable {
		t.Errorf("no daemon: %q %v", status, usable)
	}
	c := &checkup{}
	b.doctorInside(c)
	if !rowsSay(c, agentUnknownNoDaemon+": under vm the link carries the agent") {
		t.Errorf("doctor: %+v", c.rows)
	}
	for _, r := range c.rows {
		if r.label == "ssh agent" && r.level == levelProblem {
			t.Errorf("doctor calls it a problem: %+v", r)
		}
	}
	b.daemon.Stat = &daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "default", Link: daemonproto.LinkRunning}}}
	if err := os.MkdirAll(b.data, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockLink(b.data)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if status, _ := b.agentStatus(); status != "forwarded, but not reachable from the VM" {
		t.Errorf("link up: %q", status)
	}
}

// githubNoneConfig is a Mac's ~/.ssh/config seen in the wild: github.com
// told to use no agent, every other host 1Password's.
const githubNoneConfig = "Host github.com\n  IdentityAgent none\n\nHost *\n  " +
	`IdentityAgent "~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"` + "\n"

// The default agent is asked of ssh for a host no block names, so a
// github.com block's IdentityAgent none does not make it none; one for
// every host does, said as such, and $SSH_AUTH_SOCK is not taken then.
func TestAgentFromProbeHost(t *testing.T) {
	const home = "/Users/me"
	// What ssh -G prints for each host with githubNoneConfig.
	byHost := map[string]string{
		"github.com":   "user me\nidentityagent none",
		agentProbeHost: "user me\nidentityagent " + onePasswordSock,
	}
	var asked []string
	a, _ := agentApp(t, "darwin", "", "", map[string]string{"SSH_AUTH_SOCK": "/env.sock"})
	a.SSH = fakeSSH("", nil, home, map[string]string{home + "/.ssh/config": githubNoneConfig})
	a.SSH.G = func(host string) ([]byte, error) {
		asked = append(asked, host)
		out, ok := byHost[host]
		if !ok {
			return nil, errors.New("unexpected host " + host)
		}
		return []byte(out + "\n"), nil
	}
	if sock, from := a.hostAgent(); sock != onePasswordSock || from != agentFromConfig {
		t.Errorf("hostAgent %q (%s)", sock, from)
	}
	if len(asked) != 1 || asked[0] != agentProbeHost {
		t.Errorf("asked ssh -G for %q", asked)
	}
	byHost[agentProbeHost] = "user me\nidentityagent none"
	a.agentSeen = nil // another command's App, which asks again
	sock, from := a.hostAgent()
	if sock != "" || from != agentFromNone {
		t.Errorf("none for every host: %q (%s)", sock, from)
	}
	if said := a.agentSourceSaid(&checkup{}); said != " ("+agentFromNone+")" {
		t.Errorf("doctor: %q", said)
	}
}

// On a Mac engine with ssh_agent = "none", status and doctor say the
// sandbox has no agent because none was asked for, never that the host
// has one to forward; a sandbox created before the "none", which still
// has the engine's, is a problem doctor names, with caboose restart.
func TestDoctorMacAgentNone(t *testing.T) {
	for _, forwarded := range []bool{false, true} {
		t.Run(map[bool]string{false: "not forwarded", true: "forwarded"}[forwarded], func(t *testing.T) {
			box := runningBox(isolationContainer)
			if forwarded {
				box.SandboxMounts = append(box.SandboxMounts, backend.Mount{Source: hostServicesAgent, Target: containerAgent})
				box.Exec = func(s backend.ExecSpec) *exec.Cmd {
					switch {
					case len(s.Argv) == 3 && s.Argv[0] == "bash" && strings.Contains(s.Argv[2], "SSH_AUTH_SOCK"):
						return exec.Command("printf", "%s", containerAgent)
					case slices.Equal(s.Argv, []string{"ssh-add", "-l"}):
						return exec.Command("echo", "256 SHA256:k me (ED25519)")
					}
					return nil
				}
			}
			b := newBoxApp(t, isolationContainer, box)
			pinHost(t, true)
			b.write(t, "os", "OrbStack")
			b.mountLocal("local/linux-arm64")
			b.Cfg.SSHAgent = "none"
			b.Cfg.File = &config.File{Path: "/e/config.toml", Vals: map[string]any{"link.ssh_agent": "none"}}
			const why = `ssh_agent in [link] of /e/config.toml is "none"`
			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			out, _ := b.said()
			if !forwarded && !strings.Contains(out, "not forwarded ("+why+")") {
				t.Errorf("status:\n%s", out)
			}
			_ = b.Doctor([]string{"--offline"})
			out, _ = b.said()
			if strings.Contains(out, "the host has an agent") || strings.Contains(out, "is not used") {
				t.Errorf("doctor claims an agent:\n%s", out)
			}
			want := "not forwarded: " + why
			if forwarded {
				want = "forwarded, though " + why
			}
			if !strings.Contains(out, want) || forwarded != strings.Contains(out, "caboose restart, to stop forwarding it") {
				t.Errorf("doctor, want %q:\n%s", want, out)
			}
		})
	}
}

// One command asks for the host's agent several times (status, doctor's
// rows, the mounts): ssh -G runs once for them, and again for another
// setting or once the answer is old, so a long-lived App never keeps what
// config.toml or ~/.ssh/config said before.
func TestHostAgentRemembered(t *testing.T) {
	a, _ := agentApp(t, "darwin", info("OrbStack"), "", map[string]string{"SSH_AUTH_SOCK": "/env.sock"})
	runs := 0
	out := "user me"
	a.SSH = fakeSSH("", nil, "/h", nil)
	a.SSH.G = func(string) ([]byte, error) { runs++; return []byte(out + "\n"), nil }
	_, _ = a.agentDeclined()
	_ = a.sshAgentSource()
	_ = a.agentSourceSaid(&checkup{})
	if runs != 1 {
		t.Errorf("ssh -G ran %d times", runs)
	}
	a.Cfg.SSHAgent = "none"
	a.Cfg.File = &config.File{Path: "/e/config.toml", Vals: map[string]any{"link.ssh_agent": "none"}}
	if got := a.sshAgentSource(); got != "" {
		t.Errorf("after ssh_agent = none: %q", got)
	}
	a.Cfg.SSHAgent, a.Cfg.File = "", nil
	out = "identityagent none"
	a.agentSeen.at = a.agentSeen.at.Add(-agentMemoTTL)
	if got := a.sshAgentSource(); got != "" || runs != 2 {
		t.Errorf("an old answer kept: %q, ssh -G ran %d times", got, runs)
	}
}

// On a Mac engine with ssh_agent unset, doctor says why ssh -G could not
// decide the agent, and when ~/.ssh/config could not be read for it either,
// that an IdentityAgent none was not ruled out and the engine's is forwarded.
func TestAgentSourceSaidMacSSHGFailed(t *testing.T) {
	const home = "/Users/me"
	gerr := errors.New("ssh -G caboose-default-agent.invalid timed out")
	for _, tc := range []struct {
		name, config, want string
	}{
		{"unreadable", "Match exec \"slow\"\n  IdentityAgent none\n", "OrbStack's own (ssh -G caboose-default-agent.invalid timed out; "},
		{"no IdentityAgent", "User me\n", "OrbStack's own (ssh -G caboose-default-agent.invalid timed out; ~/.ssh/config, read directly, names no IdentityAgent)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := agentApp(t, "darwin", info("OrbStack"), "", map[string]string{"SSH_AUTH_SOCK": "/env.sock"})
			a.SSH = fakeSSH("", gerr, home, map[string]string{home + "/.ssh/config": tc.config})
			if got := a.sshAgentSource(); got != hostServicesAgent {
				t.Errorf("mounts %q", got)
			}
			said := a.agentSourceSaid(&checkup{})
			if !strings.Contains(said, tc.want) {
				t.Errorf("said %q, want %q", said, tc.want)
			}
			if tc.name == "unreadable" && !strings.Contains(said, "an IdentityAgent none could not be read, so the engine's agent is forwarded") {
				t.Errorf("said %q", said)
			}
		})
	}
	t.Run("ssh -G answered", func(t *testing.T) {
		a, _ := agentApp(t, "darwin", info("OrbStack"), "user me", map[string]string{"SSH_AUTH_SOCK": "/env.sock"})
		if said := a.agentSourceSaid(&checkup{}); said != "" {
			t.Errorf("said %q", said)
		}
	})
}

// Under vm the link gives no agent when none was asked for: doctor says
// so, never that the agent is unreachable or that ssh_agent should name a
// socket.
func TestDoctorVMAgentNone(t *testing.T) {
	box := runningBox(isolationVM)
	box.Exec = func(s backend.ExecSpec) *exec.Cmd {
		switch {
		case len(s.Argv) == 3 && s.Argv[0] == "bash" && strings.Contains(s.Argv[2], "SSH_AUTH_SOCK"):
			return exec.Command("printf", "%s", containerAgent)
		case slices.Equal(s.Argv, []string{"ssh-add", "-l"}):
			return exec.Command("sh", "-c", "echo 'Error connecting to agent: Connection refused' >&2; exit 2")
		}
		return nil
	}
	b := newBoxApp(t, isolationVM, box)
	b.mountLocal("local/linux-arm64")
	b.Cfg.SSHAgent = "none"
	b.Cfg.File = &config.File{Path: "/e/config.toml", Vals: map[string]any{"link.ssh_agent": "none"}}
	const why = `ssh_agent in [link] of /e/config.toml is "none"`
	if status, usable := b.agentStatus(); status != "not forwarded ("+why+")" || usable {
		t.Errorf("agentStatus %q, %v", status, usable)
	}
	_ = b.Doctor([]string{"--offline"})
	out, _ := b.said()
	if !strings.Contains(out, "not forwarded: "+why) || strings.Contains(out, "not reachable") || strings.Contains(out, "set ssh_agent") {
		t.Errorf("doctor:\n%s", out)
	}
}
