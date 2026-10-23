package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/saferm"
)

// AppAssetName is a release's menu-bar app for a Mac of goarch: a zip of
// Caboose.app, signed and notarized, its ticket stapled, as release.yml
// names it.
func AppAssetName(tag, goarch string) string {
	return fmt.Sprintf("caboose-app_%s_darwin_%s.zip", strings.TrimPrefix(tag, "v"), goarch)
}

// maxAppZip bounds the app's download, which holds one launcher binary.
const maxAppZip = 256 << 20

// App is a release's Caboose.app where install.sh put it
// (appbundle.InstallPath): an update replaces it with the release's, and
// never puts one where there is none.
type App struct {
	Path string // ~/Applications/Caboose.app
	// Unzip unpacks a zip into a directory as Finder would; Codesign runs
	// codesign and returns what it printed. Nil is ditto's and codesign's.
	Unzip    func(ctx context.Context, zip, dir string) error
	Codesign func(ctx context.Context, args ...string) (string, error)
}

// ErrNoApp is an App with no release bundle at its Path.
var ErrNoApp = errors.New("no Caboose.app of a release's is installed")

// Version is the caboose version of the bundle at Path, which must be a
// release's (appbundle.ReleaseID): ErrNoApp when there is none.
func (a App) Version() (string, error) {
	return bundleVersion(a.Path)
}

// bundleVersion is the CabooseVersion of the release bundle at app.
func bundleVersion(app string) (string, error) {
	plist := filepath.Join(app, "Contents", "Info.plist")
	if fi, err := os.Lstat(plist); errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoApp
	} else if err != nil {
		return "", err
	} else if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", plist)
	}
	b, err := appbundle.ReadPlist(plist)
	if err != nil {
		return "", err
	}
	if id, err := appbundle.ParseIdentifier(b); err != nil {
		return "", fmt.Errorf("%s: %w", app, err)
	} else if id != appbundle.ReleaseID {
		return "", fmt.Errorf("%s is not a release's Caboose.app (its identifier is %q): %w", app, id, ErrNoApp)
	}
	v, err := appbundle.ParseString(b, "CabooseVersion")
	if err != nil {
		return "", fmt.Errorf("%s: %w", app, err)
	}
	return v, nil
}

// Install replaces the bundle at Path with tag's for goarch, when the one
// there is older: the zip checked against the release's checksums,
// unpacked beside Path, and put in its place only once its signature is
// valid, by the same team as the installed one's, for caboose's release
// identifier and of version tag. The old bundle is moved aside first and
// put back if the new one cannot take its place. A running app keeps its
// process until it relaunches into the new bundle. It returns whether it
// replaced it, and ErrNoApp, changing nothing, when no release bundle is
// at Path.
func (a App) Install(ctx context.Context, src Source, tag, goarch string) (bool, error) {
	cur, err := a.Version()
	if err != nil {
		return false, err
	}
	if Valid(cur) && Compare(cur, tag) >= 0 {
		return false, nil
	}
	oldTeam, err := a.team(ctx, a.Path)
	if err != nil {
		return false, fmt.Errorf("the installed Caboose.app: %w", err)
	}
	parent := filepath.Dir(a.Path)
	stage, err := os.MkdirTemp(parent, "."+appbundle.Name+".update-")
	if err != nil {
		return false, err
	}
	defer saferm.RemoveAll(parent, stage)

	name := AppAssetName(tag, goarch)
	zip := filepath.Join(stage, name)
	f, err := os.OpenFile(zip, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	err = src.Download(ctx, tag, name, f, maxAppZip)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	out := filepath.Join(stage, "x")
	if err := os.Mkdir(out, 0o755); err != nil {
		return false, err
	}
	if err := a.unzip(ctx, zip, out); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	fresh := filepath.Join(out, appbundle.Name)
	if fi, err := os.Lstat(fresh); err != nil || !fi.IsDir() {
		return false, fmt.Errorf("%s holds no %s", name, appbundle.Name)
	}
	if v, err := bundleVersion(fresh); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	} else if v != tag {
		return false, fmt.Errorf("%s holds caboose %s, not %s", name, v, tag)
	}
	if out, err := a.codesign(ctx, "--verify", "--deep", "--strict", fresh); err != nil {
		return false, fmt.Errorf("the new Caboose.app's signature is not valid: %v: %s", err, strings.TrimSpace(out))
	}
	id, team, err := a.signer(ctx, fresh)
	if err != nil {
		return false, err
	}
	if id != appbundle.ReleaseID {
		return false, fmt.Errorf("the new Caboose.app is signed as %q, not %s", id, appbundle.ReleaseID)
	}
	if team != oldTeam {
		return false, fmt.Errorf("the new Caboose.app is signed by team %q, not %q as the installed one", team, oldTeam)
	}
	return true, swapApp(parent, stage, fresh, a.Path)
}

// swapApp puts fresh at dst, both in parent: dst moved into stage, fresh
// renamed into its place, and dst put back if that fails. What is left in
// stage goes with it.
func swapApp(parent, stage, fresh, dst string) error {
	old := filepath.Join(stage, "old")
	if err := os.Rename(dst, old); err != nil {
		return err
	}
	if err := os.Rename(fresh, dst); err != nil {
		return errors.Join(err, os.Rename(old, dst))
	}
	return nil
}

// team is the Team ID that signed the bundle at app ("" when ad hoc).
func (a App) team(ctx context.Context, app string) (string, error) {
	_, team, err := a.signer(ctx, app)
	return team, err
}

// signer is the identifier and Team ID codesign -dv gives the bundle at
// app; an ad hoc signature has no Team ID, which is "".
func (a App) signer(ctx context.Context, app string) (id, team string, err error) {
	out, err := a.codesign(ctx, "-dv", "--verbose=2", app)
	if err != nil {
		return "", "", fmt.Errorf("codesign -dv %s: %v: %s", app, err, strings.TrimSpace(out))
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		switch {
		case !ok:
		case k == "Identifier":
			id = v
		case k == "TeamIdentifier" && v != "not set":
			team = v
		}
	}
	if id == "" {
		return "", "", fmt.Errorf("codesign -dv %s names no identifier", app)
	}
	return id, team, nil
}

func (a App) unzip(ctx context.Context, zip, dir string) error {
	if a.Unzip != nil {
		return a.Unzip(ctx, zip, dir)
	}
	if out, err := exec.CommandContext(ctx, "/usr/bin/ditto", "-x", "-k", zip, dir).CombinedOutput(); err != nil {
		return fmt.Errorf("ditto: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// codesign writes what it says to stderr, -dv included.
func (a App) codesign(ctx context.Context, args ...string) (string, error) {
	if a.Codesign != nil {
		return a.Codesign(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", args...).CombinedOutput()
	return string(out), err
}
