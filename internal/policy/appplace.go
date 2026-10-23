package policy

import (
	"fmt"
	"io/fs"
	"path"
)

// AppPlace is where the menu-bar app runs from, as the caller found it on
// the disk: the facts CheckAppPlace judges.
type AppPlace struct {
	// Path is the app's executable as the system names it, absolute.
	// Resolved is the same with every symlink in it followed (the
	// physical path), and Via every symlink met on the way, as the path
	// it was met at. ResolveErr is set when it could not be resolved.
	Path, Resolved string
	Via            []string
	ResolveErr     error
	// Bundle is the bundle directory Path is the executable of
	// (B.app/Contents/MacOS/caboose), "" when it is none: what the host
	// reads besides the executable (Info.plist, resources), so nothing
	// within it may be reached either.
	Bundle string
	// BundleEntries are the bundle and every entry within it, as found
	// on the disk without following a symlink; BundleErr is set when
	// they could not all be told.
	BundleEntries []BundleEntry
	BundleErr     error
	// IDs are the identities of Path, of Resolved and of every directory
	// above each, as found on the disk.
	IDs []FileID
	// Links is the executable's count of hard links, 0 when it could not
	// be told. Another link elsewhere, which a sandbox might reach, is
	// the same file under a name no check sees.
	Links uint64
}

// BundleEntry is one entry of the app's bundle as the disk has it: its
// path, its identity, its type (fs.FileMode's type bits, from an lstat)
// and its count of hard links. The host reads what is in the bundle, so
// an entry that is the same file or directory as one a sandbox reaches,
// under another name, is as reached as the name itself.
type BundleEntry struct {
	Path  string
	ID    FileID
	Type  fs.FileMode
	Links uint64
}

// AppReach is what the sandboxes reach, or will at their next start: every
// root of every environment, read-only or not, and every path a sandbox
// mounts, physical where they resolve, with the identity of each that
// exists. Err is set when it cannot be told, which refuses every place:
// an unknown reach is everything. Everything names the setting that hands
// a sandbox what no list of paths holds (the engine's socket, a
// privileged container), "" when none does: it refuses every place too.
type AppReach struct {
	Paths      []string
	IDs        []FileID
	Err        error
	Everything string
}

// CheckAppPlace refuses the menu-bar app's place when a sandbox could
// change what runs there: the host runs the app, at every login once it
// is a login item, so it may not run from anything a sandbox reaches. A
// place is refused when what the sandboxes reach cannot be told or is
// everything, when it
// cannot be resolved, when it is reached through a symlink (judged as
// written and as resolved: a sandbox could repoint a link), when it or a
// directory above it is within a reached path by name (case aside), when
// a reached path is within its bundle, or
// has the identity of one (a firmlink, another normalization of a name, a
// second mount of the same directory), or when the executable has any
// other hard link, or how many cannot be told. Nil is the place allowed.
// It does no I/O.
func CheckAppPlace(pl AppPlace, r AppReach) error {
	shownPath := clip(printable(pl.Path), 300)
	switch {
	case r.Err != nil:
		return fmt.Errorf("what the sandboxes reach cannot be told now, so no place is safe until it can: %s", clip(printable(r.Err.Error()), 300))
	case r.Everything != "":
		return fmt.Errorf("a sandbox reaches everything on this machine through %s, so no place is safe while it is set", clip(printable(r.Everything), 300))
	case pl.ResolveErr != nil:
		return fmt.Errorf("it cannot be looked at: %s: %s", shownPath, clip(printable(pl.ResolveErr.Error()), 300))
	case !absClean(pl.Path) || !absClean(pl.Resolved):
		return fmt.Errorf("it is not a clean absolute path: %s", shownPath)
	case len(pl.Via) > 0:
		return fmt.Errorf("it is reached through a symlink, which could be repointed: %s goes through %s", shownPath, clip(printable(pl.Via[0]), 300))
	case pl.Resolved != pl.Path:
		return fmt.Errorf("it is not where it is named: %s is %s once resolved", shownPath, clip(printable(pl.Resolved), 300))
	}
	for _, s := range []string{pl.Path, pl.Resolved} {
		for _, w := range r.Paths {
			if w != "" && within(s, w) {
				return fmt.Errorf("a sandbox reaches it, and could replace what the host runs: %s is within %s", shownPath, clip(printable(w), 300))
			}
		}
	}
	if pl.Bundle != "" {
		if !absClean(pl.Bundle) || !within(pl.Path, pl.Bundle) {
			return fmt.Errorf("its bundle %s does not hold it: %s", clip(printable(pl.Bundle), 300), shownPath)
		}
		for _, w := range r.Paths {
			if w != "" && path.IsAbs(w) && within(w, pl.Bundle) {
				return fmt.Errorf("a sandbox reaches into its bundle, and could change what the host runs: %s is within %s", clip(printable(w), 300), clip(printable(pl.Bundle), 300))
			}
		}
	}
	if err := checkBundleEntries(pl, r); err != nil {
		return err
	}
	if len(r.IDs) > 0 && len(pl.IDs) == 0 {
		return fmt.Errorf("it has no identity to compare with what the sandboxes reach: %s", shownPath)
	}
	p := Policy{WritableIDs: r.IDs}
	if p.writableID(pl.IDs) {
		return fmt.Errorf("a sandbox reaches it by another name, and could replace what the host runs: %s", shownPath)
	}
	if pl.Links != 1 {
		return fmt.Errorf("it has %d hard links, not 1: another name for it could be one a sandbox reaches: %s", pl.Links, shownPath)
	}
	return nil
}

// checkBundleEntries judges what is in pl's bundle by identity: the names
// alone (CheckAppPlace's) miss a reached directory spelled another way
// inside the bundle, and a hard link between a file of the bundle and
// one a sandbox reaches.
func checkBundleEntries(pl AppPlace, r AppReach) error {
	if pl.Bundle == "" {
		return nil
	}
	bundle := clip(printable(pl.Bundle), 300)
	switch {
	case pl.BundleErr != nil:
		return fmt.Errorf("what is in its bundle %s cannot be told: %s", bundle, clip(printable(pl.BundleErr.Error()), 300))
	case len(pl.BundleEntries) == 0:
		return fmt.Errorf("what is in its bundle %s was not looked at", bundle)
	}
	ids := Policy{WritableIDs: r.IDs}
	for _, e := range pl.BundleEntries {
		shown := clip(printable(e.Path), 300)
		switch t := e.Type.Type(); {
		case !absClean(e.Path) || !within(e.Path, pl.Bundle):
			return fmt.Errorf("its bundle %s holds %s, which is not within it", bundle, shown)
		case t == fs.ModeSymlink:
			return fmt.Errorf("its bundle holds a symlink, which could point where a sandbox reaches: %s", shown)
		case t != 0 && t != fs.ModeDir:
			return fmt.Errorf("its bundle holds what is neither a directory nor a plain file: %s", shown)
		case t == 0 && e.Links != 1:
			return fmt.Errorf("its bundle holds a file with %d hard links, not 1: another name for it could be one a sandbox reaches: %s", e.Links, shown)
		case ids.writableID([]FileID{e.ID}):
			return fmt.Errorf("a sandbox reaches into its bundle by another name, and could change what the host runs: %s", shown)
		}
	}
	return nil
}
