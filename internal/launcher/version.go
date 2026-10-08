package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/version"
)

// imageState is how the local image relates to this launcher.
type imageState int

const (
	imageMissing    imageState = iota // not in the local store
	imageCurrent                      // built from this launcher's contexts, on the base in use
	imageStale                        // built from something else; see the reason
	imageUnlabelled                   // not built by caboose build: none of its labels
)

// imageStatus is an image's imageState, and when stale, why: a phrase that
// follows "built by VERSION, ", such as "for another host user".
//
// builtOn is set when the image is stale because it was built from
// something other than what the image profile asks for now: another kind
// of base, another ref, a Dockerfile dir since edited, an apko profile's
// packages or lock since changed. It is a phrase naming what it was built
// on ("'node:20'", "a Dockerfile"). That is a change the user made, rather
// than drift nobody asked for, and a launch that creates the container
// rebuilds for it (see ensureImage).
//
// keep is set on an image that is stale for a reason a rebuild would not
// cure (a dockerfile profile's dir cannot be read): no launch rebuilds it.
type imageStatus struct {
	state   imageState
	reason  string
	builtOn string
	keep    bool
}

// switched reports whether the image is stale for a changed base.
func (st imageStatus) switched() bool { return st.builtOn != "" }

// classifyImage places an image's labels, as docker.ImageLabels read them,
// against this launcher and the image profile in use. Only the hashes and
// the base decide: a `make build` stamps `git describe` output and a
// release its tag, so the same files can carry two versions, and one
// version (dev) many contexts.
//
// The base is judged by its kind (assets.LabelBaseKind, the image kind it
// was built from), which must be the profile's, and then by what
// identifies it: on apko, the hash of the lock it was built from, against
// the lock next to config.toml, whose packages must still be the
// profile's; on dockerfile, the dir's DirHash as built against the dir
// now; on a ref, its name, and its ID now, baseID, against the one the
// layer was built on -- a pull or rebuild of the base is a new image.
// baseID "" means not known (not asked, or the base is not local), and
// skips that comparison. Then the layer's hash, whatever the kind.
//
// Every label is read as unset when empty: the layer sets them all, to ""
// where one does not apply, since a base built FROM a caboose image passes
// its labels on (assets.LayerLabels). An image without the layer's hash or
// a kind of base this caboose knows was not built by this caboose's build
// -- one of the user's tagged with the environment's name, say, or by a
// caboose of another time -- and is not judged at all, let alone rebuilt
// over. Base names are compared as docker resolves them
// (config.SameImage), so alpine and docker.io/library/alpine:latest are one
// base.
//
// Last, the agent user: an image labelled with other host IDs than this
// process's was built for someone else, and its files in the bind mounts
// would not be theirs.
func (a *App) classifyImage(labels map[string]string, exists bool, baseID string) imageStatus {
	p := a.Cfg.ImageProfile
	stale := func(format string, args ...any) imageStatus {
		return imageStatus{state: imageStale, reason: fmt.Sprintf(format, args...)}
	}
	switched := func(builtOn, format string, args ...any) imageStatus {
		st := stale(format, args...)
		st.builtOn = builtOn
		return st
	}
	layer := labels[assets.LabelLayerHash]
	builtOn := labels[assets.LabelBaseName]
	baseHash := labels[assets.LabelBaseHash]
	kind := labels[assets.LabelBaseKind]
	// What the image was built on, as the notes name it.
	was := map[string]string{
		assets.BaseKindApko:       apkoPhrase,
		assets.BaseKindDockerfile: dockerfilePhrase,
		assets.BaseKindRef:        "'" + builtOn + "'",
	}[kind]
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	builtUID, builtGID := labels[assets.LabelUID], labels[assets.LabelGID]
	switch {
	case !exists:
		return imageStatus{state: imageMissing}
	case layer == "" || kind == "":
		return imageStatus{state: imageUnlabelled}
	case was == "":
		// A kind this caboose does not build is a switch like any other.
		was = "a base of kind '" + kind + "'"
		return switched(was, "on %s, not on %s", was, a.profilePhrase())
	case kind != p.Kind:
		return switched(was, "on %s, not on %s", was, a.profilePhrase())
	case p.Kind == config.ImageKindRef && !config.SameImage(builtOn, p.Ref):
		return switched(was, "on %s, not on %s", was, a.profilePhrase())
	case p.Kind == config.ImageKindDockerfile:
		// An edit to the dir is the user's change, as a switched base is:
		// the launch that creates the container rebuilds for it.
		now, err := assets.DirHash(p.Dir)
		if err != nil {
			st := stale("and %s cannot be read to compare with (%v)", a.short(p.Dir), err)
			st.keep = true
			return st
		}
		if baseHash != now {
			return switched("an earlier state of "+a.short(p.Dir), "from %s as it was before an edit (build context %s, now %s)",
				a.short(p.Dir), short(baseHash), short(now))
		}
	case p.Kind == config.ImageKindApko:
		// So is a change to the packages, or to the lock: the next build
		// resolves them again, or builds the lock as it is now.
		if st, ok := a.classifyLock(baseHash); !ok {
			return st
		}
	}
	switch {
	case layer != assets.LayerHash():
		return stale("with a different layer than this launcher embeds (layer context %s, this launcher's %s)",
			short(layer), short(assets.LayerHash()))
	case p.Kind == config.ImageKindRef && baseID != "" && labels[assets.LabelBaseID] != baseID:
		return stale("on '%s' as %s, which it no longer names (now %s)",
			p.Ref, shortID(labels[assets.LabelBaseID]), shortID(baseID))
	case builtUID != uid || builtGID != gid:
		return stale("for another host user (uid %s, gid %s; this one is %s:%s)", builtUID, builtGID, uid, gid)
	}
	return imageStatus{state: imageCurrent}
}

