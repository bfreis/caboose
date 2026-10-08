package launcher

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/imagecheck"
)

// Exit statuses of caboose check-image. 1 is the answer "no", as for any
// check; could-not-check gets its own status, as grep's 2 does, so a script
// deciding whether to use an image does not mistake a stopped docker engine
// for a bad image. (caboose version exits 1 for the same docker trouble, but it
// has no "no" answer to keep apart from it.)
const (
	checkUnmet  = 1
	checkFailed = 2
)

// CheckImage is caboose check-image [IMAGE]: whether IMAGE can be the base of
// the sandbox, as a checklist on stdout and the unmet requirements, with why
// each is needed, on stderr. Like caboose version it needs no root and
// creates nothing but a throwaway container, which `docker run --rm` removes.
//
// IMAGE defaults to the base in use: a ref profile's image, else the base
// caboose builds from an apko or dockerfile profile, tagged
// config.BaseImageFor the environment -- the image the layer is built on,
// not the one caboose runs, which has the layer's user in it already. A
// build runs this same check on the base before building the layer on it.
//
// An image that is not in the local store is pulled first, with docker's
// progress on stderr -- but only one that was named, on the command line or
// in a ref profile. A base caboose builds is built locally, and pulling its
// name would fetch whatever a registry happens to hold under it.
func (a *App) CheckImage(args []string) error {
	image, named := a.Cfg.BaseRef(), a.Cfg.ImageProfile.Kind == config.ImageKindRef
	switch {
	case len(args) > 1:
		return Die("caboose check-image takes at most one image (got %q)", strings.Join(args, " "))
	case len(args) == 1 && strings.HasPrefix(args[0], "-"):
		return Die("caboose check-image takes an image, not options (got %q)", args[0])
	case len(args) == 1:
		image, named = args[0], true
	}

	out := newUI(a.Stdout, termWidth(a.Stdout, 100))
	out.banner("caboose check-image", image)
	out.blank()
	check := a.checkImageDocker
	if a.isVM() {
		check = a.checkImageVM
	}
	rep, err := check(out, image, named)
	if err != nil {
		return err
	}

	rows := rep.Checklist()
	// Docker inside the sandbox is a fact of the image's, never a
	// requirement, and only vm's: there the sandbox starts the image's
	// own dockerd. Under docker and gvisor nothing starts one, and the
	// sandbox's docker CLI talks to this machine's engine or to nothing,
	// so a row about the image's dockerd would only mislead.
	if row, ok := rep.DockerInside(); ok && a.isVM() {
		rows = append(rows, row)
	}
	lw := 0
	for _, row := range rows {
		lw = max(lw, len(row.Label))
	}
	for _, row := range rows {
		out.row(checkMarks[row.Level], row.Label, lw, row.Value)
	}
	out.blank()
	problems := rep.Problems()
	if n := len(problems); n == 0 {
		out.ok("Usable: every requirement is met.")
	} else {
		out.fail("Not usable: %d %s unmet.", n, plural(n, "requirement", "requirements"))
	}
	a.checkImageSays(rep, problems)
	if len(problems) > 0 {
		return &ExitError{Code: checkUnmet}
	}
	return nil
}

// checkImageDocker is the check in the docker engine: image, pulled first
// when named and not local, probed in a container of it.
func (a *App) checkImageDocker(out *ui, image string, named bool) (*imagecheck.Report, error) {
	_, exists, err := a.Docker.ImageLabels(image)
	if err != nil && !exists {
		return nil, cannotCheck(imageInspectFailed(image, err))
	}
	if !exists {
		if !named {
			return nil, notBuiltYet(a, image)
		}
		a.Note("image '%s' is not in the local store; pulling it (docker's output follows)", image)
		if err := a.Docker.Stream(a.Stderr, a.Stderr, "pull", image); err != nil {
			return nil, &ExitError{Code: checkFailed, Msg: fmt.Sprintf("cannot pull image '%s' (%v)", image, err)}
		}
	}

	// The probe runs a container of the image, and tries the network from
	// it: seconds, which a terminal is told about.
	sp := checkSpinner(out, "checking "+image+" in a container of it")
	rep, err := imagecheck.Run(a.Docker, image, "", os.Getuid(), os.Getgid())
	sp.end()
	if err != nil {
		if docker.IsUnreachable(err) {
			return nil, cannotCheck(Die("cannot check image '%s': is the docker engine running? (%v)", image, err))
		}
		return nil, cannotCheck(Die("cannot check image '%s': %v", image, err))
	}
	return rep, nil
}

// imageGuest is what caboose check-image asks of the builder guest
// (builder.Guest).
type imageGuest interface {
	Has(image string) (bool, error)
	Pull(image string, progress io.Writer) error
	Check(image string, probe []byte, uid, gid int) (string, *imagecheck.Report, error)
	Stop() error
}

