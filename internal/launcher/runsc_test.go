package launcher

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Releases for the tests, tar.bz2 as gVisor ships them (Go writes no
// bzip2): good holds runsc, the shim and gvisor-bin/; escape has a
// ../evil; symlink's runsc is a link to /etc/passwd; norunsc has only
// gvisor-bin/.
const (
	releaseGood    = "QlpoOTFBWSZTWdbta6MAAOXfgMuBQAL/gCdGkgj+458giKgwANm2DSJqehMQyZMEZpPTRNGT1GBqaak8kYIeoYAmmmCYRpgipCep6hkTaNEaNGJgBNHlLsXmyndXkC/BIM8ec4pwwoqRzSss7JOTQMgmjNG9CZJJgRSAJAdWBq4+nV9QkqyTVMIPmukVqAQJCQFrMvpCYkGRMJnY50m+mEDkAtQU4riDgeFLv/MSEsOsw7mIXNBYE0sU+/J0BX/U0KA0fSfUH6coBa21nTCEkoCim7fnsK3+iUVBUl6lIGdtSNwTkvZQP8XckU4UJDW7WujA"
	releaseEscape  = "QlpoOTFBWSZTWTtI5lwAAI37gMmAAAJAAe+AAChqJR9ACAggAHQSkmQ0BoMgPUyCSkyDQ0GIAD7bS4GoA95oqSQjLFrjN1oUl48sEIYAwIEBoI0YCEZNEJDiOxUX4yztfYMQUXmazp8LRw87mUfkIgfi7kinChIHaRzLgA=="
	releaseSymlink = "QlpoOTFBWSZTWfTVT5AAAHH7gMiAABBAAPUAAgguAV6AAAggAFQ0gg0ZMmMnqCSRGj1NGnqBofREhIHwQhG3dsxylECGBizzFNhESRUQ+l6OIuDKzzguZS+PQKmE0RANi7kinChIemqnyAA="
	releaseNoRunsc = "QlpoOTFBWSZTWe+rcQoAAIp7gMmAAADAAvaABgBwoZ9ACAggAHUJRR6jQAYmQGgkpMmhoaAA0yUndG9OSAdWkhDY+Dy0IRYPLBCGAMTzs0YtUQsFBB8KwGUVtX1ArsiA4TwiRpyUJEyRMaUHEyhSERIPxdyRThQkO+rcQoA="
)

// runscServer serves a fake gVisor release for arch: the tarball release
// (base64), and sum (its own sha512 when "") beside it.
func runscServer(t *testing.T, arch, release, sum string) *httptest.Server {
	t.Helper()
	tarball, err := base64.StdEncoding.DecodeString(release)
	if err != nil {
		t.Fatal(err)
	}
	if sum == "" {
		h := sha512.Sum512(tarball)
		sum = hex.EncodeToString(h[:]) + "  gvisor.tar.bz2\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + arch + "/gvisor.tar.bz2":
			w.Write(tarball)
		case "/" + arch + "/gvisor.tar.bz2.sha512":
			w.Write([]byte(sum))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadRunsc(t *testing.T) {
	srv := runscServer(t, "aarch64", releaseGood, "")
	parent := filepath.Join(t.TempDir(), "runsc")
	dir := filepath.Join(parent, "aarch64")
	path, err := downloadRunsc(context.Background(), srv.Client(), srv.URL, "aarch64", dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "runsc") {
		t.Errorf("path = %s", path)
	}
	rec, ok := readRunscRelease(dir)
	if !ok || rec.SHA512 != releaseSum(t, releaseGood) || rec.URL != srv.URL+"/aarch64/gvisor.tar.bz2" || time.Since(rec.Downloaded) > time.Minute {
		t.Errorf("record = %+v, %v", rec, ok)
	}
	for name, want := range map[string]string{"runsc": "\x7fELF runsc", "gvisor-bin/gvisor_sentry": "sentry", "containerd-shim-runsc-v1": "shim"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, name)); fi.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %v", name, fi.Mode())
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "gvisor-bin/README")); fi.Mode().Perm() != 0o644 {
		t.Errorf("README mode = %v", fi.Mode())
	}
	// Downloaded again, the new release replaces the old, which leaves
	// nothing behind: not a stray file of its own, not the tarball.
	if err := os.WriteFile(filepath.Join(dir, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadRunsc(context.Background(), srv.Client(), srv.URL, "aarch64", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stray")); err == nil {
		t.Error("the old release is still there")
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

// A release that is not what the checksum says, or holds anything but
// plain files and directories under it, or no runsc, is never placed, and
// one already there stays as it was.
func TestDownloadRunscRefuses(t *testing.T) {
	other := sha512.Sum512([]byte("something else"))
	for name, tc := range map[string]struct {
		arch, release, sum, want string
	}{
		"mismatch":     {"x86_64", releaseGood, hex.EncodeToString(other[:]) + "  gvisor.tar.bz2\n", "checksum mismatch"},
		"not a sha512": {"x86_64", releaseGood, "abc  gvisor.tar.bz2\n", "not a sha512"},
		"empty":        {"x86_64", releaseGood, "\n", "empty"},
		"missing":      {"aarch64", releaseGood, "", "404"},
		"escape":       {"x86_64", releaseEscape, "", `refusing "../evil"`},
		"symlink":      {"x86_64", releaseSymlink, "", `refusing "runsc": not a plain file`},
		"no runsc":     {"x86_64", releaseNoRunsc, "", "no runsc in it"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := runscServer(t, "x86_64", tc.release, tc.sum)
			parent := t.TempDir()
			dir := filepath.Join(parent, "x86_64")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "runsc"), []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := downloadRunsc(context.Background(), srv.Client(), srv.URL, tc.arch, dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "runsc")); string(b) != "old" {
				t.Errorf("runsc = %q", b)
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 1 {
				t.Errorf("left behind: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
				t.Error("wrote outside")
			}
		})
	}
}

func TestRunscArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64": "x86_64", "amd64": "x86_64", "aarch64": "aarch64", "arm64": "aarch64", "riscv64": ""} {
		if got := runscArch(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// The runtime is added, or replaced, and everything else stays as it was,
// in its order; the same entry again is no change at all.
func TestWithRuntime(t *testing.T) {
	rt := runscEntry("/Users/me/.caboose/runsc/aarch64/runsc")
	entry := `"runsc": {
      "path": "/Users/me/.caboose/runsc/aarch64/runsc",
      "runtimeArgs": [
        "--host-uds=open",
        "--net-raw",
        "--allow-packet-socket-write",
        "--dcache=0"
      ]
    }`
	for name, tc := range map[string]struct{ in, want string }{
		"no file": {"", "{\n  \"runtimes\": {\n    " + entry + "\n  }\n}\n"},
		"others kept, in order": {
			`{"zeta": 1, "features": {"buildkit": true}, "alpha": [1,2]}`,
			"{\n  \"zeta\": 1,\n  \"features\": {\n    \"buildkit\": true\n  },\n  \"alpha\": [\n    1,\n    2\n  ],\n  \"runtimes\": {\n    " + entry + "\n  }\n}\n",
		},
		"other runtimes kept": {
			`{"runtimes": {"kata": {"path": "/k"}}, "debug": false}`,
			"{\n  \"runtimes\": {\n    \"kata\": {\n      \"path\": \"/k\"\n    },\n    " + entry + "\n  },\n  \"debug\": false\n}\n",
		},
		"replaced": {
			`{"runtimes": {"runsc": {"path": "/old/runsc"}}}`,
			"{\n  \"runtimes\": {\n    " + entry + "\n  }\n}\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, changed, err := withRuntime([]byte(tc.in), "runsc", rt)
			if err != nil || !changed || string(out) != tc.want {
				t.Fatalf("changed %v, err %v:\n%s\nwant:\n%s", changed, err, out, tc.want)
			}
			again, changed, err := withRuntime(out, "runsc", rt)
			if err != nil || changed || string(again) != string(out) {
				t.Errorf("again: changed %v, err %v:\n%s", changed, err, again)
			}
		})
	}
}

func TestWithRuntimeRefuses(t *testing.T) {
	for in, want := range map[string]string{
		`[]`:                   "not a JSON object",
		`{"a": 1} {}`:          "more after",
		`{"a": 1, "a": 2}`:     `"a" is there twice`,
		`{"runtimes": "runc"}`: "runtimes: not a JSON object",
		`{"a": `:               "EOF",
	} {
		if _, _, err := withRuntime([]byte(in), "runsc", runscEntry("/r")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", in, err, want)
		}
	}
}
