package launcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/linkdebug"
)

// loginEnvVars are what the daemon asks a login shell for: the PATH the
// user's tools are found on, the SSH agent, docker's settings and
// linkdebug's variable. Nothing else of the shell's environment is taken.
var loginEnvVars = []string{"PATH", "SSH_AUTH_SOCK", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", linkdebug.Var}

// loginEnvTimeout bounds the login shell: a profile may wait for a
// terminal, a network or a password.
const loginEnvTimeout = 5 * time.Second

// maxLoginEnvOutput bounds what is read from the shell.
const maxLoginEnvOutput = 1 << 20

// fallbackPath is the PATH a daemon is left with when the login shell gave
// none and the daemon started with none.
const fallbackPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// LoginEnv asks shell, as a login shell, for loginEnvVars and sets the ones
// it printed with setenv; on any failure it keeps what the process has. It
// returns the lines to log. The shell reads the user's own profile, which
// is the point: the daemon behaves the same whichever terminal or app
// started it. The shell runs in a process group of its own that is killed
// when timeout passes.
func LoginEnv(ctx context.Context, shell string, timeout time.Duration, getenv func(string) string, setenv func(k, v string)) []string {
	var notes []string
	if shell == "" {
		notes = append(notes, "login environment: SHELL is not set; keeping the daemon's own environment")
	} else if got, err := askLoginShell(ctx, shell, timeout); err != nil {
		notes = append(notes, fmt.Sprintf("login environment: %v; keeping the daemon's own environment", err))
	} else {
		var names []string
		for _, k := range loginEnvVars {
			if v, ok := got[k]; ok && v != "" {
				setenv(k, v)
				names = append(names, k)
			}
		}
		notes = append(notes, fmt.Sprintf("login environment: from %s -l: %s", shell, strings.Join(names, " ")))
	}
	if getenv("PATH") == "" {
		setenv("PATH", fallbackPath)
		notes = append(notes, "login environment: no PATH; using "+fallbackPath)
	}
	return notes
}

// askLoginShell runs `shell -l -c 'exec /usr/bin/env'`, which every
// common shell (bash, zsh, fish, sh) takes, and returns the wanted
// variables of what it printed. A profile's own output comes first and is
// ignored; of a variable printed twice the last counts.
func askLoginShell(ctx context.Context, shell string, timeout time.Duration) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", "exec /usr/bin/env")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out limitBuffer
	cmd.Stdout = &out
	cmd.Stderr = nil // /dev/null; stdin too
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", shell, err)
	}
	err := cmd.Wait()
	// Whatever the shell left in its group goes too.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s -l did not answer within %v", shell, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s -l: %v", shell, err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		for _, want := range loginEnvVars {
			if k == want {
				got[k] = v
			}
		}
	}
	return got, nil
}

// limitBuffer keeps the first maxLoginEnvOutput bytes written to it.
type limitBuffer struct{ b bytes.Buffer }

func (l *limitBuffer) Write(p []byte) (int, error) {
	if room := maxLoginEnvOutput - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (l *limitBuffer) String() string { return l.b.String() }

// captureLoginEnv is LoginEnv on the process's own environment.
func captureLoginEnv(ctx context.Context) []string {
	return LoginEnv(ctx, os.Getenv("SHELL"), loginEnvTimeout, os.Getenv, func(k, v string) { _ = os.Setenv(k, v) })
}
