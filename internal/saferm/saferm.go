// Package saferm removes files and directories only inside a root it is given.
//
// Every path is resolved first (the directory holding it, through symlinks),
// then must lie strictly below the resolved root: a path that is empty, is
// the root itself, or reaches outside it through "..", an absolute path or a
// symlink is refused and nothing is removed. A symlink as the last element is
// removed as a link, never followed.
package saferm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutside is returned for a path that is not strictly inside the root.
var ErrOutside = errors.New("saferm: path is outside the root")

// RemoveAll removes path and everything below it, if path is inside root.
// A path that does not exist is not an error.
func RemoveAll(root, path string) error {
	p, err := resolve(root, path)
	if err != nil || p == "" {
		return err
	}
	return os.RemoveAll(p)
}

// Remove removes the file or empty directory path, if path is inside root.
// A path that does not exist is not an error.
func Remove(root, path string) error {
	p, err := resolve(root, path)
	if err != nil || p == "" {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RemoveRoot removes root itself and everything below it. It is the one way
// to remove a root, and refuses an empty root, the filesystem root, the home
// directory, any top-level directory (one component deep), and a root that
// does not resolve to a directory.
func RemoveRoot(root string) error {
	r, err := resolveRoot(root)
	if err != nil {
		return err
	}
	if home, err := os.UserHomeDir(); err == nil {
		if h, err := filepath.EvalSymlinks(home); err == nil && h == r {
			return fmt.Errorf("saferm: refusing to remove the home directory %q", r)
		}
	}
	if filepath.Dir(r) == r || filepath.Dir(r) == filepath.Dir(filepath.Dir(r)) || filepath.Dir(filepath.Dir(r)) == filepath.Dir(r) {
		return fmt.Errorf("saferm: refusing to remove %q", r)
	}
	return os.RemoveAll(r)
}

func resolveRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("saferm: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	r, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("saferm: root: %w", err)
	}
	fi, err := os.Stat(r)
	if err != nil {
		return "", fmt.Errorf("saferm: root: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("saferm: root %q is not a directory", r)
	}
	return r, nil
}

// resolve returns the path to remove, or "" when there is nothing to remove.
func resolve(root, path string) (string, error) {
	if path == "" {
		return "", errors.New("saferm: empty path")
	}
	r, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	base := filepath.Base(abs)
	if base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("%w: %q", ErrOutside, path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		if os.IsNotExist(err) {
			// The parent is not there, so neither is the path; but a missing
			// parent cannot be judged, so say nothing was removed.
			return "", nil
		}
		return "", err
	}
	full := filepath.Join(dir, base)
	rel, err := filepath.Rel(r, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q (root %q)", ErrOutside, path, root)
	}
	if _, err := os.Lstat(full); os.IsNotExist(err) {
		return "", nil
	}
	return full, nil
}
