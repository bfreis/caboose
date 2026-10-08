package assets

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	caboose "github.com/bfreis/caboose"
)

// Each Dockerfile COPYs only what its context holds; the base COPYs nothing
// at all, since everything caboose adds belongs in the layer.
func TestContextsHoldEveryCopySource(t *testing.T) {
	for _, tc := range []struct {
		dockerfile string
		context    []ContextFile
	}{{"Dockerfile", BaseContext}, {LayerDockerfile, LayerContext}} {
		df, err := caboose.Files.ReadFile(tc.dockerfile)
		if err != nil {
			t.Fatal(err)
		}
		have := map[string]bool{}
		for _, f := range tc.context {
			have[f.Name] = true
		}
		if !have[tc.dockerfile] {
			t.Errorf("%s is not in its own context", tc.dockerfile)
		}
		sc := bufio.NewScanner(bytes.NewReader(df))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && (fields[0] == "COPY" || fields[0] == "ADD") {
				if tc.dockerfile == "Dockerfile" {
					t.Errorf("the base Dockerfile has %s", sc.Text())
					continue
				}
				// A source named by the build's architecture must be there
				// for every architecture caboose builds for.
				for _, arch := range []string{"amd64", "arm64"} {
					src := strings.ReplaceAll(fields[1], "${TARGETARCH}", arch)
					if !have[src] {
						t.Errorf("%s COPYs %s, which is not in its context", tc.dockerfile, src)
					}
				}
			}
		}
	}
}

func TestWriteContextsWriteOnlyTheirFiles(t *testing.T) {
	for _, tc := range []struct {
		write func(string) error
		want  string
	}{
		{WriteLayerContext, "agent-bin entrypoint.sh layer-user.sh layer.Dockerfile shellrc.bash tmux.conf"},
	} {
		dir := t.TempDir()
		if err := tc.write(dir); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		if got := strings.Join(names, " "); got != tc.want {
			t.Fatalf("context = %q, want %q", got, tc.want)
		}
	}
	dir := t.TempDir()
	if err := WriteLayerContext(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("entrypoint.sh is not executable: %v", fi.Mode())
	}
}

func TestSandboxInstructionsHasPlaceholder(t *testing.T) {
	b, err := SandboxInstructions()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("@@CABOOSE_DIR@@")) {
		t.Error("embedded sandbox/CLAUDE.md lost its @@CABOOSE_DIR@@ placeholder")
	}
}

// fixture is a stand-in for every embedded context file.
func fixture() fstest.MapFS {
	m := fstest.MapFS{}
	for _, f := range append(append([]ContextFile{}, BaseContext...), LayerContext...) {
		m[f.Name] = &fstest.MapFile{Data: []byte("content of " + f.Name + "\n")}
	}
	return m
}

func mustHashOf(t *testing.T, fsys fs.FS, tag string, files []ContextFile) string {
	t.Helper()
	h, err := contextHash(fsys, tag, files)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLayerHashIsStable(t *testing.T) {
	a, b := LayerHash(), LayerHash()
	if a != b || len(a) != 64 {
		t.Errorf("layer hash = %q then %q", a, b)
	}
}

// The expected value is built here byte by byte, independently of
// contextHash's helpers, so an accidental change to the framing (which
// would silently mark every existing image stale) fails this test.
func TestContextHashOfKnownFixture(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("caboose-layer-v1\x00")
	u64 := func(v int) { _ = binary.Write(&buf, binary.BigEndian, uint64(v)) }
	for _, f := range LayerContext {
		content := "content of " + f.Name + "\n"
		u64(len(f.Name))
		buf.WriteString(f.Name)
		_ = binary.Write(&buf, binary.BigEndian, uint32(f.Mode))
		u64(len(content))
		buf.WriteString(content)
	}
	sum := sha256.Sum256(buf.Bytes())
	if got, want := mustHashOf(t, fixture(), tagLayer, LayerContext), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("hash = %s, want %s", got, want)
	}
}

// Staleness rests on this: the embedded Dockerfile, now only a seed for a
// dockerfile profile's dir, is not in the layer's hash, so a change to it
// flags no image.
func TestLayerHashCoversItsOwnFiles(t *testing.T) {
	orig := mustHashOf(t, fixture(), tagLayer, LayerContext)
	for _, f := range append(append([]ContextFile{}, BaseContext...), LayerContext...) {
		m := fixture()
		m[f.Name].Data = append(m[f.Name].Data, '#')
		changed := mustHashOf(t, m, tagLayer, LayerContext) != orig
		if changed != (f.Name != "Dockerfile") {
			t.Errorf("editing %s: layer hash changed %v", f.Name, changed)
		}
	}
}

func TestContextHashChangesWithContent(t *testing.T) {
	base := mustHashOf(t, fixture(), tagLayer, LayerContext)

	// The same bytes, split differently between two adjacent files.
	moved := fixture()
	first, second := LayerContext[0].Name, LayerContext[1].Name
	d := moved[first].Data
	moved[first].Data = d[:len(d)-1]
	moved[second].Data = append([]byte{d[len(d)-1]}, moved[second].Data...)
	if mustHashOf(t, moved, tagLayer, LayerContext) == base {
		t.Error("moving a byte between files did not change the hash")
	}
}

func TestContextHashChangesWithMode(t *testing.T) {
	base := mustHashOf(t, fixture(), tagLayer, LayerContext)
	files := append([]ContextFile{}, LayerContext...)
	files[2].Mode = 0o644
	if mustHashOf(t, fixture(), tagLayer, files) == base {
		t.Error("changing a mode did not change the hash")
	}
}

func TestContextHashMissingFile(t *testing.T) {
	m := fixture()
	delete(m, "entrypoint.sh")
	if _, err := contextHash(m, tagLayer, LayerContext); err == nil || !strings.Contains(err.Error(), "entrypoint.sh") {
		t.Errorf("err = %v, want one naming entrypoint.sh", err)
	}
}
