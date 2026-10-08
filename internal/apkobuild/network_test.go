package apkobuild

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func wolfiReachable() bool {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Head(WolfiKey)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func TestWolfiNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the Wolfi test in short mode")
	}
	if !wolfiReachable() {
		t.Skip("packages.wolfi.dev is unreachable")
	}
	ctx := context.Background()
	cache := t.TempDir()
	locks := map[string]*Lock{}
	for _, arch := range []string{"aarch64", "x86_64"} {
		l, err := Resolve(ctx, Spec{}, Options{Arch: arch, CacheDir: cache})
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if l.Arch() != arch || len(l.Packages()) < len(Required()) {
			t.Errorf("%s: arch %q, %d packages", arch, l.Arch(), len(l.Packages()))
		}
		locks[arch] = l
	}

	o := Options{Arch: "aarch64", CacheDir: cache}
	var a, b bytes.Buffer
	da, err := Build(ctx, locks["aarch64"], o, "caboose-apko:test", &a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Build(ctx, locks["aarch64"], o, "caboose-apko:test", &b)
	if err != nil {
		t.Fatal(err)
	}
	if da != db || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("builds differ: %s vs %s", da, db)
	}
	if _, err := Build(ctx, locks["x86_64"], o, "caboose-apko:test", &a); err == nil {
		t.Error("a lock for another architecture was built")
	}

	_, err = Resolve(ctx, Spec{Packages: []string{"no-such-package-xyz"}}, o)
	if err == nil || !strings.Contains(err.Error(), "no-such-package-xyz") {
		t.Errorf("missing package error does not name it: %v", err)
	}
}

func TestWolfiCheckNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the Wolfi test in short mode")
	}
	if !wolfiReachable() {
		t.Skip("packages.wolfi.dev is unreachable")
	}
	ctx := context.Background()
	o := Options{Arch: "aarch64", CacheDir: t.TempDir()}
	r, err := Check(ctx, Spec{Packages: []string{"jq"}}, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("jq on aarch64: %d packages, %d bytes installed", r.Packages, r.InstalledBytes)
	if r.Packages < len(Required()) || r.InstalledBytes <= 0 {
		t.Errorf("result %+v", r)
	}
	_, err = Check(ctx, Spec{Packages: []string{"graphvis"}}, o)
	if err == nil || !strings.Contains(err.Error(), `no package named "graphvis" (did you mean`) || !strings.Contains(err.Error(), "graphviz") {
		t.Errorf("error %v", err)
	}
}
