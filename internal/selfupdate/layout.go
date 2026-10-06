package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Binary is the executable's name, in an archive and in a version's dir.
const Binary = "caboose"

// VMM is the one other executable an archive may hold: caboose-vmm, on
// macOS, which runs a vm isolation's VM and sits next to Binary.
const VMM = "caboose-vmm"

// Layout is where install.sh puts caboose, as Claude Code's installer puts
// claude: every version in a dir of its own under Versions, and Link, the
// command on PATH, a symlink to the current one. Switching versions is
// replacing Link, one rename; the one before stays, for going back.
type Layout struct {
	Link     string // ~/.local/bin/caboose, the one thing outside CABOOSE_HOME
	Versions string // CABOOSE_HOME/versions
}

// DefaultLayout is the layout of an install under cabooseHome (CABOOSE_HOME,
// ~/.caboose by default), its command linked from home's ~/.local/bin.
// install.sh must agree with it.
func DefaultLayout(home, cabooseHome string) Layout {
	return Layout{
		Link:     filepath.Join(home, ".local", "bin", Binary),
		Versions: filepath.Join(cabooseHome, "versions"),
	}
}

// Keep is how many versions an update leaves: the new one, and the one
// before it.
const Keep = 2

// Managed reports whether exe, the running executable, is a version this
// layout holds -- VERSIONS/TAG/caboose, symlinks resolved -- and so one
// install.sh made, which may replace itself. A binary built from a
// checkout, by go install, or by a package manager is somewhere else, and is
// left to whatever put it there.
func (l Layout) Managed(exe string) (tag string, ok bool) {
	exe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", false
	}
	versions, err := filepath.EvalSymlinks(l.Versions)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(versions, exe)
	if err != nil {
		return "", false
	}
	dir, file := filepath.Split(rel)
	tag = strings.TrimSuffix(dir, string(filepath.Separator))
	if file != Binary || !Valid(tag) || strings.Contains(tag, string(filepath.Separator)) {
		return "", false
	}
	return tag, true
}

// Current is the tag Link points at, or "".
func (l Layout) Current() string {
	target, err := os.Readlink(l.Link)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(l.Link), target)
	}
	dir, file := filepath.Split(filepath.Clean(target))
	tag := filepath.Base(dir)
	if file != Binary || filepath.Clean(filepath.Dir(filepath.Clean(dir))) != filepath.Clean(l.Versions) || !Valid(tag) {
		return ""
	}
	return tag
}

// Install downloads tag's archive for goos/goarch from src, checks it
// against the release's checksums, and makes it the current version: its
// binary in VERSIONS/TAG, Link pointed at it. Versions beyond Keep go,
// except the one Link pointed at before and running, the executable doing
// this (which may be neither). Nothing is changed until the download is
// whole and checked.
func (l Layout) Install(ctx context.Context, src Source, tag, goos, goarch, running string) error {
	name := AssetName(tag, goos, goarch)
	var sums, archive bytes.Buffer
	if err := src.fetch(ctx, tag, ChecksumsFile, &sums, 1<<20); err != nil {
		return err
	}
	if err := src.fetch(ctx, tag, name, &archive, maxArchive); err != nil {
		return err
	}
	if err := verify(archive.Bytes(), sums.Bytes(), name); err != nil {
		return err
	}
	bins, err := extract(archive.Bytes())
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return l.place(tag, bins, running)
}

// place installs bins, by name, as tag's executables and switches Link to
// its Binary.
func (l Layout) place(tag string, bins map[string][]byte, running string) error {
	if err := os.MkdirAll(l.Versions, 0o755); err != nil {
		return err
	}
	// Written whole into a dir of its own, then renamed into place: a
	// version's dir either is complete or is not there.
	tmp, err := os.MkdirTemp(l.Versions, "."+tag+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for name, bin := range bins {
		if err := os.WriteFile(filepath.Join(tmp, name), bin, 0o755); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(l.Versions, tag)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	before := l.Current()
	if err := l.link(filepath.Join(dest, Binary)); err != nil {
		return err
	}
	keep := map[string]bool{tag: true, before: true}
	if t, ok := l.Managed(running); ok {
		keep[t] = true
	}
	l.prune(keep)
	return nil
}

// link points Link at target by creating the new symlink next to it and
// renaming it over, so Link is always one or the other.
func (l Layout) link(target string) error {
	if err := os.MkdirAll(filepath.Dir(l.Link), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(l.Link), ".caboose.new")
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.Link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// prune removes versions beyond Keep, newest kept first, never one in keep,
// and what an interrupted install left behind. Failing to remove one is
// not an error: it stays until the next update.
func (l Layout) prune(keep map[string]bool) {
	entries, err := os.ReadDir(l.Versions)
	if err != nil {
		return
	}
	var tags []string
	for _, e := range entries {
		switch name := e.Name(); {
		case strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-"):
			os.RemoveAll(filepath.Join(l.Versions, name))
		case e.IsDir() && Valid(name):
			tags = append(tags, name)
		}
	}
	// Newest first.
	for i := 1; i < len(tags); i++ {
		for j := i; j > 0 && Compare(tags[j], tags[j-1]) > 0; j-- {
			tags[j], tags[j-1] = tags[j-1], tags[j]
		}
	}
	kept := 0
	for _, t := range tags {
		if keep[t] {
			kept++
		}
	}
	for _, t := range tags {
		if keep[t] {
			continue
		}
		if kept < Keep {
			kept++
			continue
		}
		os.RemoveAll(filepath.Join(l.Versions, t))
	}
}

// extract is the executables in a release archive (a .tar.gz with them at
// its root, next to README.md): Binary, which it must hold, and VMM, when
// it does.
func extract(archive []byte) (map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	bins := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if (name != Binary && name != VMM) || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxArchive {
			return nil, fmt.Errorf("%s is larger than %d bytes", name, maxArchive)
		}
		if bins[name], err = io.ReadAll(io.LimitReader(tr, maxArchive)); err != nil {
			return nil, err
		}
	}
	if bins[Binary] == nil {
		return nil, fmt.Errorf("no %s in the archive", Binary)
	}
	return bins, nil
}
