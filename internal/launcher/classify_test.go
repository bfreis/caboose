package launcher

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

// An environment's image/ dir is a base of its own kind: current while
// the dir is as it was built, and switched -- a change the user made,
// which the launch that creates the container rebuilds for -- after an
// edit, or when the image was built on another kind of base.
func TestClassifyImageDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	built, err := assets.DirHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Cfg: &config.Config{Image: "caboose", ImageDir: dir, AutoBuild: true}}
	onDir := labelsFor(assets.BaseKindEnv, built, "caboose-base")

	if st := a.classifyImage(onDir, true, ""); st.state != imageCurrent {
		t.Errorf("as built: %+v", st)
	}
	for _, tc := range []struct {
		name    string
		labels  map[string]string
		builtOn string
		reason  string
	}{
		{"built on the embedded Dockerfile", labelsFor(assets.BaseKindDefault, assets.BaseHash(), "caboose-base"),
			defaultBasePhrase, "on the embedded Dockerfile's base, not on the environment's " + dir},
		{"built on [image] base", labelsFor(assets.BaseKindBYO, "", "node:22"),
			"'node:22'", "on 'node:22', not on the environment's " + dir},
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
		st.reason != "from "+dir+" as it was before an edit (image dir "+short(built)+", now "+short(now)+")" {
		t.Errorf("after an edit: %+v", st)
	}
	if !a.rebuildsAtCreation(st) {
		t.Error("an edit is not rebuilt for at the next creation")
	}

	// The layer still counts.
	old := labelsFor(assets.BaseKindEnv, now, "caboose-base")
	old[assets.LabelLayerHash] = "0123456789abcdef"
	if st := a.classifyImage(old, true, ""); st.state != imageStale || st.switched() || !strings.HasPrefix(st.reason, "with a different layer") {
		t.Errorf("another layer: %+v", st)
	}

	// Unreadable: stale, but nothing to rebuild for by itself.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if st := a.classifyImage(onDir, true, ""); st.state != imageStale || st.switched() || !strings.Contains(st.reason, "cannot be read") {
		t.Errorf("unreadable dir: %+v", st)
	}
}

// The dir gone again: an image built from it is on another base than the
// embedded Dockerfile's, or than the [image] base.
func TestClassifyAwayFromImageDir(t *testing.T) {
	onDir := labelsFor(assets.BaseKindEnv, "abc", "caboose-base")
	a := &App{Cfg: &config.Config{Image: "caboose"}}
	if st := a.classifyImage(onDir, true, ""); !st.switched() || st.builtOn != envDirPhrase ||
		st.reason != "on the environment's image dir, not on the embedded Dockerfile's base" {
		t.Errorf("default: %+v", st)
	}
	a.Cfg.BaseImage = "node:22"
	if st := a.classifyImage(onDir, true, ""); !st.switched() || st.builtOn != envDirPhrase ||
		st.reason != "on the environment's image dir, not on the [image] base 'node:22'" {
		t.Errorf("byo: %+v", st)
	}
}
