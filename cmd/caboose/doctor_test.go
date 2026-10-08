package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/statesync"
)

// doctorBox is what a healthy environment answers doctor with: the engine
// up, image caboose:default current, container caboose-default running on it with the configured
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
	// What a container created from the default sandbox config mounts.
	var keeps strings.Builder
	for _, rel := range []string{".claude", ".claude.json", ".config/caboose", ".config/git", ".config/jj", ".config/gh", ".ssh"} {
		fmt.Fprintf(&keeps, "/home/agent/%s\t%s\n", rel, filepath.Join(data, "home", rel))
	}
	fmt.Fprintf(&keeps, "/etc/claude-code\t%s\n", filepath.Join(data, "claude-code"))
	return strings.Join(over, "\n") + `
case "$1" in version) exit 0 ;; esac
` + imageLabels(defaultLabels("v1")) + "\n" + imageIDIs("sha:1") + "\n" + containerRunning + `
case "$*" in
  "inspect --type=container caboose-default --format "*)
    printf '/work/dev\t%s\n/home/agent/.local/bin\t%s\n` + statesync.ContainerDir + `\t%s\n' '` + root + `' '` +
		filepath.Join(data, "local", "linux-arm64", "bin") + `' '` + filepath.Join(data, statesync.Dir) + `'; printf '%s' '` + keeps.String() + `'; exit 0 ;;
  "exec caboose-default claude --version") echo "2.1.0 (Claude Code)"; exit 0 ;;
  "exec caboose-default bash -c for d in /proc/"*) echo "7 1 sleep"; exit 0 ;;
  "exec caboose-default bash -c printf '%s\0%s\0'"*) printf 'Etc/UTC\0002\0'; exit 0 ;;
  "exec caboose-default bash -c printf '%s' \"\${SSH_AUTH_SOCK-}\"") printf /ssh-agent.sock; exit 0 ;;
  "exec caboose-default ssh-add -l") echo "256 SHA256:abc me@mac (ED25519)"; exit 0 ;;
  "exec caboose-default ssh-add -L") echo "ssh-ed25519 AAAAkey me@mac"; exit 0 ;;
esac`
}

// doctorEnv is a sandboxed HOME (TZ as the container has it, no agent on
// the host) whose docker answers with doctorBox and over.
func doctorEnv(t *testing.T, over ...string) (home, log string) {
	t.Helper()
	home = sandboxEnv(t, "TZ", "Etc/UTC", "SSH_AUTH_SOCK", "")
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
// leaves it, with the tests' dockerfile profile chosen (sandboxEnv).
func setUp(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".caboose", "envs", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := config.EditFile([]byte(config.Template), config.Edit{Set: map[string]any{"image": "dockerfile.default"}, Tables: []string{"dockerfile.default"}})
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// apkoBuilt gives home's default environment the lock of apko.default at
// its defaults, as a build resolves it, and returns the labels of an image
// built from it.
func apkoBuilt(t *testing.T, home string) string {
	t.Helper()
	list, err := apkobuild.Spec{Defaults: true}.List()
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []map[string]string
	for _, p := range list {
		pkgs = append(pkgs, map[string]string{"name": p, "version": "1", "architecture": "aarch64", "url": "https://example.invalid/" + p})
	}
	b, err := json.Marshal(map[string]any{"version": "v1", "config": map[string]string{"name": apkobuild.ConfigName(apkobuild.Spec{Defaults: true}, list)},
		"contents": map[string]any{"packages": pkgs}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".caboose", "envs", "default", "apko-default.lock.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := apkobuild.ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	return defaultLabels("v1", assets.LabelBaseKind+"="+assets.BaseKindApko, assets.LabelBaseHash+"="+lock.Hash())
}

// An environment never set up is a note, not a problem: it runs on
// defaults, an apko image among them.
func TestDoctorNotSetUp(t *testing.T) {
	home, _ := doctorEnv(t)
	if err := os.Remove(filepath.Join(home, ".caboose", "envs", "default", "config.toml")); err != nil {
		t.Fatal(err)
	}
	scriptedDocker(t, doctorBox(t, home, imageLabels(apkoBuilt(t, home))))
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
	p := filepath.Join(home, ".caboose", "envs", "default", "data", "home", ".claude", ".credentials.json")
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
	managed := filepath.Join(home, ".caboose", "envs", "default", "data", "claude-code")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "CLAUDE.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runIt("doctor", "--offline")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q; stdout:\n%s", code, errOut, out)
	}
	wantRows(t, out, map[string]string{
		"env":       "default",
		"roots":     " at /work",
		"docker":    "the engine answers",
		"image":     "caboose:default, matching this launcher (built by v1)",
		"container": "caboose-default, running",
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
	sandboxEnv(t)
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
			answer := `[ "$*" = "inspect --type=container -f {{.State.Status}} caboose-default" ] && { echo ` + state + `; exit 0; }`
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
				want["container"] = "note: caboose-default does not exist; a launch creates it"
			} else {
				want["container"] = "note: caboose-default is exited; a launch starts it"
			}
			wantRows(t, out, want)
			changesNothing(t, log)
		})
	}
}

