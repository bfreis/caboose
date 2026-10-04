package launcher

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vmAsset is a vm asset holding files, as the release workflow packs one.
func vmAsset(t *testing.T, files map[string]string, links ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(data))
	}
	for _, l := range links {
		tw.WriteHeader(&tar.Header{Name: l, Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func fullAsset() map[string]string {
	return map[string]string{"kernel-arm64": "kernel", "kernel-arm64.config": "config",
		"kernel-arm64.SOURCE": "source", "builder-arm64.img": "builder", "../escape": "no"}
}

// A release build fetches its version's asset, checked against
// checksums.txt, into CABOOSE_HOME/vm/<tag>/<arch>/, and drops the vm
// files of versions no longer installed.
func TestFetchVMFiles(t *testing.T) {
	e := newUpdateEnv(t)
	e.srv.Add("v1.0.0", vmAssetName("v1.0.0", "arm64"), vmAsset(t, fullAsset()))
	stale := filepath.Join(e.a.Cfg.CabooseHome, vmDirName, "v0.9.0", "arm64")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := e.a.releaseTag(); got != "v1.0.0" {
		t.Fatalf("releaseTag %q", got)
	}
	if err := e.a.fetchVMFiles("v1.0.0", "arm64"); err != nil {
		t.Fatal(err)
	}
	dir := e.a.vmReleaseDir("v1.0.0", "arm64")
	for name, want := range fullAsset() {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if name == "../escape" {
			if err == nil {
				t.Error("unpacked a file the asset may not hold")
			}
			continue
		}
		if err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("an uninstalled version's vm files stay: %v", err)
	}
	if !strings.Contains(e.err.String(), "fetching caboose-vm_1.0.0_arm64.tar.gz") {
		t.Errorf("said %q", e.err)
	}
}

// An asset that does not match checksums.txt, or lacks a file, leaves
// nothing behind.
func TestFetchVMFilesRefuses(t *testing.T) {
	e := newUpdateEnv(t)
	name := vmAssetName("v1.0.0", "arm64")
	e.srv.Add("v1.0.0", name, vmAsset(t, fullAsset()))
	e.srv.Replace("v1.0.0", name, vmAsset(t, map[string]string{"kernel-arm64": "evil"}))
	if err := e.a.fetchVMFiles("v1.0.0", "arm64"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a tampered asset: %v", err)
	}
	e = newUpdateEnv(t)
	e.srv.Add("v1.0.0", name, vmAsset(t, map[string]string{"kernel-arm64": "k"}, "builder-arm64.img"))
	if err := e.a.fetchVMFiles("v1.0.0", "arm64"); err == nil || !strings.Contains(err.Error(), "no kernel-arm64.config") {
		t.Fatalf("an incomplete asset: %v", err)
	}
	if _, err := os.Stat(e.a.vmReleaseDir("v1.0.0", "arm64")); !os.IsNotExist(err) {
		t.Errorf("a failed fetch left %v", err)
	}
}
