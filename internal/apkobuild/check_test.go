package apkobuild

import (
	"context"
	"crypto/sha1" //nolint:gosec // apk checksums are sha1
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// indexPkg is one package in an index-only fixture.
type indexPkg struct {
	name, deps, provides string
	installed            int64
}

// newIndexFixture is a repository whose APKINDEX lists pkgs; each package
// file is served too, so that a test can see it is never fetched.
func newIndexFixture(t *testing.T, arch string, pkgs []indexPkg) *fixtureRepo {
	t.Helper()
	var b strings.Builder
	files := map[string][]byte{}
	for _, p := range pkgs {
		sum := sha1.Sum([]byte(p.name)) //nolint:gosec
		fmt.Fprintf(&b, "C:Q1%s\nP:%s\nV:1.0-r0\nA:%s\nS:100\nI:%d\nT:x\nU:https://example.invalid\nL:MIT\no:%s\nt:0\n",
			base64.StdEncoding.EncodeToString(sum[:]), p.name, arch, p.installed, p.name)
		if p.deps != "" {
			fmt.Fprintf(&b, "D:%s\n", p.deps)
		}
		if p.provides != "" {
			fmt.Fprintf(&b, "p:%s\n", p.provides)
		}
		b.WriteString("\n")
		files["/os/"+arch+"/"+p.name+"-1.0-r0.apk"] = []byte("not to be fetched")
	}
	files["/os/"+arch+"/APKINDEX.tar.gz"] = gz(t, tarOf(t, true, [][2]string{{"APKINDEX", b.String()}, {"DESCRIPTION", "fixture"}}))
	return &fixtureRepo{files: files}
}

// recorder notes every path requested through it.
type recorder struct {
	next  http.RoundTripper
	mu    sync.Mutex
	paths []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

var checkPkgs = []indexPkg{
	{name: "app", deps: "libfoo cmd:helper", installed: 1000},
	{name: "libfoo", installed: 200},
	{name: "helper-tools", provides: "cmd:helper=1.0", installed: 30},
	{name: "postgresql-17-client", installed: 5},
	{name: "postgresql-17", installed: 7},
	{name: "nodejs-24", installed: 9},
	{name: "nodejs-20", installed: 9},
}

func TestCheckResolvesFromTheIndexAlone(t *testing.T) {
	rec := &recorder{next: newIndexFixture(t, "x86_64", checkPkgs)}
	o := Options{Arch: "x86_64", Repositories: []string{fixtureURL}, Keyring: []string{}, Transport: rec, IgnoreSignatures: true}
	r, err := checkList(context.Background(), []string{"app"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Packages != 3 || r.InstalledBytes != 1230 {
		t.Errorf("result %+v, want 3 packages, 1230 bytes", r)
	}
	if len(rec.paths) == 0 {
		t.Fatal("nothing fetched")
	}
	for _, p := range rec.paths {
		if p != "/os/x86_64/APKINDEX.tar.gz" {
			t.Errorf("fetched %s: only the index should be", p)
		}
	}
	// A virtual name a package provides counts as known.
	if _, err := checkList(context.Background(), []string{"cmd:helper"}, o); err != nil {
		t.Errorf("a provided name: %v", err)
	}
}

func TestCheckNamesUnknownPackages(t *testing.T) {
	o := Options{Arch: "x86_64", Repositories: []string{fixtureURL}, Keyring: []string{},
		Transport: newIndexFixture(t, "x86_64", checkPkgs), IgnoreSignatures: true}
	_, err := checkList(context.Background(), []string{"app", "postgresql-17-clent", "nodejs-22", "zzz"}, o)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		`no package named "postgresql-17-clent" (did you mean postgresql-17-client?)`,
		`no package named "nodejs-22" (did you mean nodejs-20, nodejs-24?)`,
		`no package named "zzz"` + "\n",
	} {
		if !strings.Contains(err.Error()+"\n", want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestSuggest(t *testing.T) {
	names := []string{"graphviz", "graphviz-dev", "jq", "yq", "python-3.13", "python-3.12", "py3-pip", "ripgrep"}
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"grapviz", []string{"graphviz"}},
		{"python-3.11", []string{"python-3.12", "python-3.13"}},
		{"ripgrpe", []string{"ripgrep"}},
		{"kubectl", nil},
		{"jq", []string{"yq"}},
	} {
		if got := Suggest(tc.in, names); !slices.Equal(got, tc.want) {
			t.Errorf("Suggest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	many := []string{"ab", "ac", "ad", "ae", "af"}
	if got := Suggest("aa", many); len(got) != 3 {
		t.Errorf("Suggest offers %d, want 3: %q", len(got), got)
	}
}

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"", "", 0}, {"a", "", 1}, {"kitten", "sitting", 3}, {"clent", "client", 1}, {"abcdef", "ab", 3}} {
		if got := editDistance(tc.a, tc.b, 2); got != min(tc.want, 3) {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, min(tc.want, 3))
		}
	}
}

// A list the indexes cannot satisfy is a ResolutionError; an index that
// cannot be fetched is not, since it says nothing about the list.
func TestCheckTellsResolutionFromFetchFailures(t *testing.T) {
	o := Options{Arch: "x86_64", Repositories: []string{fixtureURL}, Keyring: []string{},
		Transport: newIndexFixture(t, "x86_64", append(slices.Clone(checkPkgs), indexPkg{name: "broken", deps: "nothere"})), IgnoreSignatures: true}
	var re *ResolutionError
	for _, list := range [][]string{{"zzz"}, {"broken"}} {
		_, err := checkList(context.Background(), list, o)
		if !errors.As(err, &re) {
			t.Errorf("%q: %v (%T), want a ResolutionError", list, err, err)
		}
	}
	// A repository with no index: the fetch fails.
	o.Transport = &fixtureRepo{files: map[string][]byte{}}
	_, err := checkList(context.Background(), []string{"app"}, o)
	if err == nil || errors.As(err, &re) {
		t.Errorf("a fetch failure: %v (%T), want an error that is no ResolutionError", err, err)
	}
}

// A check holds the cache's lock while it uses the cache: a prune then
// removes nothing.
func TestCheckHoldsTheCacheLock(t *testing.T) {
	cache := t.TempDir()
	var pruneErr error
	o := Options{Arch: "x86_64", Repositories: []string{fixtureURL}, Keyring: []string{}, CacheDir: cache,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if pruneErr == nil {
				_, pruneErr = PruneCache(cache, nil, time.Now())
			}
			return newIndexFixture(t, "x86_64", checkPkgs).RoundTrip(r)
		}), IgnoreSignatures: true}
	if _, err := checkList(context.Background(), []string{"app"}, o); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(pruneErr, ErrCacheInUse) {
		t.Errorf("a prune during the check: %v", pruneErr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