// dockerfileTOML is the tests' dockerfile profile, as sandboxEnv writes it.
const dockerfileTOML = "image = \"dockerfile.default\"\n[dockerfile.default]\n"

func TestDoctorImage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		toml   string
		answer string
		code   int
		row    string
		fix    string
	}{
		{"stale", "", imageLabels(defaultLabels("v0", assets.LabelLayerHash+"="+otherHash)), 1,
			"problem: caboose:default is out of date: built by v0, with a different layer", "caboose restart, which rebuilds it (this ends running sessions)"},
		{"stale, no auto build", dockerfileTOML + "[build]\nauto_build = false\n", imageLabels(defaultLabels("v0", assets.LabelLayerHash+"="+otherHash)), 1,
			"problem: caboose:default is out of date: built by v0, with a different layer", "caboose build, then caboose restart (this ends running sessions)"},
		{"on another base", "[ref.default]\nimage = \"node:20\"\n", "", 1,
			"problem: caboose:default is out of date", "caboose restart, which rebuilds it on the configured base"},
		{"missing", "", imageAbsent, 0, "note: 'caboose:default' is not built yet; the first launch builds it", ""},
		{"missing, no auto build", dockerfileTOML + "[build]\nauto_build = false\n", imageAbsent, 1,
			"problem: 'caboose:default' is not built, and auto_build = false in [build]", "caboose build"},
		{"unlabelled", "", imageLabels("{}"), 0, "note: caboose:default was not built by caboose build (it has none of its labels)", ""},
		{"its own base", "[ref.default]\nimage = \"caboose:default\"\n", "", 1,
			"problem: image in [ref.default] names the environment's own image ('caboose:default')", "name the image to build on in image in [ref.default]"},
		{"no Dockerfile", "image = \"dockerfile.nowhere\"\n[dockerfile.nowhere]\n", "", 1,
			"problem: dockerfile.nowhere builds from ", "caboose setup image, which writes caboose's Dockerfile there, or write one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := doctorEnv(t, tc.answer)
			if tc.toml != "" {
				writeConfig(t, home, tc.toml)
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
		if code != 1 || row(out, "container", "problem: the container runs an older image than the local 'caboose:default'") == "" ||
			!fixFor(out, "container", "caboose restart, which moves it onto the local image (this ends running sessions)") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("roots", func(t *testing.T) {
		other := t.TempDir()
		doctorEnv(t, containerLabels("/work/dev"), `[ "$1 $2" = "inspect --type=container" ] && [ "$4" = --format ] && { printf '/work/dev\t%s\n' '`+other+`'; exit 0; }`)
		code, out, _ := runIt("doctor", "--offline")
		if code != 1 || row(out, "roots", "problem: the container mounts "+other+" at /work/dev; the configuration says") == "" {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("timezone and versions", func(t *testing.T) {
		doctorEnv(t, `case "$*" in "exec caboose-default bash -c printf '%s\0%s\0'"*) printf '\0005\0sock'; exit 0 ;; esac`)
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
	home, _ := doctorEnv(t)
	writeConfig(t, home, "[roots]\nsrc = \"/nonexistent/src\"\n")
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantRows(t, out, map[string]string{
		"roots":  "problem: root src, /nonexistent/src, does not exist",
		"docker": "the engine answers", // and it went on
	})
	if !fixFor(out, "roots", "point the roots at directories that exist (set by [roots] in "+filepath.Join(home, ".caboose", "envs", "default", "config.toml")+")") {
		t.Errorf("no fix:\n%s", out)
	}
}

func TestDoctorAgent(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		doctorEnv(t, `[ "$*" = "exec caboose-default ssh-add -l" ] && { echo "The agent has no identities." >&2; exit 1; }`)
		code, out, _ := runIt("doctor", "--offline")
		if code != 1 || row(out, "ssh agent", "problem: forwarded, but the agent holds no keys") == "" ||
			!fixFor(out, "ssh agent", "add a key to the host's agent (ssh-add)") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	notForwarded := `[ "$*" = "exec caboose-default bash -c printf '%s' \"\${SSH_AUTH_SOCK-}\"" ] && exit 0`
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
	git := filepath.Join(home, ".caboose", "envs", "default", "data", "home", ".config", "git")
	if err := os.MkdirAll(git, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(git, "config")); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(home, ".caboose", "envs", "default", "data", "claude-code")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(managed, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 || row(out, "data dir", "problem: ~/.config/git/config is a symlink or a hard link") == "" ||
		!fixFor(out, "data dir", "remove it from inside: caboose shell, then rm ~/.config/git/config") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if row(out, "instructions", "problem: "+filepath.Join(managed, "CLAUDE.md")+" is a symlink or a hard link") == "" ||
		!fixFor(out, "instructions", "rm "+filepath.Join(managed, "CLAUDE.md")+" on this machine") {
		t.Fatalf("instructions:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(home, "elsewhere")); err == nil {
		t.Error("doctor wrote through the link")
	}
}

// doctor names caboose's instructions where a launch writes them, and
// where the sandbox reads them.
func TestDoctorInstructions(t *testing.T) {
	home, _ := doctorEnv(t)
	managed := filepath.Join(home, ".caboose", "envs", "default", "data", "claude-code")
	_, out, _ := runIt("doctor", "--offline")
	if row(out, "instructions", "note: caboose's instructions are not written yet; the next launch writes them to "+
		filepath.Join(managed, "CLAUDE.md")+" (/etc/claude-code/CLAUDE.md in the container, read-only)") == "" {
		t.Fatalf("none yet:\n%s", out)
	}
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "CLAUDE.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ = runIt("doctor", "--offline")
	if row(out, "instructions", filepath.Join(managed, "CLAUDE.md")+
		", which the container reads, read-only, as Claude Code's managed /etc/claude-code/CLAUDE.md") == "" {
		t.Fatalf("written:\n%s", out)
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
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TZ", "Etc/UTC")
	if code, _, errOut := runIt("sync", "--remote", remote); code != 0 {
		t.Fatalf("sync: exit %d: %s", code, errOut)
	}
	mem := filepath.Join(data, "home", ".claude", "projects", statesync.ProjectKey("/work/dev/proj"), "memory", "MEMORY.md")
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
		if strings.Contains(l, " git ") || strings.HasSuffix(l, " git") || strings.Contains(l, "caboose-agent sync") {
			t.Errorf("--offline ran git: %s", l)
		}
	}

	os.Remove(log)
	code, out, _ = runIt("doctor")
	if code != 0 || row(out, "sync", "nothing new on the remote") == "" {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(strings.Join(dockerLog(t, log), "\n"), "caboose-agent sync status") {
		t.Error("did not ask the sandbox")
	}
}

// What the sandbox config keeps shows; a container that mounts other
// entries -- here none of its own -- is a problem a restart fixes. A bad
// entry is a problem of its own, with where to fix it.
func TestDoctorKeep(t *testing.T) {
	home, _ := doctorEnv(t, `[ "$1 $2" = "inspect --type=container" ] && [ "$4" = --format ] && { printf '/work/dev\t%s\n' "$HOME/dev"; exit 0; }`)
	data := filepath.Join(home, ".caboose", "envs", "default", "data")
	cfg := filepath.Join(data, "home", ".config", "caboose", "sandbox.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	extra := "\n[[keep]]\npath = \"~/.aws\"\n\n[[keep]]\npath = \"~/.config/deep/er\"\n\n[[keep]]\npath = \"~/.npmrc\"\nfile = true\n"
	if err := os.WriteFile(cfg, append(sandboxcfg.Default([]string{"/work/oss"}), extra...), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	wantRows(t, out, map[string]string{
		"keep":  "~/.claude, ~/.claude.json, ~/.config/caboose, ~/.config/git, ~/.config/jj, ~/.config/gh, ~/.ssh, ~/.aws, ~/.npmrc; 8 sync rules",
		"roots": "problem: the sandbox config expects a root at /work/oss, which none of this machine's configured roots is at",
	})
	for _, want := range []string{
		`problem: keep "~/.config/deep/er": "~/.config/deep/er" is too deep`,
		"note: ~/.npmrc is kept as a file, a single-file mount",
		"problem: the container does not keep ~/.aws, ~/.claude, ~/.claude.json",
	} {
		if row(out, "keep", want) == "" {
			t.Errorf("no keep row %q in:\n%s", want, out)
		}
	}
	if !fixFor(out, "keep", "caboose restart (this ends running sessions)") || !fixFor(out, "roots", "caboose setup roots") {
		t.Errorf("fixes:\n%s", out)
	}
}

// A private key in the sandbox's ~/.ssh is worth a note -- not a problem:
// it may be a choice.
func TestDoctorSSHKey(t *testing.T) {
	home, _ := doctorEnv(t)
	setUp(t, home)
	data := filepath.Join(home, ".caboose", "envs", "default", "data")
	ssh := filepath.Join(data, "home", ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "id_ed25519"), []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "known_hosts"), []byte("github.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runIt("doctor", "--offline")
	if code != 0 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	wantRows(t, out, map[string]string{
		"ssh": "note: the sandbox's ~/.ssh holds a private key file (id_ed25519), kept in " + ssh,
	})
	if strings.Contains(out, "known_hosts)") {
		t.Errorf("known_hosts named as a key:\n%s", out)
	}
}

// A container created without caboose's instructions is a problem, whose
// fix is the restart that mounts them.
func TestDoctorInstructionsUnmounted(t *testing.T) {
	doctorEnv(t, `case "$*" in
  "inspect --type=container caboose-default --format "*) printf '/work/dev\t/nowhere\n'; exit 0 ;;
esac`)
	code, out, _ := runIt("doctor", "--offline")
	if code != 1 || row(out, "instructions", "problem: the container does not mount caboose's instructions at /etc/claude-code, so its sessions do not get them") == "" ||
		!fixFor(out, "instructions", "caboose restart") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}
