package hostlink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/bfreis/caboose/internal/policy"
)

// AppPlaceOf gathers what policy.CheckAppPlace judges of exe, the menu-bar
// app's executable: exe resolved as an alias's command is (ResolvePath),
// the symlinks met, and the identity of exe, of what it resolves to, of
// every symlink met and of every directory above each.
func AppPlaceOf(exe string) policy.AppPlace {
	pl := policy.AppPlace{Path: exe}
	pl.Resolved, pl.Via, pl.ResolveErr = ResolvePath(exe, false)
	pl.IDs = idChain(exe)
	if pl.Resolved != "" {
		pl.IDs = append(pl.IDs, idChain(pl.Resolved)...)
		pl.Links = linksOf(pl.Resolved)
	}
	for _, v := range pl.Via {
		pl.IDs = append(pl.IDs, idChain(v)...)
	}
	return pl
}

// linksOf is p's count of hard links, or 0 when it cannot be told.
func linksOf(p string) uint64 {
	fi, err := os.Lstat(p)
	if err != nil {
		return 0
	}
	return linksIn(fi)
}

// linksIn is fi's count of hard links, or 0 when it does not carry one.
func linksIn(fi fs.FileInfo) uint64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0
	}
	// Nlink is of other types on other systems.
	return uint64(st.Nlink)
}

// maxBundleEntries bounds BundleEntriesOf's walk: caboose's bundle holds
// a handful of entries, and one that holds more than this is not told.
const maxBundleEntries = 10000

// errTooMany ends BundleEntriesOf's walk.
var errTooMany = errors.New("too many entries")

// BundleEntriesOf is bundle and every entry within it, for
// policy.CheckAppPlace: each one's path, identity, type and count of hard
// links, from an lstat, never following a symlink. An error when any of
// them cannot be told, or there are more than maxBundleEntries.
func BundleEntriesOf(bundle string) ([]policy.BundleEntry, error) {
	var out []policy.BundleEntry
	err := filepath.WalkDir(bundle, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if len(out) == maxBundleEntries {
			return errTooMany
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		id, ok := policy.IDOf(fi)
		if !ok {
			return fmt.Errorf("%s: no identity", p)
		}
		links := linksIn(fi)
		if links == 0 {
			return fmt.Errorf("%s: its count of hard links cannot be told", p)
		}
		out = append(out, policy.BundleEntry{Path: p, ID: id, Type: fi.Mode().Type(), Links: links})
		return nil
	})
	if errors.Is(err, errTooMany) {
		return nil, fmt.Errorf("it holds more than %d entries", maxBundleEntries)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
