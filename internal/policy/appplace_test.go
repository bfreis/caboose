package policy

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// bundleOK is a bundle's entries as a disk has them: the bundle, its
// Contents, MacOS and Info.plist, and the executable, each of its own
// identity.
func bundleOK(bundle string) []BundleEntry {
	return []BundleEntry{
		{Path: bundle, ID: FileID{1, 30}, Type: fs.ModeDir, Links: 3},
		{Path: bundle + "/Contents", ID: FileID{1, 31}, Type: fs.ModeDir, Links: 3},
		{Path: bundle + "/Contents/Info.plist", ID: FileID{1, 32}, Links: 1},
		{Path: bundle + "/Contents/MacOS", ID: FileID{1, 33}, Type: fs.ModeDir, Links: 2},
		{Path: bundle + "/Contents/MacOS/caboose", ID: FileID{1, 12}, Links: 1},
	}
}

// withEntry is bundleOK's entries with entry i replaced by e.
func withEntry(bundle string, i int, e func(*BundleEntry)) []BundleEntry {
	es := bundleOK(bundle)
	e(&es[i])
	return es
}

func TestCheckAppPlace(t *testing.T) {
	const exe = "/Users/u/.caboose/app-dev/Caboose.app/Contents/MacOS/caboose"
	ok := AppPlace{Path: exe, Resolved: exe, IDs: []FileID{{1, 10}, {1, 11}, {1, 12}}, Links: 1}
	reach := AppReach{Paths: []string{"/Users/u/dev", "/Users/u/.caboose/envs/default/data/home"}, IDs: []FileID{{1, 20}, {1, 21}}}
	if err := CheckAppPlace(ok, reach); err != nil {
		t.Fatalf("a place no sandbox reaches is refused: %v", err)
	}
	inBundle := ok
	inBundle.Bundle = "/Users/u/.caboose/app-dev/Caboose.app"
	inBundle.BundleEntries = bundleOK(inBundle.Bundle)
	if err := CheckAppPlace(inBundle, AppReach{Paths: append(reach.Paths, "/Users/u/.caboose/app-dev/Caboose.app2", "/Users/u/.caboose/other")}); err != nil {
		t.Fatalf("a bundle with nothing reached inside it is refused: %v", err)
	}
	if err := CheckAppPlace(ok, AppReach{}); err != nil {
		t.Fatalf("with no sandbox at all, the place is refused: %v", err)
	}
	inRoot := "/Users/u/dev/caboose/Caboose.app/Contents/MacOS/caboose"
	const app = "/Users/u/.caboose/app-dev/Caboose.app"
	bundled := func(es []BundleEntry, err error) AppPlace {
		pl := inBundle
		pl.BundleEntries, pl.BundleErr = es, err
		return pl
	}
	cases := []struct {
		name string
		pl   AppPlace
		r    AppReach
		want string
	}{
		{"reach unknown", ok, AppReach{Paths: reach.Paths, Err: errors.New("docker did not answer")}, "cannot be told"},
		{"reach unknown, nothing listed", ok, AppReach{Err: errors.New("x")}, "cannot be told"},
		{"not resolved", AppPlace{Path: exe, ResolveErr: errors.New("no such file")}, reach, "cannot be looked at"},
		{"relative", AppPlace{Path: "Caboose.app/Contents/MacOS/caboose", Resolved: "Caboose.app/Contents/MacOS/caboose", IDs: ok.IDs}, reach, "clean absolute"},
		{"unclean", AppPlace{Path: "/Users/u/dev/../.caboose/x", Resolved: "/Users/u/.caboose/x", IDs: ok.IDs}, reach, "clean absolute"},
		{"in a root", AppPlace{Path: inRoot, Resolved: inRoot, IDs: ok.IDs}, reach, "within /Users/u/dev"},
		{"in a root, other case", AppPlace{Path: "/users/U/Dev/c/caboose", Resolved: "/users/U/Dev/c/caboose", IDs: ok.IDs}, reach, "within /Users/u/dev"},
		{"is a reached path", AppPlace{Path: "/Users/u/dev", Resolved: "/Users/u/dev", IDs: ok.IDs}, reach, "within"},
		{"in a writable part", AppPlace{Path: "/Users/u/.caboose/envs/default/data/home/x/caboose", Resolved: "/Users/u/.caboose/envs/default/data/home/x/caboose", IDs: ok.IDs}, reach, "within"},
		{"resolved into a root", AppPlace{Path: exe, Resolved: inRoot, IDs: ok.IDs}, AppReach{}, "once resolved"},
		{"through a symlink", AppPlace{Path: exe, Resolved: exe, Via: []string{"/Users/u/.caboose/app-dev"}, IDs: ok.IDs}, AppReach{}, "through /Users/u/.caboose/app-dev"},
		{"by identity", AppPlace{Path: exe, Resolved: exe, IDs: []FileID{{1, 10}, {1, 21}}}, reach, "by another name"},
		{"no identity", AppPlace{Path: exe, Resolved: exe}, reach, "no identity"},
		{"a root inside the bundle", AppPlace{Path: exe, Resolved: exe, Bundle: app, BundleEntries: bundleOK(app), IDs: ok.IDs},
			AppReach{Paths: []string{"/Users/u/.caboose/app-dev/Caboose.app/Contents/Resources"}}, "into its bundle"},
		{"the bundle reached itself", AppPlace{Path: exe, Resolved: exe, Bundle: app, BundleEntries: bundleOK(app), IDs: ok.IDs},
			AppReach{Paths: []string{"/users/u/.caboose/app-dev/caboose.app"}}, "a sandbox reaches"},
		{"a bundle that does not hold it", AppPlace{Path: exe, Resolved: exe, Bundle: "/elsewhere/Caboose.app", IDs: ok.IDs}, AppReach{}, "does not hold it"},
		{"a second hard link", AppPlace{Path: exe, Resolved: exe, IDs: ok.IDs, Links: 2}, reach, "2 hard links"},
		{"hard links not told", AppPlace{Path: exe, Resolved: exe, IDs: ok.IDs}, reach, "0 hard links"},
		{"a bundle entry reached by another name", bundled(withEntry(app, 2, func(e *BundleEntry) { e.ID = FileID{1, 20} }), nil), reach, "into its bundle by another name"},
		{"the bundle itself reached by another name", bundled(withEntry(app, 0, func(e *BundleEntry) { e.ID = FileID{1, 21} }), nil), reach, "into its bundle by another name"},
		{"a bundle file hard linked", bundled(withEntry(app, 2, func(e *BundleEntry) { e.Links = 2 }), nil), reach, "a file with 2 hard links"},
		{"a bundle file's links not told", bundled(withEntry(app, 2, func(e *BundleEntry) { e.Links = 0 }), nil), reach, "a file with 0 hard links"},
		{"a symlink in the bundle", bundled(withEntry(app, 2, func(e *BundleEntry) { e.Type = fs.ModeSymlink }), nil), reach, "a symlink"},
		{"a device in the bundle", bundled(withEntry(app, 2, func(e *BundleEntry) { e.Type = fs.ModeDevice }), nil), reach, "neither a directory nor a plain file"},
		{"an entry outside the bundle", bundled(withEntry(app, 2, func(e *BundleEntry) { e.Path = "/Users/u/dev/x" }), nil), reach, "not within it"},
		{"an unclean entry", bundled(withEntry(app, 2, func(e *BundleEntry) { e.Path = app + "/Contents/../x" }), nil), reach, "not within it"},
		{"the bundle not walked", bundled(nil, errors.New("permission denied")), reach, "cannot be told: permission denied"},
		{"the bundle not looked at", bundled(nil, nil), reach, "was not looked at"},
		{"engine socket", ok, AppReach{Paths: reach.Paths, IDs: reach.IDs, Everything: "engine_socket in [container.default] of environment default"}, "everything on this machine through engine_socket in [container.default]"},
		{"everything", ok, AppReach{Paths: []string{"/"}}, "within /"},
		{"relative reach", ok, AppReach{Paths: []string{"dev"}}, "within dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckAppPlace(c.pl, c.r)
			if err == nil {
				t.Fatal("allowed")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refused with %q, want it to say %q", err, c.want)
			}
		})
	}
}

// TestCheckAppPlaceEmptyReachEntry: an empty entry holds nothing.
func TestCheckAppPlaceEmptyReachEntry(t *testing.T) {
	const exe = "/a/b"
	if err := CheckAppPlace(AppPlace{Path: exe, Resolved: exe, IDs: []FileID{{1, 1}}, Links: 1}, AppReach{Paths: []string{""}}); err != nil {
		t.Fatal(err)
	}
}
