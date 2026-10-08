package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
)

// labelsFor are the labels a build on a base of kind (with baseHash, and
// named name) would have given the image, by this launcher, for this user.
func labelsFor(kind, baseHash, name string) map[string]string {
	return map[string]string{
		assets.LabelVersion: "v1", assets.LabelLayerHash: assets.LayerHash(),
		assets.LabelBaseKind: kind, assets.LabelBaseHash: baseHash, assets.LabelBaseName: name,
		assets.LabelUID: strconv.Itoa(os.Getuid()), assets.LabelGID: strconv.Itoa(os.Getgid()),
	}
}

// writeLock writes a lock as apkobuild.Resolve would have made it for
// spec, from the package list input, each package at version, for arch,
// and returns it as read back.
func writeLock(t *testing.T, path string, spec apkobuild.Spec, input []string, version, arch string) *apkobuild.Lock {
	t.Helper()
	type pkg struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		Version      string `json:"version"`
		Architecture string `json:"architecture"`
	}
	var pkgs []pkg
	for _, p := range input {
		pkgs = append(pkgs, pkg{p, "https://example.invalid/" + p + ".apk", version, arch})
	}
	b, err := json.Marshal(map[string]any{
		"version":  "v1",
		"config":   map[string]string{"name": apkobuild.ConfigName(spec, input)},
		"contents": map[string]any{"packages": pkgs},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := apkobuild.ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// specList is what profile p's lock is resolved from.
func specList(t *testing.T, p config.ImageProfile) []string {
	t.Helper()
	l, err := p.Spec().List()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// An apko profile's base is its lock: current while the lock is the one
// the image was built from and was resolved for the profile's spec;
// switched -- a change the user made, which the launch that creates the
// container rebuilds for -- when the profile's packages changed, or the
// lock is gone or unusable; plainly stale -- drift that came with the
// launcher -- when the lock is the profile's but caboose's groups now make
// other packages of it, or its hash is not the one the image was built
// from.
func TestClassifyApko(t *testing.T) {
	env := t.TempDir()
	p := config.DefaultImageProfile()
	a := &App{Cfg: &config.Config{Env: "x", Image: "caboose:x", EnvDir: env, ImageProfile: p, AutoBuild: true}}
	lock := writeLock(t, a.Cfg.LockPath(), p.Spec(), specList(t, p), "1", "x86_64")
	built := labelsFor(assets.BaseKindApko, lock.Hash(), "caboose-base:x")

	if st := a.classifyImage(built, true, ""); st.state != imageCurrent {
		t.Errorf("as built: %+v", st)
	}

	// The lock's hash is not the image's (resolved again, or another apko):
	// drift, not the user's change.
	newer := writeLock(t, a.Cfg.LockPath(), p.Spec(), specList(t, p), "2", "x86_64")
	st := a.classifyImage(built, true, "")
	if st.state != imageStale || st.switched() ||
		st.reason != "from another lock than apko.default has now ("+short(lock.Hash())+", now "+short(newer.Hash())+": caboose's packages or apko changed with this launcher)" {
		t.Errorf("lock changed: %+v", st)
	}

	// caboose's groups changed with the launcher: the lock is the profile's,
	// but lists other packages than its spec stands for now.
	writeLock(t, a.Cfg.LockPath(), p.Spec(), append(specList(t, p), "retired-package"), "1", "x86_64")
	if st := a.classifyImage(built, true, ""); st.state != imageStale || st.switched() ||
		!strings.Contains(st.reason, "(caboose's packages changed with this launcher)") {
		t.Errorf("groups changed: %+v", st)
	}

	// The profile's packages changed: the user's change.
	writeLock(t, a.Cfg.LockPath(), p.Spec(), specList(t, p), "1", "x86_64")
	a.Cfg.ImageProfile.Packages = []string{"postgresql-17-client"}
	if st := a.classifyImage(built, true, ""); st.state != imageStale || !st.switched() ||
		!strings.HasSuffix(st.reason, "(its packages changed)") || st.builtOn != "earlier packages" {
		t.Errorf("packages changed: %+v", st)
	}
	// So is one that asks for the same packages (one the defaults have).
	a.Cfg.ImageProfile.Packages = []string{apkobuild.DefaultPackages()[0]}
	if st := a.classifyImage(built, true, ""); st.state != imageStale || !st.switched() ||
		!strings.HasSuffix(st.reason, "(its packages changed)") {
		t.Errorf("spec changed, same packages: %+v", st)
	}
	a.Cfg.ImageProfile.Packages = nil
	if !a.rebuildsAtCreation(st) {
		t.Error("a stale lock is not rebuilt for at the next creation")
	}

	// No lock at all.
	if err := os.Remove(a.Cfg.LockPath()); err != nil {
		t.Fatal(err)
	}
	if st := a.classifyImage(built, true, ""); st.state != imageStale || !st.switched() ||
		!strings.Contains(st.reason, "has no lock now") || !strings.HasSuffix(st.reason, "(its packages changed)") {
		t.Errorf("no lock: %+v", st)
	}

	// An unusable one.
	if err := os.WriteFile(a.Cfg.LockPath(), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := a.classifyImage(built, true, ""); st.state != imageStale || !st.switched() || !strings.Contains(st.reason, "lock cannot be used") {
		t.Errorf("bad lock: %+v", st)
	}

	// The layer still counts.
	writeLock(t, a.Cfg.LockPath(), p.Spec(), specList(t, p), "1", "x86_64")
	old := labelsFor(assets.BaseKindApko, lock.Hash(), "caboose-base:x")
	old[assets.LabelLayerHash] = "0123456789abcdef"
	if st := a.classifyImage(old, true, ""); st.state != imageStale || st.switched() || !strings.HasPrefix(st.reason, "with a different layer") {
		t.Errorf("another layer: %+v", st)
	}
}

// A dockerfile profile's dir is its base: current while the dir is as it
// was built, and switched after an edit, or when the image was built on
// another kind of base.
func TestClassifyDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	built, err := assets.DirHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := config.ImageProfile{Kind: config.ImageKindDockerfile, Name: "default", Dir: dir}
	a := &App{Cfg: &config.Config{Image: "caboose", ImageProfile: p, AutoBuild: true}}
	onDir := labelsFor(assets.BaseKindDockerfile, built, "caboose-base")

	if st := a.classifyImage(onDir, true, ""); st.state != imageCurrent {
		t.Errorf("as built: %+v", st)
	}
	want := filepath.Join(dir, "Dockerfile") + " (dockerfile.default)"
	for _, tc := range []struct {
		name    string
		labels  map[string]string
		builtOn string
		reason  string
	}{
		{"built with apko", labelsFor(assets.BaseKindApko, "abc", "caboose-base"),
			apkoPhrase, "on packages built with apko, not on " + want},
		{"built on a ref", labelsFor(assets.BaseKindRef, "", "node:22"),
			"'node:22'", "on 'node:22', not on " + want},
	} {
		st := a.classifyImage(tc.labels, true, "")
		if st.state != imageStale || st.builtOn != tc.builtOn || st.reason != tc.reason || !st.switched() {
			t.Errorf("%s: %+v", tc.name, st)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now, _ := assets.DirHash(dir)
	st := a.classifyImage(onDir, true, "")
	if st.state != imageStale || !st.switched() || st.builtOn != "an earlier state of "+dir ||
		st.reason != "from "+dir+" as it was before an edit (build context "+short(built)+", now "+short(now)+")" {
		t.Errorf("after an edit: %+v", st)
	}
	if !a.rebuildsAtCreation(st) {
		t.Error("an edit is not rebuilt for at the next creation")
	}

	// Unreadable: stale, but nothing to rebuild for by itself.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if st := a.classifyImage(onDir, true, ""); st.state != imageStale || st.switched() || !strings.Contains(st.reason, "cannot be read") {
		t.Errorf("unreadable dir: %+v", st)
	}
}

// A ref is judged by its name and the ID it named when the layer was
// built; another kind of base is a switch; an image of no kind this
// caboose knows is not judged.
func TestClassifyRef(t *testing.T) {
	p := config.ImageProfile{Kind: config.ImageKindRef, Name: "mine", Ref: "node:22"}
	a := &App{Cfg: &config.Config{Image: "caboose", ImageProfile: p, AutoBuild: true}}
	onRef := labelsFor(assets.BaseKindRef, "", "docker.io/library/node:22")
	onRef[assets.LabelBaseID] = "sha256:aaa"
	if st := a.classifyImage(onRef, true, "sha256:aaa"); st.state != imageCurrent {
		t.Errorf("as built: %+v", st)
	}
	if st := a.classifyImage(onRef, true, "sha256:bbb"); st.state != imageStale || st.switched() ||
		!strings.Contains(st.reason, "which it no longer names") {
		t.Errorf("pulled since: %+v", st)
	}
	a.Cfg.ImageProfile.Ref = "node:24"
	if st := a.classifyImage(onRef, true, ""); !st.switched() || st.builtOn != "'docker.io/library/node:22'" ||
		st.reason != "on 'docker.io/library/node:22', not on 'node:24' (ref.mine)" {
		t.Errorf("another ref: %+v", st)
	}
	if st := a.classifyImage(labelsFor(assets.BaseKindDockerfile, "abc", "caboose-base"), true, ""); !st.switched() ||
		st.builtOn != dockerfilePhrase || st.reason != "on a Dockerfile, not on 'node:24' (ref.mine)" {
		t.Errorf("from a Dockerfile: %+v", st)
	}
	if st := a.classifyImage(labelsFor("default", "abc", "caboose-base"), true, ""); !st.switched() ||
		st.reason != "on a base of kind 'default', not on 'node:24' (ref.mine)" {
		t.Errorf("a kind this caboose does not build: %+v", st)
	}
}