// classifyLock judges an image built from an apko profile, whose layer
// says it was built from the lock with hash built, against the profile's
// lock now: false, with the image's status, when the two differ. A lock
// that is gone, or was resolved for another spec than the profile's (its
// own packages and defaults), is the user's change, as a switched base is.
// A lock of the profile's spec that stands for other packages than the
// spec does now, or that has another hash than the image was built from,
// is drift nobody asked for: caboose's groups, or apko, changed with the
// launcher. That is plainly stale.
func (a *App) classifyLock(built string) (imageStatus, bool) {
	p := a.Cfg.ImageProfile
	switched := func(builtOn, format string, args ...any) (imageStatus, bool) {
		return imageStatus{state: imageStale, reason: fmt.Sprintf(format, args...), builtOn: builtOn}, false
	}
	stale := func(format string, args ...any) (imageStatus, bool) {
		return imageStatus{state: imageStale, reason: fmt.Sprintf(format, args...)}, false
	}
	path := a.Cfg.LockPath()
	if path == "" {
		return switched("earlier packages", "and %s has no lock to compare with (its packages changed)", p)
	}
	lock, err := apkobuild.ReadLock(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return switched("earlier packages", "and %s has no lock now: its packages are resolved again (its packages changed)", p)
	case err != nil:
		return switched("earlier packages", "and %s's lock cannot be used (%v)", p, err)
	}
	if !lock.Spec().Equal(p.Spec()) {
		return switched("earlier packages", "from other packages than %s lists now (its packages changed)", p)
	}
	want, err := p.Spec().List()
	if err != nil {
		return switched("earlier packages", "and %s's packages cannot be listed (%v)", p, err)
	}
	if !slices.Equal(lock.Input(), want) {
		return stale("from caboose's packages as an earlier launcher grouped them (caboose's packages changed with this launcher): the next build resolves %s again", p)
	}
	if now := lock.Hash(); now != built {
		return stale("from another lock than %s has now (%s, now %s: caboose's packages or apko changed with this launcher)", p, short(built), short(now))
	}
	return imageStatus{}, true
}

// apkoPhrase names a base built with apko in a builtOn, and
// dockerfilePhrase one built from a Dockerfile.
const (
	apkoPhrase       = "packages built with apko"
	dockerfilePhrase = "a Dockerfile"
)

// profilePhrase names what the image profile builds on, for a note about
// an image built on something else.
func (a *App) profilePhrase() string {
	p := a.Cfg.ImageProfile
	switch p.Kind {
	case config.ImageKindRef:
		return fmt.Sprintf("'%s' (%s)", p.Ref, p)
	case config.ImageKindDockerfile:
		return fmt.Sprintf("%s (%s)", a.short(filepath.Join(p.Dir, "Dockerfile")), p)
	}
	return fmt.Sprintf("%s (%s)", a.packagesPhrase(), p)
}

// baseNow says what the configuration asks the image to be built on, for a
// note about an image built on another base: the other half of builtOn.
func (a *App) baseNow() string {
	p := a.Cfg.ImageProfile
	switch p.Kind {
	case config.ImageKindRef:
		return fmt.Sprintf("image is %s now, which builds on '%s'", p, p.Ref)
	case config.ImageKindDockerfile:
		return fmt.Sprintf("image is %s now, built from %s as it is now", p, a.short(filepath.Join(p.Dir, "Dockerfile")))
	}
	return fmt.Sprintf("image is %s now, built from %s as its lock pins them", p, a.packagesPhrase())
}

