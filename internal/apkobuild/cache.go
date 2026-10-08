package apkobuild

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The package cache (Options.CacheDir) is apko's, laid out as
//
//	<repository URL, query-escaped>/<arch>/<name>-<version>/   one package: its control, data and
//	                                                           signature, as links into a dir of its own
//	<repository URL, query-escaped>/<arch>/APKINDEX/<etag>.tar.gz   one index per etag seen, a link to a .tmp beside it
//	<key's dir URL, query-escaped>/<dir>/<key>.rsa.pub/            the signing key, by etag
//
// A package's dir is named by its URL, as the lock records it: the URL's
// last two path components are the arch and the file, minus ".apk", and
// what comes before them is the repository.

// CachePrune is what PruneCache removed.
type CachePrune struct {
	// Packages is how many packages' dirs went, Indexes how many old index
	// files.
	Packages, Indexes int
	// Bytes is what the regular files removed took.
	Bytes int64
}

// packageDir matches a package's dir: apk versions end in -r<N>.
var packageDir = regexp.MustCompile(`-r[0-9]+$`)

// cacheKey is the dir a package at rawURL is cached in, relative to the
// cache: what apko's cachePathFromURL makes of it, minus ".apk".
func cacheKey(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("package URL %q: %w", rawURL, err)
	}
	u.ForceQuery, u.RawQuery, u.RawFragment, u.Fragment = false, "", "", ""
	file := path.Base(u.Path)
	name, ok := strings.CutSuffix(file, ".apk")
	if !ok || name == "" {
		return "", fmt.Errorf("package URL %q does not name an .apk", rawURL)
	}
	archDir := path.Dir(u.Path)
	u.Path = path.Dir(archDir)
	u.RawPath = ""
	return filepath.Join(url.QueryEscape(u.String()), path.Base(archDir), name), nil
}

// PruneCache removes from dir, a package cache, every package no lock of
// keep names, and every index but the newest of each repository and
// architecture (a build fetches the current one anyway). Anything modified
// after recent stays: a build that is fetching packages has not written its
// lock yet. It never follows a link: a dir is entered only when it is one,
// and a link is removed, never what it points to, unless that is a file
// beside it. What it does not recognise, it leaves. A dir that does not
// exist is an empty cache. It holds the cache's lock exclusive meanwhile,
// and removes nothing, returning ErrCacheInUse, while anything else holds
// it (LockCache). On an error, what was removed until then is
// still returned.
func PruneCache(dir string, keep []*Lock, recent time.Time) (CachePrune, error) {
	var r CachePrune
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	unlock, ok, err := tryLockCacheExclusive(dir)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, ErrCacheInUse
	}
	defer unlock()
	locked := map[string]bool{}
	for _, l := range keep {
		for _, p := range l.l.Contents.Packages {
			k, err := cacheKey(p.URL)
			if err != nil {
				return r, err
			}
			locked[k] = true
		}
	}
	repos, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		arches, err := os.ReadDir(filepath.Join(dir, repo.Name()))
		if err != nil {
			return r, err
		}
		for _, arch := range arches {
			if !arch.IsDir() {
				continue
			}
			rel := filepath.Join(repo.Name(), arch.Name())
			entries, err := os.ReadDir(filepath.Join(dir, rel))
			if err != nil {
				return r, err
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := filepath.Join(dir, rel, e.Name())
				switch {
				case e.Name() == "APKINDEX":
					if err := pruneIndexes(p, recent, &r); err != nil {
						return r, err
					}
				case packageDir.MatchString(e.Name()) && !locked[filepath.Join(rel, e.Name())]:
					size, newest, err := treeInfo(p)
					if err != nil {
						return r, err
					}
					if newest.After(recent) {
						continue
					}
					if err := os.RemoveAll(p); err != nil {
						return r, err
					}
					r.Packages++
					r.Bytes += size
				}
			}
		}
	}
	return r, nil
}

// treeInfo is what the regular files under p take, and the newest
// modification time of p or anything under it, links not followed.
func treeInfo(p string) (size int64, newest time.Time, err error) {
	err = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	return size, newest, err
}

// pruneIndexes removes from an APKINDEX dir every index but the newest, and
// the files beside them that no index left links to, keeping anything
// modified after recent.
func pruneIndexes(dir string, recent time.Time, r *CachePrune) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	infos := map[string]fs.FileInfo{}
	newest := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		infos[e.Name()] = fi
		if strings.HasSuffix(e.Name(), ".tar.gz") && (newest == "" || fi.ModTime().After(infos[newest].ModTime())) {
			newest = e.Name()
		}
	}
	// What a kept index links to stays.
	kept := map[string]bool{}
	for name, fi := range infos {
		if !strings.HasSuffix(name, ".tar.gz") || name != newest && !fi.ModTime().After(recent) {
			continue
		}
		kept[name] = true
		if t, ok := besideTarget(dir, name, fi); ok {
			kept[t] = true
		}
	}
	for name, fi := range infos {
		if kept[name] || fi.ModTime().After(recent) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if strings.HasSuffix(name, ".tar.gz") {
			r.Indexes++
		}
		if fi.Mode().IsRegular() {
			r.Bytes += fi.Size()
		}
	}
	return nil
}

// besideTarget is the name of the file in dir that the link name points to,
// when it is one and points to a plain name there.
func besideTarget(dir, name string, fi fs.FileInfo) (string, bool) {
	if fi.Mode()&fs.ModeSymlink == 0 {
		return "", false
	}
	t, err := os.Readlink(filepath.Join(dir, name))
	if err != nil || t != filepath.Base(t) || t == "." || t == ".." {
		return "", false
	}
	return t, true
}
