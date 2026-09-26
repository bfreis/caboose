package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/statesync"
)

// doctorBox is what a healthy environment answers doctor with: the engine
// up, image img current, container box running on it with the configured
// root, a platform dir and the sync repo mounted, Claude Code in it, no
// sessions, the container's TZ and keep-versions as the host has them, and
// an agent holding one key. Each of over's lines comes first, so a test
// changes one answer by giving it.
func doctorBox(t *testing.T, home string, over ...string) string {
	t.Helper()
	root, err := config.Physical(filepath.Join(home, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(home, ".caboose", "envs", "default", "data")
	return strings.Join(over, "\n") + `
case "$1" in version) exit 0 ;; esac
` + imageLabels(defaultLabels("v1")) + "\n" + imageIDIs("sha:1") + "\n" + containerRunning + `
case "$*" in
  "inspect --type=container box --format "*)
    printf '/work\t%s\n/home/agent/.local/bin\t%s\n/home/agent/.ssh\t%s\n` + statesync.ContainerDir + `\t%s\n' '` + root + `' '` +
		filepath.Join(data, "dot_local", "linux-arm64", "bin") + `' '` + filepath.Join(data, "dot_ssh") + `' '` + filepath.Join(data, statesync.Dir) + `'; exit 0 ;;
  "exec box claude --version") echo "2.1.0 (Claude Code)"; exit 0 ;;
  "exec box bash -c for d in /proc/"*) echo "7 1 sleep"; exit 0 ;;
  "exec box bash -c printf '%s\0%s\0'"*) printf 'Etc/UTC\0002\0'; exit 0 ;;
  "exec box bash -c printf '%s' \"\${SSH_AUTH_SOCK-}\"") printf /ssh-agent.sock; exit 0 ;;
  "exec box ssh-add -l") echo "256 SHA256:abc me@mac (ED25519)"; exit 0 ;;
  "exec box ssh-add -L") echo "ssh-ed25519 AAAAkey me@mac"; exit 0 ;;
esac`
}

// doctorEnv is a sandboxed HOME (TZ as the container has it, no agent on
// the host) whose docker answers with doctorBox and over.
func doctorEnv(t *testing.T, over ...string) (home, log string) {
	t.Helper()
	home = sandboxEnv(t, "CABOOSE_CONTAINER", "box", "CABOOSE_IMAGE", "img", "TZ", "Etc/UTC", "SSH_AUTH_SOCK", "")
	log = scriptedDocker(t, doctorBox(t, home, over...))
	return home, log
}

// levelWords are what doctor's marks stand for, as row reads them.
var levelWords = map[string]string{"✓": "", "!": "note: ", "✗": "problem: ", "–": ""}

// row is doctor's row for label that contains want, or "". A row reads as
// its level's word, then its text: "note: ...", "problem: ...".
func row(out, label, want string) string {
	for _, l := range strings.Split(out, "\n") {
		mark, rest, ok := strings.Cut(strings.TrimPrefix(l, "  "), " ")
		word, known := levelWords[mark]
		if !ok || !known || !strings.HasPrefix(rest, label+"  ") {
			continue
		}
		if v := word + strings.TrimSpace(rest[len(label):]); strings.Contains(v, want) {
			return v
		}
	}
	return ""
}

// fixFor is whether doctor's fixes, after its "To fix:", give label want.
func fixFor(out, label, want string) bool {
	_, fixes, ok := strings.Cut(out, "To fix:\n")
	for _, l := range strings.Split(fixes, "\n") {
		if rest, found := strings.CutPrefix(l, "    "+label+" "); ok && found && strings.HasPrefix(strings.TrimSpace(rest), want) {
			return true
		}
	}
	return false
}

// wantRows checks out has, for each label, a row containing its text.
func wantRows(t *testing.T, out string, rows map[string]string) {
	t.Helper()
	for label, want := range rows {
		if row(out, label, want) == "" {
			t.Errorf("no %q row with %q in:\n%s", label, want, out)
		}
	}
}

// changesNothing checks doctor asked docker only questions.
func changesNothing(t *testing.T, log string) {
	t.Helper()
	for _, l := range dockerLog(t, log) {
		switch strings.Fields(l + " x")[0] {
		case "run", "create", "start", "stop", "rm", "build", "pull", "restart":
			t.Errorf("doctor ran docker %s", l)
		}
	}
}

// setUp gives home's default environment a config.toml, as caboose setup
// leaves it.
func setUp(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".caboose", "envs", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config.Template), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An environment never set up is a note, not a problem: it runs on
// defaults.
func TestDoctorNotSetUp(t *testing.T) {
	home, _ := doctorEnv(t)
	code, out, _ := runIt("doctor", "--offline")
	want := "note: default is not set up (no " + filepath.Join(home, ".caboose", "envs", "default", "config.toml") +
		"), and runs on defaults; 'caboose setup' asks for its settings"
	if code != 0 || row(out, "env", want) != want {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// loggedIn gives home's default environment a Claude login.
func loggedIn(t *testing.T, home string) string {
	t.Helper()
	p := filepath.Join(home, ".caboose", "envs", "default", "data", ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// No login is a note: the first session asks for one.
func TestDoctorLogin(t *testing.T) {
	home, _ := doctorEnv(t)
	setUp(t, home)
	code, out, _ := runIt("doctor", "--offline")
	if code != 0 || row(out, "login", "note: not logged in to Claude yet; the first session asks") == "" {
		t.Errorf("exit %d:\n%s", code, out)
	}
	p := loggedIn(t, home)
	if _, out, _ := runIt("doctor", "--offline"); row(out, "login", "Claude, in "+p) == "" {
		t.Errorf("logged in:\n%s", out)
	}
}

func TestDoctorHealthy(t *testing.T) {
	home, log := doctorEnv(t)
	setUp(t, home)
	loggedIn(t, home)
	code, out, errOut := runIt("doctor", "--offline")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q; stdout:\n%s", code, errOut, out)
	}
	wantRows(t, out, map[string]string{
		"env":       "default",
		"roots":     " at /work",
		"docker":    "the engine answers",
		"image":     "img, matching this launcher (built by v1)",
		"container": "box, running",
		"claude":    "2.1.0",
		"sessions":  "none running",
		"ssh agent": "forwarded, 1 key",
		"login":     "Claude, in ",
		"git":       "not checked: no git on the host",
		"signing":   "not checked: no git on the host",
		"sync":      "not set up",
	})
	for _, l := range []string{"  ✗ ", "  ! ", "timezone", "versions", "platform"} {
		if strings.Contains(out, l) {
			t.Errorf("%q in a healthy report:\n%s", l, out)
		}
	}
	if !strings.HasSuffix(out, "\n  ✓ No problems found.\n") {
		t.Errorf("no verdict at the end:\n%s", out)
	}
	changesNothing(t, log)
}

func TestDoctorDockerDown(t *testing.T) {
	sandboxEnv(t, "CABOOSE_CONTAINER", "box", "CABOOSE_IMAGE", "img")
	log := scriptedDocker(t, daemonDown)
	code, out, _ := runIt("doctor")
	if code != 1 {
		t.Fatalf("exit %d; stdout:\n%s", code, out)
	}
	wantRows(t, out, map[string]string{
		"docker":    "problem: the engine does not answer",
		"claude":    "not checked: docker did not answer",
		"ssh agent": "not checked: docker did not answer",
		"env":       "default",
	})
	if !strings.Contains(out, "\n  ✗ 1 problem. To fix:\n") || !fixFor(out, "docker", "start the Docker engine") {
		t.Errorf("no fix for the engine:\n%s", out)
	}
	changesNothing(t, log)
}

func TestDoctorContainerNotRunning(t *testing.T) {
	for _, state := range []string{"exited", "absent"} {
		t.Run(state, func(t *testing.T) {
			answer := `[ "$*" = "inspect --type=container -f {{.State.Status}} box" ] && { echo ` + state + `; exit 0; }`
			if state == "absent" {
				answer = `case "$*" in "inspect --type=container"*) exit 1 ;; esac`
			}
			_, log := doctorEnv(t, answer)
			code, out, _ := runIt("doctor")
			if code != 0 {
				t.Fatalf("exit %d; stdout:\n%s", code, out)
			}
			want := map[string]string{
				"claude":   "not checked: the container is not running",
				"sessions": "not checked: the container is not running",
			}
			if state == "absent" {
				want["container"] = "note: box does not exist; a launch creates it"
			} else {
				want["container"] = "note: box is exited; a launch starts it"
			}
			wantRows(t, out, want)
			changesNothing(t, log)
		})
	}
}

func TestDoctorImage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    []string
		answer string
		code   int
		row    string
		fix    string
	}{
		{"stale", nil, imageLabels(defaultLabels("v0", assets.LabelLayerHash+"="+otherHash)), 1,
			"problem: img is out of date: built by v0, with a different layer", "caboose restart, which rebuilds it (this ends running sessions)"},
		{"stale, no auto build", []string{"CABOOSE_NO_AUTO_BUILD", "1"}, imageLabels(defaultLabels("v0", assets.LabelLayerHash+"="+otherHash)), 1,
			"problem: img is out of date: built by v0, with a different layer", "caboose build, then caboose restart (this ends running sessions)"},
		{"on another base", []string{"CABOOSE_BASE_IMAGE", "node:20"}, "", 1,
			"problem: img is out of date", "caboose restart, which rebuilds it on the configured base"},
		{"missing", nil, imageAbsent, 0, "note: 'img' is not built yet; the first launch builds it", ""},
		{"missing, no auto build", []string{"CABOOSE_NO_AUTO_BUILD", "1"}, imageAbsent, 1,
			"problem: 'img' is not built, and CABOOSE_NO_AUTO_BUILD", "caboose build"},
		{"unlabelled", nil, imageLabels("{}"), 0, "note: img was not built by caboose build (it has none of its labels)", ""},
		{"its own base", []string{"CABOOSE_BASE_IMAGE", "img"}, "", 1,
			"problem: CABOOSE_BASE_IMAGE names the same image as CABOOSE_IMAGE", "unset CABOOSE_IMAGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctorEnv(t, tc.answer)
			for i := 0; i+1 < len(tc.env); i += 2 {
				t.Setenv(tc.env[i], tc.env[i+1])
			}
			code, out, _ := runIt("doctor", "--offline")
			if code != tc.code || row(out, "image", tc.row) == "" {
				t.Fatalf("exit %d, want %d, and an image row with %q:\n%s", code, tc.code, tc.row, out)
			}
			if tc.fix != "" && !fixFor(out, "image", tc.fix) {
				t.Errorf("no fix %q:\n%s", tc.fix, out)
			}
		})
	}
}

func TestDoctorContainerDrift(t *testing.T) {
	t.Run("older image", func(t *testing.T) {
		doctorEnv(t, imageIDIs("sha:2"))
		code, out, _ := runIt("doctor", "--offline")
		if code != 1 || row(out, "container", "problem: box was created from an older image than img") == "" ||
			!fixFor(out, "container", "caboose restart (this ends running sessions)") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("roots", func(t *testing.T) {
		other := t.TempDir()
		doctorEnv(t, `[ "$1 $2" = "inspect --type=container" ] && [ "$4" = --format ] && { printf '/work\t%s\n' '`+other+`'; exit 0; }`)
		code, out, _ := runIt("doctor", "--offline")
		if code != 1 || row(out, "roots", "problem: the container mounts "+other+" at /work; the configuration says") == "" {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("timezone and versions", func(t *testing.T) {
		doctorEnv(t, `case "$*" in "exec box bash -c printf '%s\0%s\0'"*) printf '\0005\0sock'; exit 0 ;; esac`)
		code, out, _ := runIt("doctor", "--offline")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		wantRows(t, out, map[string]string{
			"timezone": "note: the container has UTC, the host Etc/UTC now",
			"versions": "note: the container keeps 5 Claude Code versions, the configuration 2",
			"docker":   "note: the host's docker socket is mounted",
		})
	})
}

func TestDoctorRootMissing(t *testing.T) {
	doctorEnv(t)
	t.Setenv("CABOOSE_REPO_ROOT", "/nonexistent/src")
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantRows(t, out, map[string]string{
		"roots":  "problem: repo root /nonexistent/src does not exist",
		"docker": "the engine answers", // and it went on
	})
	if !fixFor(out, "roots", "point the roots at directories that exist (set by CABOOSE_REPO_ROOT)") {
		t.Errorf("no fix:\n%s", out)
	}
}

func TestDoctorAgent(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		doctorEnv(t, `[ "$*" = "exec box ssh-add -l" ] && { echo "The agent has no identities." >&2; exit 1; }`)
		code, out, _ := runIt("doctor", "--offline")
		if code != 1 || row(out, "ssh agent", "problem: forwarded, but the agent holds no keys") == "" ||
			!fixFor(out, "ssh agent", "add a key to the host's agent (ssh-add)") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	notForwarded := `[ "$*" = "exec box bash -c printf '%s' \"\${SSH_AUTH_SOCK-}\"" ] && exit 0`
	t.Run("the host has none", func(t *testing.T) {
		doctorEnv(t, notForwarded)
		code, out, _ := runIt("doctor", "--offline")
		if code != 0 || row(out, "ssh agent", "note: not forwarded: the host has no agent") == "" {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("the host has one now", func(t *testing.T) {
		doctorEnv(t, notForwarded)
		sock := filepath.Join(t.TempDir(), "agent.sock")
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Skip(err)
		}
		t.Cleanup(func() { l.Close() })
		t.Setenv("SSH_AUTH_SOCK", sock)
		code, out, _ := runIt("doctor", "--offline")
		if code != 1 || row(out, "ssh agent", "problem: not forwarded, though the host has an agent now") == "" ||
			!fixFor(out, "ssh agent", "caboose restart, to forward it") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
}

func TestDoctorLinkWhereALaunchWrites(t *testing.T) {
	home, _ := doctorEnv(t)
	claude := filepath.Join(home, ".caboose", "envs", "default", "data", ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(claude, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 || row(out, "data dir", "problem: ~/.claude/CLAUDE.md is a symlink or a hard link") == "" ||
		!fixFor(out, "data dir", "remove it from inside: caboose shell, then rm ~/.claude/CLAUDE.md") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(home, "elsewhere")); err == nil {
		t.Error("doctor wrote through the link")
	}
}

func TestDoctorUsage(t *testing.T) {
	doctorEnv(t)
	code, out, errOut := runIt("doctor", "--fix")
	if code != 2 || out != "" || !strings.Contains(errOut, "usage: caboose doctor [--offline]") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// With a remote, doctor fetches -- unless --offline, which runs no git at
// all -- and reports what waits on each side.
func TestDoctorSync(t *testing.T) {
	data, remote, _ := syncEnv(t, func(git, data string) string {
		home := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(data))))
		return doctorBox(t, home) + "\n" + mountedHere(git, data)
	})
	t.Setenv("CABOOSE_IMAGE", "img")
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TZ", "Etc/UTC")
	if code, _, errOut := runIt("sync", "--remote", remote); code != 0 {
		t.Fatalf("sync: exit %d: %s", code, errOut)
	}
	mem := filepath.Join(data, ".claude", "projects", statesync.ProjectKey("/work/proj"), "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(mem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("- a note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(os.Getenv("PATH"), "log")

	os.Remove(log)
	code, out, _ := runIt("doctor", "--offline")
	wantRows(t, out, map[string]string{
		"sync": "note: 1 change here not sent yet",
	})
	if code != 0 || row(out, "sync", "not checked: the remote: --offline") == "" {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, l := range dockerLog(t, log) {
		if strings.Contains(l, " git ") || strings.HasSuffix(l, " git") {
			t.Errorf("--offline ran git: %s", l)
		}
	}

	os.Remove(log)
	code, out, _ = runIt("doctor")
	if code != 0 || row(out, "sync", "nothing new on the remote") == "" {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(strings.Join(dockerLog(t, log), "\n"), " fetch -q origin") {
		t.Error("did not fetch")
	}
}

func TestDoctorTwoBases(t *testing.T) {
	home, _ := doctorEnv(t)
	t.Setenv("CABOOSE_BASE_IMAGE", "node:22")
	dir := filepath.Join(home, ".caboose", "envs", "default", "image")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 || row(out, "image", "problem: the environment has "+dir+", and CABOOSE_BASE_IMAGE names 'node:22'") == "" ||
		!strings.Contains(out, "or move "+dir+" away") || strings.Count(out, "\n  ✗ image  ") != 1 {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// [persist] shows with the configuration; a container that mounts other
// entries -- here none -- is a problem a restart fixes.
func TestDoctorPersist(t *testing.T) {
	home, _ := doctorEnv(t, `[ "$1 $2" = "inspect --type=container" ] && [ "$4" = --format ] && { printf '/work\t%s\n' "$HOME/dev"; exit 0; }`)
	dir := filepath.Join(home, ".caboose", "envs", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[persist]\naws = \"~/.aws\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	wantRows(t, out, map[string]string{
		"persist": "~/.aws (aws)",
	})
	want := "problem: the container persists none; the configuration says ~/.aws (aws)"
	if row(out, "persist", want) == "" || !fixFor(out, "persist", "caboose restart (this ends running sessions)") {
		t.Errorf("no drift problem %q in:\n%s", want, out)
	}
}

// A private key in the sandbox's ~/.ssh, and a persist/ dir no entry names
// any more, are each worth a note -- not problems: both may be a choice.
func TestDoctorSSHKeyAndLeftoverPersist(t *testing.T) {
	home, _ := doctorEnv(t)
	setUp(t, home)
	data := filepath.Join(home, ".caboose", "envs", "default", "data")
	for _, d := range []string{"dot_ssh", "persist/old"} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, "dot_ssh", "id_ed25519"), []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "dot_ssh", "known_hosts"), []byte("github.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runIt("doctor", "--offline")
	if code != 0 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	wantRows(t, out, map[string]string{
		"ssh":     "note: the sandbox's ~/.ssh holds a private key file (id_ed25519), kept in " + filepath.Join(data, "dot_ssh"),
		"persist": "note: old in " + filepath.Join(data, "persist") + " is no longer in [persist], so not mounted; it is kept",
	})
	if strings.Contains(out, "known_hosts)") {
		t.Errorf("known_hosts named as a key:\n%s", out)
	}
}
