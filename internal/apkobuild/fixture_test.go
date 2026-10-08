package apkobuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // apk checksums are sha1
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fixtureRepo is a repository of one tiny unsigned package, served from
// memory through a transport, so resolving and building need no network.
type fixtureRepo struct {
	files map[string][]byte
}

const fixtureURL = "http://fixture.invalid/os"

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarOf makes a tar of regular files, in order; end adds the trailing blocks.
func tarOf(t *testing.T, end bool, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if end {
			for d := ""; ; {
				i := strings.IndexByte(f[0][len(d):], '/')
				if i < 0 {
					break
				}
				d = f[0][:len(d)+i+1]
				if err := tw.WriteHeader(&tar.Header{Name: d, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
					t.Fatal(err)
				}
			}
		}
		h := &tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg}
		if end {
			sum := sha1.Sum([]byte(f[1])) //nolint:gosec
			h.Format = tar.FormatPAX
			h.PAXRecords = map[string]string{"APK-TOOLS.checksum.SHA1": fmt.Sprintf("%x", sum)}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if end {
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
	} else if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newFixture(t *testing.T, arch, ver string) *fixtureRepo {
	t.Helper()
	data := gz(t, tarOf(t, true, [][2]string{{"usr/share/fixture/hello", "hello " + ver + "\n"}}))
	pkginfo := fmt.Sprintf("pkgname = fixture-hello\npkgver = %s\npkgdesc = a fixture\nurl = https://example.invalid\nbuilddate = 0\npackager = test\nsize = 8\narch = %s\norigin = fixture-hello\nlicense = MIT\ndatahash = %x\n", ver, arch, sha256.Sum256(data))
	control := gz(t, tarOf(t, false, [][2]string{{".PKGINFO", pkginfo}}))
	sum := sha1.Sum(control) //nolint:gosec
	index := fmt.Sprintf("C:Q1%s\nP:fixture-hello\nV:%s-r0\nA:%s\nS:%d\nI:8\nT:a fixture\nU:https://example.invalid\nL:MIT\no:fixture-hello\nt:0\n\n",
		base64.StdEncoding.EncodeToString(sum[:]), ver, arch, len(control)+len(data))
	apkIndex := gz(t, tarOf(t, true, [][2]string{{"APKINDEX", index}, {"DESCRIPTION", "fixture"}}))
	return &fixtureRepo{files: map[string][]byte{
		"/os/" + arch + "/APKINDEX.tar.gz":                  apkIndex,
		"/os/" + arch + "/fixture-hello-" + ver + "-r0.apk": append(append([]byte{}, control...), data...),
	}}
}

func (f *fixtureRepo) RoundTrip(r *http.Request) (*http.Response, error) {
	b, ok := f.files[r.URL.Path]
	if !ok {
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")), Request: r, Header: http.Header{}}, nil
	}
	return &http.Response{
		StatusCode: 200, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(b)),
		ContentLength: int64(len(b)), Request: r, Header: http.Header{},
	}, nil
}

func fixtureOptions(f *fixtureRepo, arch string) Options {
	return Options{
		Arch: arch, Repositories: []string{fixtureURL}, Keyring: []string{},
		Transport: f, IgnoreSignatures: true,
	}
}

func TestFixtureResolveAndBuild(t *testing.T) {
	ctx := context.Background()
	repo := newFixture(t, "x86_64", "1.0")
	o := fixtureOptions(repo, "x86_64")
	l, err := resolveList(ctx, Spec{Packages: []string{"fixture-hello"}}, []string{"fixture-hello"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Packages(); len(got) != 1 || got[0] != (PackageVersion{"fixture-hello", "1.0-r0"}) {
		t.Fatalf("packages = %v", got)
	}
	if got := l.Input(); len(got) != 1 || got[0] != "fixture-hello" {
		t.Errorf("input = %v", got)
	}

	var a, b bytes.Buffer
	da, err := Build(ctx, l, o, "fixture:test", &a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Build(ctx, l, o, "fixture:test", &b)
	if err != nil {
		t.Fatal(err)
	}
	if da != db || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("builds differ: %s vs %s", da, db)
	}
	if !strings.HasPrefix(da, "sha256:") {
		t.Errorf("digest = %q", da)
	}
	names := tarNames(t, a.Bytes())
	if !contains(names, "manifest.json") {
		t.Errorf("tarball lacks manifest.json: %v", names)
	}
	var manifest []struct{ RepoTags, Layers []string }
	if err := json.Unmarshal(tarFile(t, a.Bytes(), "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 || len(manifest[0].Layers) == 0 || manifest[0].RepoTags[0] != "fixture:test" {
		t.Errorf("manifest = %+v", manifest)
	}

	// A lock for another architecture is refused.
	if _, err := Build(ctx, l, fixtureOptions(repo, "aarch64"), "fixture:test", io.Discard); err == nil {
		t.Error("built a lock for the wrong architecture")
	}
	if _, err := Build(ctx, l, o, "BAD TAG", io.Discard); err == nil {
		t.Error("built with a bad tag")
	}
}

func TestFixtureMissingPackage(t *testing.T) {
	o := fixtureOptions(newFixture(t, "x86_64", "1.0"), "x86_64")
	_, err := resolveList(context.Background(), Spec{Packages: []string{"nonexistent-pkg"}}, []string{"nonexistent-pkg"}, o)
	if err == nil || !strings.Contains(err.Error(), "nonexistent-pkg") {
		t.Errorf("error does not name the package: %v", err)
	}
}

func TestFixtureNewVersionChangesLock(t *testing.T) {
	ctx := context.Background()
	l1, err := resolveList(ctx, Spec{Packages: []string{"fixture-hello"}}, []string{"fixture-hello"}, fixtureOptions(newFixture(t, "x86_64", "1.0"), "x86_64"))
	if err != nil {
		t.Fatal(err)
	}
	l2, err := resolveList(ctx, Spec{Packages: []string{"fixture-hello"}}, []string{"fixture-hello"}, fixtureOptions(newFixture(t, "x86_64", "1.1"), "x86_64"))
	if err != nil {
		t.Fatal(err)
	}
	if l1.Hash() == l2.Hash() {
		t.Error("hash is the same for two versions")
	}
}

func tarNames(t *testing.T, b []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

func tarFile(t *testing.T, b []byte, name string) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if h.Name == name {
			out, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
