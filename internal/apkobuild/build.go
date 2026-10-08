package apkobuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"chainguard.dev/apko/pkg/build"
	"chainguard.dev/apko/pkg/build/oci"
	"chainguard.dev/apko/pkg/build/types"
	"chainguard.dev/apko/pkg/tarfs"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Build builds exactly the locked packages into an image tagged tag and
// writes it to w as the tarball `docker load` reads. The image has apko's
// defaults for its entrypoint, command and user, and is built at a fixed time,
// so one lock gives one digest. Build returns that digest.
func Build(ctx context.Context, l *Lock, o Options, tag string, w io.Writer) (digest string, err error) {
	if err := o.check(); err != nil {
		return "", err
	}
	if a := l.Arch(); a != o.Arch {
		return "", fmt.Errorf("the lock is for %s but the build is for %s: resolve the packages again", a, o.Arch)
	}
	unlock, err := LockCache(o.CacheDir)
	if err != nil {
		return "", err
	}
	defer unlock()
	ref, err := name.NewTag(tag)
	if err != nil {
		return "", fmt.Errorf("image tag %q: %w", tag, err)
	}
	dir, err := os.MkdirTemp("", "caboose-apko-*")
	if err != nil {
		return "", fmt.Errorf("preparing the build: %w", err)
	}
	defer os.RemoveAll(dir)
	lockPath := filepath.Join(dir, "lock.json")
	if err := l.Write(lockPath); err != nil {
		return "", err
	}

	opts, err := o.buildOptions(l.Input())
	if err != nil {
		return "", err
	}
	opts = append(opts, build.WithLockFile(lockPath), build.WithTempDir(dir))
	bc, err := build.New(ctx, tarfs.New(), opts...)
	if err != nil {
		return "", fmt.Errorf("preparing the build: %w", err)
	}
	layers, err := bc.BuildLayers(ctx)
	if err != nil {
		return "", fmt.Errorf("downloading and installing the locked packages: %w", err)
	}
	img, err := oci.BuildImageFromLayers(ctx, bc.BaseImage(), layers, bc.ImageConfiguration(), epoch, types.ParseArchitecture(o.Arch))
	if err != nil {
		return "", fmt.Errorf("building the image: %w", err)
	}
	h, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("building the image: %w", err)
	}
	if err := tarball.Write(ref, img, w); err != nil {
		return "", fmt.Errorf("writing the image tarball: %w", err)
	}
	return h.String(), nil
}