// imageSummary is the image profile in a line, for caboose status and
// version: "apko.default (caboose's packages + 2 of yours)".
func (a *App) imageSummary() string {
	p := a.Cfg.ImageProfile
	switch p.Kind {
	case config.ImageKindRef:
		return fmt.Sprintf("%s (%s)", p, p.Ref)
	case config.ImageKindDockerfile:
		return fmt.Sprintf("%s (%s)", p, a.short(p.Dir))
	}
	return fmt.Sprintf("%s (%s)", p, a.packagesPhrase())
}

// imageStatus classifies the image whose labels a launch has just read. On
// a ref, an image that is otherwise current costs one more inspect, of the
// base's ID; a base caboose builds is identified by its hash alone, which
// the labels already hold, so it costs nothing.
func (a *App) imageStatus(labels map[string]string, exists bool) imageStatus {
	st := a.classifyImage(labels, exists, "")
	if p := a.Cfg.ImageProfile; p.Kind == config.ImageKindRef && st.state == imageCurrent {
		st = a.classifyImage(labels, exists, a.images().ImageID(p.Ref))
	}
	return st
}

// short cuts a hash to a length that is still unambiguous for a human
// comparing two of them.
func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// warnIfImageStale is warnIfImageDrifted's counterpart one level up: that
// one compares the container with the image, this one the image with the
// launcher and the base. After an upgrade the launcher embeds new files,
// and after a pull the base is a new image, but the image stays whatever the
// last caboose build made, and nothing else would say so. A warning, never a
// rebuild: building takes minutes, and moving the container onto the result
// kills sessions. Quiet for an image caboose build did not make, which is
// not known to differ from anything.
//
// Unless auto_build in [build] is false, the launch that creates the
// container rebuilds a stale image (ensureImage), so what it advises is a
// caboose restart alone.
func (a *App) warnIfImageStale(labels map[string]string, exists bool) {
	a.warnIfStale(labels, a.imageStatus(labels, exists))
}

// warnIfStale is warnIfImageStale for an image already classified as st.
func (a *App) warnIfStale(labels map[string]string, st imageStatus) {
	if st.state != imageStale {
		return
	}
	a.Note("image '%s' is out of date: built by %s, %s.", a.Cfg.Image, or(labels[assets.LabelVersion], "unknown"), st.reason)
	switch {
	case a.rebuildsAtCreation(st) && st.switched():
		a.Note("run 'caboose restart' to rebuild it on the configured base and move onto it (this kills running sessions).")
		return
	case a.rebuildsAtCreation(st):
		a.Note("the next 'caboose restart' rebuilds it and moves onto it (this kills running sessions).")
		return
	}
	a.Note("run 'caboose build', then 'caboose restart' to move onto it (this kills running sessions).")
}

// rebuildsAtCreation reports whether the next launch that creates the
// container will rebuild the image by itself: any stale image a rebuild
// cures, since a launcher that updated itself leaves the image it built
// before stale, unless auto_build = false says a launch builds nothing.
func (a *App) rebuildsAtCreation(st imageStatus) bool {
	return st.state == imageStale && !st.keep && a.Cfg.AutoBuild
}

