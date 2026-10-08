package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// A [packages] proposal changes an apko image profile's own packages: the
// names are checked against what the profile stands for (planPackages),
// the image is built with the new list before anything is written -- so a
// list that does not build changes nothing -- then config.toml is read
// again, and the proposal refused if the profile changed there meanwhile;
// only then is the list written into config.toml, and only once that is
// written, the lock the build resolved. A config.toml that could not be
// written leaves the old lock, and an image newer than config.toml says,
// which the next launch rebuilds to match it.

// packagesPlan is a [packages] proposal as it would be applied: the
// profile's packages before and after.
type packagesPlan struct {
	profile, name string
	add, drop     []string
	old, next     []string
	// defaults is the profile's defaults as planned against.
	defaults bool
	// writeLock writes the lock the build resolved, once config.toml is
	// written; nil when there is none to write.
	writeLock func() error
}

// planPackages works out the packages profile would have with pk applied,
// or why it cannot be: a name added that the profile already stands for,
// a name removed that is not its own. warnings are what to know first.
func planPackages(profile config.ImageProfile, pk proposal.Packages) (pl *packagesPlan, refusals, warnings []string) {
	ours := map[string]bool{}
	for _, p := range apkobuild.Required() {
		ours[p] = true
	}
	if profile.Defaults {
		for _, p := range apkobuild.DefaultPackages() {
			ours[p] = true
		}
	}
	required := apkobuild.Required()
	inGroups := map[string]bool{}
	for _, g := range apkobuild.Groups() {
		for _, p := range g.Packages {
			inGroups[p] = true
		}
	}
	for _, n := range pk.Add {
		switch {
		case slices.Contains(profile.Packages, n):
			refusals = append(refusals, fmt.Sprintf("Already installed: %s (this profile's packages).", n))
		case ours[n]:
			refusals = append(refusals, fmt.Sprintf("Already installed: %s (caboose's packages).", n))
		}
	}
	for _, n := range pk.Remove {
		switch {
		case slices.Contains(profile.Packages, n):
			if ours[n] {
				warnings = append(warnings, fmt.Sprintf("%s stays installed all the same: it is one of caboose's packages too.", n))
			}
		case slices.Contains(required, n):
			refusals = append(refusals, fmt.Sprintf("%s cannot be removed: the sandbox requires it.", n))
		case inGroups[n]:
			refusals = append(refusals, fmt.Sprintf("%s is one of caboose's packages, not this profile's own: to drop it, run 'caboose setup image' and choose package groups.", n))
		default:
			own := "none"
			if len(profile.Packages) > 0 {
				own = strings.Join(profile.Packages, ", ")
			}
			refusals = append(refusals, fmt.Sprintf("%s is not in this profile's packages (%s has: %s).", n, profile, own))
		}
	}
	if len(refusals) > 0 {
		return nil, refusals, warnings
	}
	next := []string{}
	for _, p := range profile.Packages {
		if !slices.Contains(pk.Remove, p) {
			next = append(next, p)
		}
	}
	next = append(next, pk.Add...)
	return &packagesPlan{profile: profile.String(), name: profile.Name, add: pk.Add, drop: pk.Remove, old: profile.Packages, next: next,
		defaults: profile.Defaults}, nil, warnings
}

// packagesRefusal is why a [packages] proposal cannot apply to image, a
// profile of another kind than apko; "" for an apko one.
func packagesRefusal(image config.ImageProfile) string {
	switch image.Kind {
	case config.ImageKindApko:
		return ""
	case config.ImageKindDockerfile:
		return fmt.Sprintf("This environment's image is %s, built from a Dockerfile: packages apply to an apko image only, and under a dockerfile image a [section] is what applies.", image)
	}
	return fmt.Sprintf("This environment's image is %s, an image of the user's own: packages apply to an apko image only, and nothing can be proposed for a ref image.", image)
}

// showPackages shows what applying pl changes.
func (a *App) showPackages(p *prompter, pl *packagesPlan) {
	p.say("The image: the packages of [%s] in %s.", pl.profile, a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)))
	for _, n := range pl.add {
		p.say("  + %s", n)
	}
	for _, n := range pl.drop {
		p.say("  - %s", n)
	}
	p.blank()
}

// applyPackages builds the image with pl's packages, on a copy of the
// configuration, and adds them to edit only once that build succeeded.
// ok is false when it did not: nothing is written, and the proposal stays.
func (a *App) applyPackages(p *prompter, pl *packagesPlan, edit *config.Edit) (ok bool) {
	p.say("Building the image with these packages first: nothing is written unless it builds.")
	orig := a.Cfg
	cp := *orig
	cp.ImageProfile.Packages = slices.Clone(pl.next)
	a.Cfg = &cp
	a.deferLock, a.pendingLock = true, nil
	var err error
	if a.buildImage != nil {
		err = a.buildImage()
	} else {
		err = a.build(nil, a.Stderr)
	}
	a.Cfg = orig
	writeLock := a.pendingLock
	a.deferLock, a.pendingLock = false, nil
	if err != nil {
		p.fail("The build failed (%v): nothing was applied, and the proposal is left pending. "+
			"Delete it, or ask the session to propose a list that builds.", err)
		return false
	}
	p.ok("Built image '%s' with them", a.Cfg.Image)
	if why := a.packagesChanged(pl); why != "" {
		p.fail("%s: nothing was written, and the proposal is left pending. Run caboose apply again to apply it to the profile as it is now.", why)
		return false
	}
	pl.writeLock = writeLock
	if edit.Set == nil {
		edit.Set = map[string]any{}
	}
	edit.Set[pl.profile+".packages"] = pl.next
	edit.Tables = append(edit.Tables, pl.profile)
	return true
}

// packagesChanged is why config.toml no longer holds the profile pl was
// planned against, read again now: "" when its packages and defaults are
// as they were.
func (a *App) packagesChanged(pl *packagesPlan) string {
	path := filepath.Join(a.Cfg.EnvDir, config.FileName)
	data, err := os.ReadFile(path)
	var f *config.File
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Sprintf("%s cannot be read again (%v)", a.short(path), err)
	default:
		if f, err = config.ParseFile(path, data); err != nil {
			return fmt.Sprintf("%s cannot be read again (%v)", a.short(path), err)
		}
	}
	now, err := f.ApkoProfile(pl.name)
	if err != nil {
		return fmt.Sprintf("%s cannot be read again (%v)", a.short(path), err)
	}
	if !slices.Equal(now.Packages, pl.old) || now.Defaults != pl.defaults {
		return fmt.Sprintf("[%s] in %s changed while the image was built", pl.profile, a.short(path))
	}
	return ""
}
