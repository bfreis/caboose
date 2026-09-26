package datadir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Claude Code's native builds only run on the libc and arch they were built
// for, and they live in the persisted ~/.local. With one shared local/, a
// data dir first used with a glibc image and then with a musl one would find
// the glibc claude installed, skip the install and fail to exec it. So
// everything built for one platform lives under local/<platform>/:
//
//	local/<platform>/bin           -> ~/.local/bin
//	local/<platform>/share/claude  -> ~/.local/share/claude
//	local/<platform>/cache/claude  -> ~/.cache/claude
//
// ~/.cache/claude is split too, although it is not under ~/.local: it is
// Claude Code's update staging (cache/claude/staging), where a new version's
// binary is downloaded before it is moved into share/claude/versions. What
// is parked there is a binary for one platform, so a staged glibc download
// found by a musl container is at best ~224MB of waste and at worst a
// wrong-platform binary installed as a version. Keeping it beside the
// versions it feeds also means deleting one platform dir removes everything
// that platform left behind. (It stays a mount of its own, so a move from
// staging into versions crosses mounts either way.)
//
// ~/.local/state is not persisted (it holds per-boot locks), which is why
// ~/.local is not simply one mount of local/<platform>. It is caboose's
// machinery, not something the sandbox config keeps, so it sits beside
// home/ rather than in it.
const LocalRoot = "local"

// The pieces of a platform dir, relative to it, each its own mount.
const (
	PlatformBin   = "bin"
	PlatformShare = "share/claude"
	PlatformCache = "cache/claude"
)

// Platforms are the Claude Code installer's names for the Linux builds it
// ships (install.sh: linux-${arch}, plus -musl where /lib/libc.musl-*.so.1
// exists or `ldd /bin/ls` says musl), which is what a platform dir is named.
var Platforms = []string{"linux-x64", "linux-arm64", "linux-x64-musl", "linux-arm64-musl"}

// IsPlatform reports whether p is one of Platforms.
func IsPlatform(p string) bool {
	for _, q := range Platforms {
		if p == q {
			return true
		}
	}
	return false
}

// PlatformDir is the platform's dir, relative to the data dir.
func PlatformDir(platform string) string { return path.Join(LocalRoot, platform) }

// PlatformMounts are the data-dir-relative sources of the platform's
// mounts, in the order they are mounted.
func PlatformMounts(platform string) []string {
	d := PlatformDir(platform)
	return []string{path.Join(d, PlatformBin), path.Join(d, PlatformShare), path.Join(d, PlatformCache)}
}

// EnsurePlatformLayout creates the platform's mount sources. Unlike
// EnsureLayout it is only run when a container is about to be created, for
// the platform of the image it is created from.
func EnsurePlatformLayout(dir, platform string) error {
	if !IsPlatform(platform) {
		return fmt.Errorf("%q is not a platform", platform)
	}
	for _, d := range PlatformMounts(platform) {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o777); err != nil {
			return err
		}
	}
	return nil
}

// PlatformDirsIn lists the platform dirs local/ holds, sorted. Anything
// else in it is not listed.
func PlatformDirsIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, LocalRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ps []string
	for _, e := range entries {
		if e.IsDir() && IsPlatform(e.Name()) {
			ps = append(ps, e.Name())
		}
	}
	sort.Strings(ps)
	return ps, nil
}

// containerVersions is where the installer's symlink points, inside the
// container.
const containerVersions = "/home/agent/.local/share/claude/versions/"

// HasInstall reports whether local, a platform dir, holds a Claude Code for
// the entrypoint to run, as its `[ -e ~/.local/bin/claude ]` will see it from
// inside the container: bin/claude, followed to what it names. The installer's symlink names the container
// path containerVersions<v>, which is dangling on the host, so that one is
// looked up under local's share/claude/versions instead. A false answer
// means the container about to start on local installs Claude Code first.
func HasInstall(local string) bool {
	bin := filepath.Join(local, PlatformBin, "claude")
	if target, err := os.Readlink(bin); err == nil && strings.HasPrefix(target, containerVersions) {
		bin = filepath.Join(local, filepath.FromSlash(PlatformShare), "versions", strings.TrimPrefix(target, containerVersions))
	}
	_, err := os.Stat(bin)
	return err == nil
}

// MountedLocalDir is the platform dir holding bin/ and share/claude/ for a
// container, given the host source of its ~/.local/bin mount, and the
// platform it is for; ok is false when that source is in no platform dir.
func MountedLocalDir(binSource string) (dir, platform string, ok bool) {
	dir = filepath.Dir(binSource)
	if !IsPlatform(filepath.Base(dir)) {
		return "", "", false
	}
	return dir, filepath.Base(dir), true
}
