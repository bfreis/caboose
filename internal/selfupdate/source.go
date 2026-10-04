// Package selfupdate keeps an install of caboose made by install.sh
// current: it finds the latest release, downloads the archive for this
// platform with the release's checksums.txt, checks the one against the
// other, and installs it next to the one running (Layout).
//
// The checksums come from the same release as the archives, so they catch
// a download cut short or damaged, not a release someone else published:
// verify is the one place a signature check goes when there is one.
package selfupdate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// DefaultReleases is where caboose's releases are published.
const DefaultReleases = "https://github.com/bfreis/caboose/releases"

// ChecksumsFile is the release asset that lists every archive's sha256.
const ChecksumsFile = "checksums.txt"

// maxArchive bounds what a download may be: a release archive is a few MB.
const maxArchive = 256 << 20

// Source is a place releases are published, laid out as GitHub's:
// BASE/latest redirects to BASE/tag/TAG, and a release's files are at
// BASE/download/TAG/NAME.
type Source struct {
	Base   string
	Client *http.Client // nil: http.DefaultClient
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// Latest is the tag of the latest release, read from where BASE/latest
// redirects -- no API, so no rate limit. It is the latest stable release:
// drafts and prereleases are never "latest".
func (s Source) Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(s.Base, "/")+"/latest", nil)
	if err != nil {
		return "", err
	}
	c := *s.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking for the latest release: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return "", fmt.Errorf("asking for the latest release: %s gave %s, not a redirect to it", req.URL, resp.Status)
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("the latest release: a redirect to %q: %w", loc, err)
	}
	// Only a caboose version: another release of the repository's, as
	// the vm kernel's source (kernel-VERSION), is never an update.
	tag := path.Base(u.Path)
	if !strings.Contains(u.Path, "/tag/") || !Valid(tag) {
		return "", fmt.Errorf("the latest release: %s redirects to %s, which is no caboose version", req.URL, loc)
	}
	return tag, nil
}

// AssetName is the archive of tag for a platform, as .goreleaser.yaml
// names it.
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("caboose_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// fetch writes the release file name of tag to w, at most limit bytes.
func (s Source) fetch(ctx context.Context, tag, name string, w io.Writer, limit int64) error {
	u := strings.TrimSuffix(s.Base, "/") + "/download/" + tag + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s", u, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	if n > limit {
		return fmt.Errorf("downloading %s: larger than %d bytes", name, limit)
	}
	return nil
}

// checksumFor finds name's sha256 in a checksums.txt ("HASH  NAME" lines,
// as sha256sum writes them).
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if _, err := hex.DecodeString(f[0]); err != nil || len(f[0]) != 64 {
				return "", fmt.Errorf("%s: %q is not a sha256", ChecksumsFile, f[0])
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s lists no %s", ChecksumsFile, name)
}

// errChecksum is an archive whose sha256 is not the one its release lists.
var errChecksum = errors.New("checksum mismatch")

// verify checks data, the archive name, against the release's checksums.
// A signature over sums would be checked here first, once releases carry
// one.
func verify(data, sums []byte, name string) error {
	want, err := checksumFor(sums, name)
	if err != nil {
		return err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s: %w (%s lists %s, the download is %s)", name, errChecksum, ChecksumsFile, want[:12], hex.EncodeToString(got[:])[:12])
	}
	return nil
}

// Download writes the release file name of tag to w, at most limit bytes,
// and returns nil only when its sha256 is the one the release's
// checksums.txt lists: w then holds all of it. A file too large to hold in
// memory (vm's builder disk) goes to a file this way.
func (s Source) Download(ctx context.Context, tag, name string, w io.Writer, limit int64) error {
	var sums bytes.Buffer
	if err := s.fetch(ctx, tag, ChecksumsFile, &sums, 1<<20); err != nil {
		return err
	}
	want, err := checksumFor(sums.Bytes(), name)
	if err != nil {
		return err
	}
	h := sha256.New()
	if err := s.fetch(ctx, tag, name, io.MultiWriter(w, h), limit); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: %w (%s lists %s, the download is %s)", name, errChecksum, ChecksumsFile, want[:12], got[:12])
	}
	return nil
}
