// Package releasetest serves fake caboose releases, laid out as GitHub
// serves them, for the tests of whatever downloads one: the updater, and
// install.sh.
package releasetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
