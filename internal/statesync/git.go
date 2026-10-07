package statesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// gitConfig overrides what a user's git config could otherwise do to the
// sync repo: sign commits (and prompt for a passphrase), run hooks, rewrite
// line endings, or detect renames the merge should not guess at. The global
// config is not read at all in the sandbox (internal/syncagent): it can
// arrive by sync, and must not steer the sync's own push.
var gitConfig = []string{
	"-c", "commit.gpgsign=false",
	"-c", "tag.gpgsign=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.symlinks=false",
	"-c", "core.autocrlf=false",
	"-c", "core.safecrlf=false",
	"-c", "core.quotePath=false",
	"-c", "merge.renames=false",
	"-c", "diff.renames=false",
	"-c", "merge.conflictStyle=diff3",
	"-c", "init.defaultBranch=main",
	"-c", "advice.detachedHead=false",
}

// git runs git commands in one repo.
type git struct {
	dir string
	// env and args are added to every command's environment and
	// arguments (Syncer.Env, Syncer.GitArgs).
	env, args []string
	// Stderr, when set, receives the output of commands that talk to the
	// remote (fetch, push).
	Stderr io.Writer
	// budget bounds the commands that reach the remote, all told, from
	// the first; deadline is when it runs out, set by that first one.
	budget   time.Duration
	deadline time.Time
	// ctx, when set, stops a command that reaches the remote once done.
	ctx context.Context
}

// ErrNoAnswer is a command that reaches the remote stopped at the end of
// the budget; its text, with the budget, is what the caller shows.
var ErrNoAnswer = errors.New("no answer from the remote")

// gitError is a failed git command, with what it said.
type gitError struct {
	args   []string
	code   int
	stderr string
}

func (e *gitError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.code)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), msg)
}

// exitCode is err's git exit status, or -1 when it is not a gitError.
func exitCode(err error) int {
	var ge *gitError
	if errors.As(err, &ge) {
		return ge.code
	}
	return -1
}

func (g *git) cmd(ctx context.Context, args []string, stdin []byte) *exec.Cmd {
	argv := append(append(append([]string{"-C", g.dir}, g.args...), gitConfig...), args...)
	c := exec.CommandContext(ctx, "git", argv...)
	if len(g.env) > 0 {
		c.Env = append(os.Environ(), g.env...)
	}
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	return c
}

// out runs git and returns its stdout.
func (g *git) out(args ...string) ([]byte, error) {
	return g.outIn(nil, args...)
}

func (g *git) outIn(stdin []byte, args ...string) ([]byte, error) {
	c := g.cmd(context.Background(), args, stdin)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &gitError{args: args, code: ee.ExitCode(), stderr: stderr.String()}
		}
		return nil, fmt.Errorf("running git: %w", err)
	}
	return stdout.Bytes(), nil
}

// run runs git for its effect.
func (g *git) run(args ...string) error {
	_, err := g.out(args...)
	return err
}

// talk runs a command that reaches the remote. Its output goes to Stderr
// when that is set. Within a budget it runs in a process group of its own,
// killed whole when the budget runs out: git's ssh, and whatever ssh
// started (a ProxyCommand), go with it.
func (g *git) talk(args ...string) error {
	ctx := context.Background()
	if g.ctx != nil {
		ctx = g.ctx
	}
	if g.budget > 0 {
		if g.deadline.IsZero() {
			g.deadline = time.Now().Add(g.budget)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, g.deadline)
		defer cancel()
	}
	c := g.cmd(ctx, args, nil)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	// What the group left holding the output open is not waited for.
	c.WaitDelay = time.Second
	var stderr bytes.Buffer
	if g.Stderr != nil {
		c.Stdout = g.Stderr
		c.Stderr = g.Stderr
	} else {
		c.Stderr = &stderr
	}
	err := c.Run()
	if g.ctx != nil && g.ctx.Err() != nil {
		return ErrStopped
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w in %s", ErrNoAnswer, g.budget.Round(time.Second))
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &gitError{args: args, code: ee.ExitCode(), stderr: stderr.String()}
		}
		return fmt.Errorf("running git: %w", err)
	}
	return nil
}

// str is out, trimmed.
func (g *git) str(args ...string) (string, error) {
	b, err := g.out(args...)
	return strings.TrimSpace(string(b)), err
}

// ok runs a command whose exit status is the answer (0 yes, 1 no).
func (g *git) ok(args ...string) (bool, error) {
	err := g.run(args...)
	switch exitCode(err) {
	case -1:
		return err == nil, err
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// zsplit splits NUL-terminated output.
func zsplit(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}
