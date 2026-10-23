// Package releasetest serves fake caboose releases, laid out as GitHub
// serves them, for the tests of whatever downloads one: the updater, and
// install.sh.
package releasetest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/selfupdate"
)

// Server is a fake release host. Base is what CABOOSE_RELEASES_URL (or
// selfupdate.Source.Base) should be.
type Server struct {
	Base string
	// VMM puts caboose-vmm (VMMBinary) in the archives Publish makes for
	// darwin, as the real releases do.
	VMM bool

	mu     sync.Mutex
	latest string
	files  map[string][]byte // "TAG/NAME"
	hits   []string
}

// New starts a server with no releases; it stops with the test.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/", s.serve)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.Base = srv.URL + "/releases"
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = append(s.hits, r.Method+" "+r.URL.Path)
	rest := strings.TrimPrefix(r.URL.Path, "/releases/")
	switch {
	case rest == "latest":
		if s.latest == "" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+s.latest, http.StatusFound)
	case strings.HasPrefix(rest, "download/"):
		b, ok := s.files[strings.TrimPrefix(rest, "download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// Binary is the content Publish gives tag's caboose: a script, so a test
// can run an installed one.
func Binary(tag string) []byte {
	return []byte("#!/bin/sh\necho \"caboose " + tag + "\" \"$@\"\n")
}

// VMMBinary is the content Publish gives tag's caboose-vmm.
func VMMBinary(tag string) []byte {
	return []byte("#!/bin/sh\necho \"caboose-vmm " + tag + "\" \"$@\"\n")
}

// Publish adds release tag for the given platforms ("linux/arm64", ...),
// each archive holding Binary(tag) as caboose, next to a README.md, and
// checksums.txt listing them. With latest, it becomes the latest release.
func (s *Server) Publish(t *testing.T, tag string, latest bool, platforms ...string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var sums strings.Builder
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		name := selfupdate.AssetName(tag, goos, goarch)
		files := map[string][]byte{"caboose": Binary(tag), "README.md": []byte("readme\n")}
		if s.VMM && goos == "darwin" {
			files[selfupdate.VMM] = VMMBinary(tag)
		}
		a := Archive(t, files)
		s.files[tag+"/"+name] = a
		sum := sha256.Sum256(a)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	s.files[tag+"/"+selfupdate.ChecksumsFile] = []byte(sums.String())
	if latest {
		s.latest = tag
	}
}

// Add adds a file to release tag, listed in its checksums.txt, as the
// release workflow adds vm's files.
func (s *Server) Add(tag, name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[tag+"/"+name] = data
	sum := sha256.Sum256(data)
	s.files[tag+"/"+selfupdate.ChecksumsFile] = append(s.files[tag+"/"+selfupdate.ChecksumsFile],
		[]byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))...)
}

// Replace swaps a release file for data, leaving checksums.txt as it is.
func (s *Server) Replace(tag, name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[tag+"/"+name] = data
}

// Hits are the requests served so far, "METHOD PATH".
func (s *Server) Hits() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

// Archive is a .tar.gz of files, each at the root, the executables (any
// named caboose or caboose-vmm) mode 0755.
func Archive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, data := range files {
		mode := int64(0o644)
		if name == "caboose" || name == selfupdate.VMM {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// AppZip is a zip of a Caboose.app, as a release's app asset holds it
// (ditto -c -k --keepParent): its Info.plist for id and tag, an
// executable, and Contents/SIGNER, which a test's codesign reads as the
// signature ("IDENTIFIER TEAM", TEAM "-" when ad hoc).
func AppZip(t *testing.T, id, tag, signer string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range []struct {
		name string
		mode os.FileMode
		data []byte
	}{
		{"Caboose.app/Contents/Info.plist", 0o644, appbundle.Plist(appbundle.Info{ID: id, Version: tag})},
		{"Caboose.app/Contents/MacOS/caboose", 0o755, Binary(tag)},
		{"Caboose.app/Contents/SIGNER", 0o644, []byte(signer + "\n")},
	} {
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		h.SetMode(f.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Unzip is ditto -x -k for the tests: the zip's files, with their modes.
func Unzip(_ context.Context, zipPath, dir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return fmt.Errorf("%s escapes", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(in)
		in.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, b, f.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// Codesign is codesign for the tests: a bundle's signature is its
// Contents/SIGNER (AppZip's), and one with Contents/BROKEN does not
// verify.
func Codesign(_ context.Context, args ...string) (string, error) {
	app := args[len(args)-1]
	if args[0] == "--verify" {
		if _, err := os.Stat(filepath.Join(app, "Contents", "BROKEN")); err == nil {
			return app + ": a sealed resource is missing or invalid", errors.New("exit status 1")
		}
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(app, "Contents", "SIGNER"))
	if err != nil {
		return app + ": code object is not signed at all", errors.New("exit status 1")
	}
	id, team, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	if team == "-" {
		team = "not set"
	}
	return fmt.Sprintf("Executable=%s/Contents/MacOS/caboose\nIdentifier=%s\nFormat=app bundle with Mach-O thin (arm64)\nTeamIdentifier=%s\n", app, id, team), nil
}
