package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeShell writes an executable sh script standing for $SHELL, which gets
// `-l -c 'exec /usr/bin/env'`.
func fakeShell(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakesh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// envRecorder is a process environment of the test's own.
type envRecorder map[string]string

func (e envRecorder) get(k string) string { return e[k] }
func (e envRecorder) set(k, v string)     { e[k] = v }

func TestLoginEnvTakesOnlyTheWantedVariables(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "agent.sock")
	sh := fakeShell(t, `
echo "welcome from the profile PATH=/junk"
echo "PATH=/opt/tools/bin:/usr/bin"
echo "SSH_AUTH_SOCK=`+sock+`"
echo "DOCKER_HOST=unix:///x/docker.sock"
echo "CABOOSE_DEBUG_LINK=sockbuf=0"
echo "GITHUB_TOKEN=secret"
echo "CABOOSE_DATA_DIR=/elsewhere"
case "$1 $2" in "-l -c") ;; *) exit 9;; esac
`)
	env := envRecorder{"PATH": "/usr/bin"}
	notes := LoginEnv(context.Background(), sh, 5*time.Second, env.get, env.set)
	want := envRecorder{"PATH": "/opt/tools/bin:/usr/bin", "SSH_AUTH_SOCK": sock,
		"DOCKER_HOST": "unix:///x/docker.sock", "CABOOSE_DEBUG_LINK": "sockbuf=0"}
	if len(env) != len(want) {
		t.Fatalf("got %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "from "+sh) {
		t.Errorf("notes: %v", notes)
	}
}

func TestLoginEnvTimesOutAndKeepsWhatItHas(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// The shell and a child of it both hang; the whole group must go.
	sh := fakeShell(t, `sleep 60 & echo $! > `+pidFile+`
echo "PATH=/from/shell"
sleep 60`)
	env := envRecorder{"PATH": "/mine"}
	start := time.Now()
	notes := LoginEnv(context.Background(), sh, 300*time.Millisecond, env.get, env.set)
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("took %v for a 300ms timeout", d)
	}
	if env["PATH"] != "/mine" || len(env) != 1 {
		t.Errorf("a timed-out shell changed the environment: %v", env)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "did not answer") {
		t.Errorf("notes: %v", notes)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, c := range strings.TrimSpace(string(b)) {
		pid = pid*10 + int(c-'0')
	}
	if pid <= 0 {
		t.Fatalf("pid file: %q", b)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		// A zombie reparented to init still answers signal 0 briefly.
		t.Errorf("the shell's child %d still runs after the timeout", pid)
	}
}

func TestLoginEnvFailureFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		shell    func(t *testing.T) string
		in       envRecorder
		wantPath string
	}{
		"exit status": {func(t *testing.T) string { return fakeShell(t, `echo PATH=/from/shell; exit 3`) },
			envRecorder{"PATH": "/mine"}, "/mine"},
		"no such shell": {func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
			envRecorder{"PATH": "/mine"}, "/mine"},
		"no shell set": {func(t *testing.T) string { return "" }, envRecorder{"PATH": "/mine"}, "/mine"},
		"nothing and no PATH": {func(t *testing.T) string { return fakeShell(t, `exit 1`) },
			envRecorder{}, fallbackPath},
		"empty answer and no PATH": {func(t *testing.T) string { return fakeShell(t, `echo hello`) },
			envRecorder{}, fallbackPath},
	} {
		t.Run(name, func(t *testing.T) {
			env := tc.in
			notes := LoginEnv(context.Background(), tc.shell(t), 2*time.Second, env.get, env.set)
			if env["PATH"] != tc.wantPath {
				t.Errorf("PATH = %q, want %q (notes %v)", env["PATH"], tc.wantPath, notes)
			}
			if len(env) != 1 {
				t.Errorf("other variables set: %v", env)
			}
			if len(notes) == 0 {
				t.Error("nothing logged")
			}
		})
	}
}
