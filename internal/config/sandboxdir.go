package config

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// CheckSandboxDir checks a directory in the sandbox that a session may
// start in ([session] dir): absolute, clean, with no ~ (nothing there
// expands it), no control or invisible character, and no #, which tmux
// reads as the start of a format in the directory it is given.
func CheckSandboxDir(dir string) error {
	switch {
	case dir == "":
		return errors.New("the directory is empty")
	case !printableText(dir):
		return errors.New("the directory has a control or invisible character")
	case strings.Contains(dir, "#"):
		return errors.New("the directory has a #, which tmux would read as the start of a format; use a path without one")
	case dir == "~" || strings.HasPrefix(dir, "~/"):
		return errors.New("~ is not expanded in the sandbox; write the path in full (the home is /home/agent)")
	case !path.IsAbs(dir):
		return errors.New("the directory is not an absolute path in the sandbox")
	case path.Clean(dir) != dir:
		return fmt.Errorf("the directory is not clean; write it as %q", path.Clean(dir))
	}
	return nil
}

// ResolveStartDir is the sandbox directory that caboose -C names: flag
// itself when absolute, else relative to base ([session] dir), cleaned.
// flag is a path in the sandbox: a shell on the host expands ~ before
// caboose sees it, to a path that is the host's, so a value that is not
// clean to begin with, or has no base to be relative to, is refused with
// what to do.
func ResolveStartDir(flag, base string) (string, error) {
	if flag == "" {
		return "", errors.New("-C takes a directory in the sandbox, and was given an empty one")
	}
	if !printableText(flag) {
		return "", errors.New("-C: the directory has a control or invisible character")
	}
	if strings.Contains(flag, "#") {
		return "", errors.New("-C: the directory has a #, which tmux would read as the start of a format; use a path without one")
	}
	if flag == "~" || strings.HasPrefix(flag, "~/") {
		return "", errors.New("-C takes a directory in the sandbox, where ~ is not expanded: write the path in full (the home is /home/agent)")
	}
	if path.IsAbs(flag) {
		return path.Clean(flag), nil
	}
	if base == "" {
		return "", fmt.Errorf("-C %s is relative, and there is no [session] dir to take it from: give an absolute path in the sandbox, or set dir in [session] of config.toml", flag)
	}
	return path.Join(base, flag), nil
}
