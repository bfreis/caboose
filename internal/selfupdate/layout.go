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

// Layout is where install.sh puts caboose, as Claude Code's installer puts
// claude: every version in a dir of its own under Versions, and Link, the
// command on PATH, a symlink to the current one. Switching versions is
// replacing Link, one rename; the one before stays, for going back.
type Layout struct {
	Link     string // ~/.local/bin/caboose
	Versions string // ~/.local/share/caboose/versions
}

// DefaultLayout is the layout under home. install.sh must agree with it.
func DefaultLayout(home string) Layout {
	return Layout{
		Link:     filepath.Join(home, ".local", "bin", Binary),
		Versions: filepath.Join(home, ".local", "share", "caboose", "versions"),
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
	bin, err := extract(archive.Bytes())
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return l.place(tag, bin, running)
}

// place installs bin as tag's binary and switches Link to it.
func (l Layout) place(tag string, bin []byte, running string) error {
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
	if err := os.WriteFile(filepath.Join(tmp, Binary), bin, 0o755); err != nil {
		return err
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

// extract is the caboose binary in a release archive (a .tar.gz with it at
// its root, next to README.md).
func extract(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s in the archive", Binary)
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(h.Name, "./") != Binary || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxArchive {
			return nil, fmt.Errorf("%s is larger than %d bytes", Binary, maxArchive)
		}
		return io.ReadAll(io.LimitReader(tr, maxArchive))
	}
}