// Version prints which launcher this is, the base it builds on, and whether
// the local image was built from it, on that base. It needs neither a root nor a container, and creates
// or starts nothing: it is what to run when something seems out of date,
// which is exactly when the rest of the setup may not be in order.
//
// The launcher's lines always print. The exit status is non-zero only when
// docker could not be asked: a missing or stale image is an answer, and the
// notes say what to do about it.
func (a *App) Version() error {
	c, out, v := a.Cfg, a.Stdout, version.Get()
	commit := or(short(v.Commit), "unknown")
	if v.Modified && v.Commit != "" {
		commit += " (modified)"
	}
	fmt.Fprintf(out, "version   : %s\n", v.Version)
	fmt.Fprintf(out, "commit    : %s\n", commit)
	fmt.Fprintf(out, "built     : %s\n", or(v.Date, "unknown"))
	fmt.Fprintf(out, "install   : %s\n", a.installLine())
	fmt.Fprintf(out, "context   : %s\n", short(assets.LayerHash()))
	fmt.Fprintf(out, "image     : %s\n", c.Image)
	// The base's ID is asked only of a ref: it is part of that image's
	// identity, where a base caboose builds is identified by its hash.
	baseID, base, p := "", c.BaseRef(), c.ImageProfile
	switch p.Kind {
	case config.ImageKindRef:
		if baseID = a.images().ImageID(base); baseID != "" {
			fmt.Fprintf(out, "base      : %s (%s, %s)\n", base, p, shortID(baseID))
		} else {
			fmt.Fprintf(out, "base      : %s (%s, not in the local store)\n", base, p)
		}
	case config.ImageKindDockerfile:
		h, err := assets.DirHash(p.Dir)
		if err != nil {
			h = "unreadable: " + err.Error()
		}
		fmt.Fprintf(out, "base      : %s (%s, built from %s, %s)\n", base, p, a.short(p.Dir), short(h))
	default:
		lock := "no lock yet"
		if l, err := apkobuild.ReadLock(c.LockPath()); err == nil {
			lock = fmt.Sprintf("%d packages locked, %s", len(l.Packages()), short(l.Hash()))
		} else if !errors.Is(err, fs.ErrNotExist) {
			lock = "its lock cannot be used"
		}
		fmt.Fprintf(out, "base      : %s (%s: %s, %s)\n", base, p, a.packagesPhrase(), lock)
	}

	// Said, not refused: this is what to run when the setup looks wrong.
	if err := c.CheckImages(); err != nil {
		a.Note("%v", err)
	}

	labels, exists, err := a.images().ImageLabels(c.Image)
	if err != nil && !exists {
		if docker.IsUnreachable(err) {
			fmt.Fprintf(out, "local     : unknown (docker did not answer)\n")
		} else {
			fmt.Fprintf(out, "local     : unknown (docker could not look it up)\n")
		}
		return imageInspectFailed(c.Image, err)
	}
	st := a.classifyImage(labels, exists, baseID)
	switch st.state {
	case imageMissing:
		fmt.Fprintf(out, "local     : not built yet\n")
		a.Note("run 'caboose build' to build it (a first launch does too, unless auto_build in [build] is false).")
	case imageCurrent:
		fmt.Fprintf(out, "local     : matches (built by %s)\n", or(labels[assets.LabelVersion], "unknown"))
	case imageStale:
		fmt.Fprintf(out, "local     : out of date (built by %s, %s)\n", or(labels[assets.LabelVersion], "unknown"), st.reason)
		if a.rebuildsAtCreation(st) {
			if st.switched() {
				a.Note("the next launch that creates the %s rebuilds it on the configured base, so", a.noun())
			} else {
				a.Note("the next launch that creates the %s rebuilds it, so", a.noun())
			}
			a.Note("'caboose restart' moves a running %s onto it (this kills running sessions);", a.noun())
			a.Note("'caboose build' rebuilds it now, without touching the %s.", a.noun())
			break
		}
		a.Note("run 'caboose build' to rebuild it from this launcher,")
		a.Note("then 'caboose restart' to move a running %s onto it (this kills running sessions).", a.noun())
	case imageUnlabelled:
		fmt.Fprintf(out, "local     : unlabelled (not built by caboose build)\n")
		a.Note("run 'caboose build' to build it, replacing what the name holds now.")
	}

	// The same comparison warnIfImageDrifted makes, stated rather than only
	// warned about: a rebuilt image does nothing for a container created
	// from the one before it. Said of the local image, the one the line
	// above judges, so that the two read together: a container on a
	// local image that is out of date is on it, and out of date with it.
	label := a.nounLabel()
	switch state := a.state(); state {
	case "absent":
		fmt.Fprintf(out, "%s %s (absent)\n", label, c.Container)
	default:
		running, current := a.box().Image(), a.images().ImageID(c.Image)
		switch {
		case running == "" || current == "":
			fmt.Fprintf(out, "%s %s (%s)\n", label, c.Container, state)
		case running == current && st.state == imageStale:
			fmt.Fprintf(out, "%s %s (%s, on the local image, so out of date too)\n", label, c.Container, state)
		case running == current:
			fmt.Fprintf(out, "%s %s (%s, on the local image)\n", label, c.Container, state)
		default:
			fmt.Fprintf(out, "%s %s (%s, on an older image than the local one)\n", label, c.Container, state)
			a.Note("run 'caboose restart' to move it onto the local image (this kills running sessions).")
		}
	}
	return nil
}

// installLine says how this caboose was installed and updates, for
// caboose version.
func (a *App) installLine() string {
	in := a.installed()
	switch in.kind {
	case installDev:
		return "a development build; it does not update itself"
	case installOther:
		return in.exe + ", not by install.sh; it does not update itself"
	}
	if a.autoUpdateOff() {
		return a.layout().Versions + " (install.sh); automatic updates are off (CABOOSE_NO_AUTO_UPDATE)"
	}
	return a.layout().Versions + " (install.sh); updates itself"
}
