package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
// builtOn is set when the image is stale because it was built on another
// kind of base, or another base, than [image] base now asks for: a
// phrase naming the base it was built on ("'node:20'", "the embedded
// Dockerfile's base"). That is a change to the configuration, which the
// user made, rather than drift nobody asked for, and a launch that creates
// the container rebuilds for it (see ensureImage).
//
// keep is set on an image that is stale for a reason a rebuild would not
// cure (the environment's image dir cannot be read): no launch rebuilds it.
type imageStatus struct {
	state   imageState
	reason  string
	builtOn string
	keep    bool
}

// switched reports whether the image is stale for a changed base.
func (st imageStatus) switched() bool { return st.builtOn != "" }

// classifyImage places an image's labels, as docker.ImageLabels read them,
// against this launcher and the base in use. Only the hashes and the base
// decide: a `make build` stamps `git describe` output and a release its tag,
// so the same files can carry two versions, and one version (dev) many
// contexts.
//
// On the default base, the image is current when both the base's hash and
// the layer's match this launcher's: the base is built from the embedded
// Dockerfile, so its hash is its identity. On an [image] base, the
// embedded Dockerfile plays no part, and what counts is the layer's hash
// and the base itself: its name, and its ID now, baseID, against the one
// the layer was built on -- a pull or rebuild of the base is a new image.
// baseID "" means not known (not asked, or the base is not local), and
// skips that comparison.
//
// Every label is read as unset when empty: the layer sets them all, to ""
// where one does not apply, since a base built FROM a caboose image passes
// its labels on (assets.LayerLabels). An image without the layer's hash or
// its kind of base was not built by caboose build -- one of the user's
// tagged with the environment's name, say -- and is not judged at all, let alone
// rebuilt over. Base names are compared as docker resolves them
// (config.SameImage), so alpine and docker.io/library/alpine:latest are one
// base.
//
// Last, the agent user: an image labelled with other host IDs than this
// process's was built for someone else, and its files in the bind mounts
// would not be theirs.
func (a *App) classifyImage(labels map[string]string, exists bool, baseID string) imageStatus {
	base, byo := a.Cfg.Base()
	dir := a.Cfg.ImageDir
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
	// The kind of base the configuration asks for.
	want := assets.BaseKindDefault
	switch {
	case byo:
		want = assets.BaseKindBYO
	case dir != "":
		want = assets.BaseKindEnv
	}
	// What the image was built on, as the notes name it.
	was := map[string]string{
		assets.BaseKindDefault: defaultBasePhrase,
		assets.BaseKindEnv:     envDirPhrase,
		assets.BaseKindBYO:     "'" + builtOn + "'",
	}[kind]
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	builtUID, builtGID := labels[assets.LabelUID], labels[assets.LabelGID]
	switch {
	case !exists:
		return imageStatus{state: imageMissing}
	case layer == "" || was == "":
		return imageStatus{state: imageUnlabelled}
	case want == assets.BaseKindBYO && (kind != assets.BaseKindBYO || !config.SameImage(builtOn, base)):
		if kind == assets.BaseKindBYO {
			was = "'" + builtOn + "'"
		}
		return switched(was, "on %s, not on the [image] base '%s'", was, base)
	case want == assets.BaseKindDefault && kind != assets.BaseKindDefault:
		if kind == assets.BaseKindBYO {
			return switched(was, "on the [image] base %s, not on the embedded Dockerfile's base", was)
		}
		return switched(was, "on %s, not on the embedded Dockerfile's base", was)
	case want == assets.BaseKindEnv && kind != assets.BaseKindEnv:
		return switched(was, "on %s, not on the environment's %s", was, dir)
	case want == assets.BaseKindEnv:
		// An edit to the dir is the user's change, as a switched base is:
		// the launch that creates the container rebuilds for it.
		now, err := assets.DirHash(dir)
		if err != nil {
			st := stale("and %s cannot be read to compare with (%v)", dir, err)
			st.keep = true
			return st
		}
		if baseHash != now {
			return switched("an earlier state of "+dir, "from %s as it was before an edit (image dir %s, now %s)",
				dir, short(baseHash), short(now))
		}
	case want == assets.BaseKindDefault && baseHash != assets.BaseHash():
		return stale("from a different Dockerfile than this launcher embeds (base context %s, this launcher's %s)",
			short(baseHash), short(assets.BaseHash()))
	}
	switch {
	case layer != assets.LayerHash():
		return stale("with a different layer than this launcher embeds (layer context %s, this launcher's %s)",
			short(layer), short(assets.LayerHash()))
	case byo && baseID != "" && labels[assets.LabelBaseID] != baseID:
		return stale("on '%s' as %s, which it no longer names (now %s)",
			base, shortID(labels[assets.LabelBaseID]), shortID(baseID))
	case builtUID != uid || builtGID != gid:
		return stale("for another host user (uid %s, gid %s; this one is %s:%s)", builtUID, builtGID, uid, gid)
	}
	return imageStatus{state: imageCurrent}
}

// defaultBasePhrase names the embedded Dockerfile's base in a builtOn, and
// envDirPhrase an environment's image/ dir.
const (
	defaultBasePhrase = "the embedded Dockerfile's base"
	envDirPhrase      = "the environment's image dir"
)

// baseNow says what the configuration asks the image to be built on, for a
// note about an image built on another base: the other half of builtOn.
func (a *App) baseNow() string {
	if base, byo := a.Cfg.Base(); byo {
		return fmt.Sprintf("base in [image] now names '%s'", base)
	}
	if a.Cfg.ImageDir != "" {
		return "it is built from " + filepath.Join(a.Cfg.ImageDir, "Dockerfile") + " as it is now"
	}
	return "[image] has no base now, which means " + defaultBasePhrase
}

// imageStatus classifies the image whose labels a launch has just read. On
// an [image] base, an image that is otherwise current costs one more
// inspect, of the base's ID; the default base is identified by its hash
// alone, which the labels already hold, so it costs nothing.
func (a *App) imageStatus(labels map[string]string, exists bool) imageStatus {
	st := a.classifyImage(labels, exists, "")
	if base, byo := a.Cfg.Base(); byo && st.state == imageCurrent {
		st = a.classifyImage(labels, exists, a.images().ImageID(base))
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
// Unless auto_build in [image] is false, the launch that creates the
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
	base, byo := c.Base()
	fmt.Fprintf(out, "context   : %s\n", short(assets.ContextHashFor(byo || c.ImageDir != "")))
	fmt.Fprintf(out, "image     : %s\n", c.Image)
	// The base's ID is asked only of a user's base: it is part of that
	// image's identity, where the default base's is its context hash.
	baseID := ""
	if byo {
		if baseID = a.images().ImageID(base); baseID != "" {
			fmt.Fprintf(out, "base      : %s (base in [image], %s)\n", base, shortID(baseID))
		} else {
			fmt.Fprintf(out, "base      : %s (base in [image], not in the local store)\n", base)
		}
	} else if c.ImageDir != "" {
		h, err := assets.DirHash(c.ImageDir)
		if err != nil {
			h = "unreadable: " + err.Error()
		}
		fmt.Fprintf(out, "base      : %s (built from %s, %s)\n", base, c.ImageDir, short(h))
	} else {
		fmt.Fprintf(out, "base      : %s (the embedded Dockerfile, context %s)\n", base, short(assets.BaseHash()))
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
		a.Note("run 'caboose build' to build it (a first launch does too, unless auto_build in [image] is false).")
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
