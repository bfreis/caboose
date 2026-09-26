package statesync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// gitConfig overrides what a user's global git config could otherwise do to
// the sync repo: sign commits (and prompt for a passphrase), run hooks,
// rewrite line endings, or detect renames the merge should not guess at.
// In the container the global config is not read at all (the launcher's
// syncGit): it can arrive by sync, and must not steer the sync's own push.
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

// Command makes the command that runs git with args. remote is set for the
// commands that reach the remote (fetch, push): the ones that can hang on a
// network, and prompt for credentials; interactive, when such a prompt has
// a terminal to be shown on.
type Command func(remote, interactive bool, args ...string) *exec.Cmd

// LocalGit runs the git on this machine's PATH.
func LocalGit(_, _ bool, args ...string) *exec.Cmd {
	return exec.Command("git", args...)
}

// git runs git commands in one repo.
type git struct {
	// dir is the repo as git sees it, which in a container is not the
	// host path.
	dir     string
	command Command
	// Stderr, when set, receives the output of commands that talk to the
	// remote (fetch, push), so a prompt for credentials is seen.
	Stderr *os.File
}

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

func (g *git) cmd(remote, interactive bool, args []string, stdin []byte) *exec.Cmd {
	command := g.command
	if command == nil {
		command = LocalGit
	}
	c := command(remote, interactive, append(append([]string{"-C", g.dir}, gitConfig...), args...)...)
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
	c := g.cmd(false, false, args, stdin)
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

// talk runs a command that reaches the remote, with its stderr shown so an
// SSH or credential prompt is not left waiting unseen.
func (g *git) talk(args ...string) error {
	c := g.cmd(true, g.Stderr != nil, args, nil)
	var stderr bytes.Buffer
	if g.Stderr != nil {
		// Stdout too: through `docker exec -t` a prompt git writes to
		// its terminal arrives on docker's stdout.
		c.Stdin = os.Stdin
		c.Stdout = g.Stderr
		c.Stderr = g.Stderr
	} else {
		c.Stderr = &stderr
	}
	if err := c.Run(); err != nil {
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
