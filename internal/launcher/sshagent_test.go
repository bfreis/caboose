package launcher

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// agentApp is an App on a fake docker whose script body is given, a fake
// ssh on PATH printing sshG for `ssh -G`, and the environment env.
func agentApp(t *testing.T, os_, body, sshG string, env map[string]string) (*App, *bytes.Buffer) {
	t.Helper()
	old := goos
	goos = os_
	t.Cleanup(func() { goos = old })
	bin := t.TempDir()
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("docker", body+"\nexit 1")
	if sshG != "" {
		write("ssh", `[ "$1" = -G ] && printf '%s\n' '`+sshG+`'`)
	}
	t.Setenv("PATH", bin)
	var errb bytes.Buffer
	return &App{
		Cfg:    &config.Config{Container: "box", Getenv: func(k string) string { return env[k] }},
		Docker: &docker.CLI{Path: filepath.Join(bin, "docker")},
		Stdout: &bytes.Buffer{}, Stderr: &errb,
	}, &errb
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
		sock := listen(t, filepath.Join(t.TempDir(), "a.sock"))
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

// On Linux the agent is mounted directly: the one ssh would use, which an
// IdentityAgent in ~/.ssh/config (1Password's documented setup) picks over
// $SSH_AUTH_SOCK.
func TestSSHAgentSourceLinux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	op := listen(t, filepath.Join(home, ".1password", "agent.sock"))
	env := listen(t, filepath.Join(t.TempDir(), "env.sock"))
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
			a, _ := agentApp(t, "linux", "", tc.sshG, map[string]string{"SSH_AUTH_SOCK": env})
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
			log := filepath.Join(t.TempDir(), "log")
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	op := listen(t, filepath.Join(home, ".1password", "agent.sock"))
	env := listen(t, filepath.Join(t.TempDir(), "env.sock"))
	set := listen(t, filepath.Join(t.TempDir(), "set.sock"))
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
			a, _ := agentApp(t, "linux", "", tc.sshG, map[string]string{"SSH_AUTH_SOCK": env})
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