// checkImageVM is the check under isolation vm, where there may be no
// docker engine: in the builder guest caboose build uses, whose store
// keeps the bases builds made or pulled. The image is looked for there,
// pulled there when named, and probed in a container of it there, with
// the same imagecheck.Args as under docker. An image only a docker engine
// on this Mac holds cannot reach the guest: its pull fails, and says so.
func (a *App) checkImageVM(out *ui, image string, named bool) (*imagecheck.Report, error) {
	probe, err := assets.ProbeScript()
	if err != nil {
		return nil, cannotCheck(Die("%v", err))
	}
	boot := a.checkGuest
	if boot == nil {
		boot = func() (imageGuest, error) { return a.startBuilder("", "") }
	}
	g, err := boot()
	if err != nil {
		return nil, cannotCheck(err)
	}
	defer g.Stop()
	has, err := g.Has(image)
	if err != nil {
		return nil, cannotCheck(Die("cannot check image '%s': the builder guest could not look it up (%v)", image, err))
	}
	if !has {
		if !named {
			return nil, notBuiltYet(a, image)
		}
		a.Note("image '%s' is not in the builder guest's store; pulling it there (docker's output follows)", image)
		if err := g.Pull(image, a.Stderr); err != nil {
			a.Note("under isolation vm an image is checked in caboose's builder guest, which pulls it from its registry:")
			a.Note("an image only a docker engine on this Mac holds cannot reach it. Push it to a registry first, or")
			a.Note("check it in that engine, from an environment whose isolation is container or gvisor: caboose -e ENV check-image %s", image)
			return nil, &ExitError{Code: checkFailed, Msg: fmt.Sprintf("cannot pull image '%s' in the builder guest (%v)", image, err)}
		}
	}
	sp := checkSpinner(out, "checking "+image+" in a container of it, in the builder guest")
	_, rep, err := g.Check(image, probe, os.Getuid(), os.Getgid())
	sp.end()
	if err != nil {
		return nil, cannotCheck(Die("%v", err))
	}
	return rep, nil
}

// notBuiltYet is the answer for a base caboose builds before a build made it.
func notBuiltYet(a *App, image string) error {
	a.Note("run 'caboose build' to build it, or name an image to check: caboose check-image IMAGE")
	return &ExitError{Code: checkFailed, Msg: fmt.Sprintf("base image '%s' is not built yet", image)}
}

// checkSpinner is a spinner on a terminal while the probe runs, which
// tries the network: seconds. Anywhere else it is nothing.
func checkSpinner(out *ui, what string) interface{ end() } {
	if out.width == 0 {
		return noSpinner{}
	}
	return startSpinner(out, what)
}

type noSpinner struct{}

func (noSpinner) end() {}

// checkMarks are how each level of the checklist shows.
var checkMarks = map[imagecheck.Level]mark{imagecheck.Met: markOK, imagecheck.Noted: markNote, imagecheck.Unmet: markProblem}

// checkImageSays writes, on stderr, what the checklist does not: each unmet
// requirement with why it is needed, and what is worth knowing. On a
// terminal they are marked and wrapped, as the checklist is; anywhere else
// they are caboose's notes, a line each.
func (a *App) checkImageSays(rep *imagecheck.Report, problems []string) {
	u := newUI(a.Stderr, termWidth(a.Stderr, 100))
	unreachable := "claude.ai did not answer from a container of this image. Not a failure: the network " +
		"may differ where it runs, or a proxy be set later. But Claude Code installs from there."
	notes := rep.Notes()
	if n := rep.DockerInsideNote(); n != "" && a.isVM() {
		notes = append(notes, n)
	}
	if u.width == 0 {
		for _, n := range notes {
			a.Note("%s", n)
		}
		for _, p := range problems {
			a.Note("%s", p)
		}
		if rep.Unreachable() {
			a.Note("claude.ai did not answer from a container of this image. Not a failure: the network")
			a.Note("may differ where it runs, or a proxy be set later. But Claude Code installs from there.")
		}
		return
	}
	// "LABEL: state -- why", as a row: the label, then the rest.
	lw := 0
	for _, p := range problems {
		label, _, _ := strings.Cut(p, ": ")
		lw = max(lw, len(label))
	}
	for _, p := range problems {
		label, rest, _ := strings.Cut(p, ": ")
		state, why, _ := strings.Cut(rest, " -- ")
		text := u.paint(bold, state)
		if why != "" {
			text += " " + u.paint(dim, "— "+why)
		}
		fmt.Fprint(u.out, u.wrap(text, "    "+label+strings.Repeat(" ", lw-len(label))+"  ", strings.Repeat(" ", lw+6)))
	}
	if len(problems) > 0 && (len(notes) > 0 || rep.Unreachable()) {
		u.blank()
	}
	for _, n := range notes {
		u.warn("%s", capFirst(n))
	}
	if rep.Unreachable() {
		u.warn("%s", unreachable)
	}
}

// cannotCheck gives a failure to reach an answer the could-not-check status.
func cannotCheck(err error) error {
	if ee, ok := err.(*ExitError); ok {
		return &ExitError{Code: checkFailed, Msg: ee.Msg}
	}
	return &ExitError{Code: checkFailed, Msg: err.Error()}
}
