package releasetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
)

// MacAppZip is a release's Caboose.app of tag made with a Mac's own tools,
// as release.yml makes it but signed ad hoc: the bundle around a copy of
// /usr/bin/true, signed by codesign as the release identifier, zipped by
// ditto. It skips the test anywhere but on a Mac.
func MacAppZip(t *testing.T, tag string) []byte {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("needs a Mac's codesign and ditto")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "true")
	b, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "out", appbundle.Name)
	if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := appbundle.Assemble(app, exe, appbundle.Info{ID: appbundle.ReleaseID, Version: tag}); err != nil {
		t.Fatal(err)
	}
	zip := filepath.Join(dir, "app.zip")
	for _, argv := range [][]string{
		{"/usr/bin/codesign", "--force", "--identifier", appbundle.ReleaseID, "--sign", "-", app},
		{"/usr/bin/ditto", "-c", "-k", "--keepParent", app, zip},
	} {
		if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", argv, err, out)
		}
	}
	b, err = os.ReadFile(zip)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
