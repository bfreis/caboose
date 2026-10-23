package appbundle

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/saferm"
)

// remove is how Assemble removes what it made or replaced: through saferm,
// confined to the bundle's parent directory. A variable for the tests.
var remove = saferm.RemoveAll

// Assemble builds the bundle at dir around exe, the launcher's binary:
// Contents/Info.plist from info, and a copy of exe as
// Contents/MacOS/caboose (mode 0755). It builds in a temporary directory
// beside dir and renames it into place. An existing dir is replaced only
// when it is caboose's bundle (named Caboose.app, its Info.plist naming
// one of caboose's identifiers); anything else there is refused and left
// as it is. Every removal is confined to dir's parent.
func Assemble(dir, exe string, info Info) error {
	if info.ID == "" {
		return errors.New("the bundle needs an identifier")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	base := filepath.Base(abs)
	if !strings.HasSuffix(base, ".app") || len(base) <= len(".app") {
		return fmt.Errorf("%s: a bundle's name ends in .app", dir)
	}
	parent := filepath.Dir(abs)
	replace, err := checkTarget(abs)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+base+".tmp-")
	if err != nil {
		return err
	}
	// MkdirTemp's mode is 0700; a bundle is read by others.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return errors.Join(err, remove(parent, tmp))
	}
	if err := fill(tmp, exe, info); err != nil {
		return errors.Join(err, remove(parent, tmp))
	}
	if !replace {
		if err := os.Rename(tmp, abs); err != nil {
			return errors.Join(err, remove(parent, tmp))
		}
		return nil
	}
	// The old bundle is moved aside into a directory of its own, the new
	// one put in its place, and only then the old one removed.
	aside, err := os.MkdirTemp(parent, "."+base+".tmp-")
	if err != nil {
		return errors.Join(err, remove(parent, tmp))
	}
	old := filepath.Join(aside, "old")
	if err := os.Rename(abs, old); err != nil {
		return errors.Join(err, remove(parent, tmp), remove(parent, aside))
	}
	if err := os.Rename(tmp, abs); err != nil {
		// Put the old one back.
		return errors.Join(err, os.Rename(old, abs), remove(parent, tmp), remove(parent, aside))
	}
	return remove(parent, aside)
}

// checkTarget says whether dir exists and may be replaced; an error when
// something is there that is not caboose's bundle.
func checkTarget(dir string) (bool, error) {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, fmt.Errorf("refusing to replace %s: it is not a directory", dir)
	case filepath.Base(dir) != Name:
		return false, fmt.Errorf("refusing to replace %s: only a bundle named %s is replaced", dir, Name)
	}
	plist := filepath.Join(dir, "Contents", "Info.plist")
	if pi, err := os.Lstat(plist); err != nil || !pi.Mode().IsRegular() {
		return false, fmt.Errorf("refusing to replace %s: it holds no Info.plist of caboose's", dir)
	}
	b, err := ReadPlist(plist)
	if err != nil {
		return false, err
	}
	id, err := ParseIdentifier(b)
	if err != nil || !Known(id) {
		return false, fmt.Errorf("refusing to replace %s: it is not caboose's bundle", dir)
	}
	return true, nil
}

// fill writes the bundle's files into dir.
func fill(dir, exe string, info Info) error {
	macos := filepath.Join(dir, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Contents", "Info.plist"), Plist(info), 0o644); err != nil {
		return err
	}
	return copyExe(exe, filepath.Join(macos, Executable))
}

func copyExe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(0o755); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
