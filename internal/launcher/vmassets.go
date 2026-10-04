package launcher

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/selfupdate"
)

// A release carries vm's files for each architecture a guest can have, as
// one asset beside the launcher's archives, listed in checksums.txt: the
// kernel (with its config and where its source is) and the builder's disk.
// A release build fetches its own version's the first time vm needs them,
// into CABOOSE_HOME/vm/<tag>/<arch>/, and keeps those of the versions
// installed; a checkout makes them with make vm-kernel and make
// vm-builder instead.

// vmAssetName is the release asset of tag's vm files for arch.
func vmAssetName(tag, arch string) string {
	return fmt.Sprintf("caboose-vm_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), arch)
}

// vmAssetFiles are the names the asset may hold, for arch: nothing else is
// taken from it.
func vmAssetFiles(arch string) []string {
	k := "kernel-" + arch
	return []string{k, k + ".config", k + ".SOURCE", "builder-" + arch + ".img"}
}

// maxVMAsset bounds the download, and each file in it: the builder's disk
// is some 400 MB.
const maxVMAsset = 4 << 30

// releaseTag is this launcher's release tag, or "" for a build that is no
// release (make, go build): one that has no release to fetch from.
func (a *App) releaseTag() string {
	if a.installed().kind == installDev {
		return ""
	}
	return currentVersion()
}

// vmReleaseDir is where a release build keeps its version's vm files.
func (a *App) vmReleaseDir(tag, arch string) string {
	return filepath.Join(a.Cfg.CabooseHome, vmDirName, tag, arch)
}

// ensureVMFiles is findVMFiles, fetching this release's vm files first
// when a release build lacks them.
func (a *App) ensureVMFiles() (vmFiles, error) {
	f, err := a.findVMFiles()
	tag := a.releaseTag()
	if err == nil || tag == "" || f.VMM == "" || !isFile(f.VMM) || (f.Kernel != "" && f.Builder != "") {
		return f, err
	}
	if err := a.fetchVMFiles(tag, f.Arch); err != nil {
		return f, fmt.Errorf("isolation vm needs its kernel and builder for %s, and fetching them failed: %v", tag, err)
	}
	return a.findVMFiles()
}

// fetchVMFiles downloads and unpacks tag's vm files for arch, checked
// against the release's checksums.txt, into vmReleaseDir: whole, or not at
// all.
func (a *App) fetchVMFiles(tag, arch string) error {
	name := vmAssetName(tag, arch)
	root := filepath.Join(a.Cfg.CabooseHome, vmDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	a.Note("fetching %s: the vm isolation's kernel and builder for this caboose (a few hundred MB, once per version)", name)
	tmp, err := os.CreateTemp(root, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := a.releases().Download(ctx, tag, name, tmp, maxVMAsset); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dest := a.vmReleaseDir(tag, arch)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part, err := os.MkdirTemp(filepath.Dir(dest), "."+arch+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(part)
	if err := unpackVMAsset(tmp, part, vmAssetFiles(arch)); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := os.Chmod(part, 0o755); err != nil {
		return err
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(part, dest); err != nil {
		return err
	}
	a.pruneVMFiles(tag)
	return nil
}

// unpackVMAsset writes the files of a vm asset (a .tar.gz) that are among
// names into dir: regular files only, each at most maxVMAsset, nothing
// outside dir, and every one of names there.
func unpackVMAsset(r io.Reader, dir string, names []string) error {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	got := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if !want[name] || h.Typeflag != tar.TypeReg || got[name] {
			continue
		}
		if h.Size > maxVMAsset {
			return fmt.Errorf("%s is larger than %d bytes", name, int64(maxVMAsset))
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, werr := io.Copy(f, io.LimitReader(tr, maxVMAsset))
		if err := errors.Join(werr, f.Close()); err != nil {
			return err
		}
		got[name] = true
	}
	for _, n := range names {
		if !got[n] {
			return fmt.Errorf("it has no %s", n)
		}
	}
	return nil
}

// pruneVMFiles removes the vm files of versions that are neither tag nor
// installed: a self-update brings a new version's, and the old go with
// the old versions.
func (a *App) pruneVMFiles(tag string) {
	root := filepath.Join(a.Cfg.CabooseHome, vmDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		t := e.Name()
		if !e.IsDir() || !selfupdate.Valid(t) || t == tag {
			continue
		}
		if _, err := os.Stat(filepath.Join(a.layout().Versions, t)); err == nil {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, t))
	}
}

// vmFetchable reports whether f lacks only what this release build fetches
// the first time vm needs it: caboose-vmm is there, the kernel or the
// builder is not.
func (a *App) vmFetchable(f vmFiles) bool {
	return a.releaseTag() != "" && f.VMM != "" && isFile(f.VMM) && (f.Kernel == "" || f.Builder == "")
}
