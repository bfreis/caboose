package builder

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/imagecheck"
)

// Request is one build of the sandbox's image: a base, checked, and the
// layer on it, as caboose build does with docker today.
type Request struct {
	// The base: BaseContext, a directory with a Dockerfile, built as
	// BaseTag; or else BaseRef, pulled when the builder lacks it or Pull
	// says so.
	BaseContext string
	BaseTag     string
	BaseLabels  []string // KEY=VALUE
	BaseRef     string
	Pull        bool
	// Probe is imagecheck.sh; UID and GID are the host's, for the check
	// and the layer's agent user.
	Probe    []byte
	UID, GID int
	// The layer: its context (assets.WriteLayerContext's), built as
	// Image, labelled with LayerLabels.
	LayerContext string
	LayerFile    string // its Dockerfile, within the context
	Image        string
	LayerLabels  []string
	// BaseArgs and LayerArgs are more docker build flags, a user's, for
	// the base and for the layer.
	BaseArgs, LayerArgs []string
}

// Result is what a build made.
type Result struct {
	Base    string // what the layer was built FROM
	BaseID  string
	Report  *imagecheck.Report
	ImageID string
	Config  Config
}

// CheckError is a base the image check refused.
type CheckError struct{ Report *imagecheck.Report }

func (e *CheckError) Error() string {
	return fmt.Sprintf("base image '%s' does not meet caboose's requirements:\n  %s",
		e.Report.Image, strings.Join(e.Report.Problems(), "\n  "))
}

// Build builds req in the guest, and writes the image as a root disk onto
// the output disk Start attached.
func (g *Guest) Build(req Request) (Result, error) {
	var r Result
	switch {
	case req.BaseContext != "":
		r.Base = req.BaseTag
		argv := []string{"docker", "build", "--progress=plain", noProvenance, "-t", req.BaseTag}
		for _, l := range req.BaseLabels {
			argv = append(argv, "--label", l)
		}
		argv = append(argv, g.egressArgs()...)
		argv = append(argv, req.BaseArgs...)
		if err := g.runWithContext(baseTimeout, req.BaseContext, append(argv, "-")...); err != nil {
			return r, fmt.Errorf("building the base: %w", err)
		}
	case req.BaseRef != "":
		r.Base = req.BaseRef
		_, _, err := g.Inspect(req.BaseRef)
		if err != nil || req.Pull {
			if err := g.Pull(req.BaseRef, nil); err != nil {
				return r, fmt.Errorf("pulling base image '%s': %w", req.BaseRef, err)
			}
		}
	default:
		return r, fmt.Errorf("a build needs a base")
	}
	id, rep, err := g.Check(r.Base, req.Probe, req.UID, req.GID)
	if err != nil {
		return r, err
	}
	r.BaseID, r.Report = id, rep
	if !rep.OK() {
		return r, &CheckError{Report: rep}
	}

	args := []string{noProvenance, "-f", req.LayerFile,
		"--build-arg", "BASE=" + r.Base,
		"--build-arg", "CABOOSE_UID=" + strconv.Itoa(req.UID),
		"--build-arg", "CABOOSE_GID=" + strconv.Itoa(req.GID)}
	for _, l := range req.LayerLabels {
		args = append(args, "--label", l)
	}
	args = append(args, g.egressArgs()...)
	args = append(args, req.LayerArgs...)
	build := append([]string{"docker", "build", "--progress=plain", "-t", req.Image}, args...)
	if err := g.runWithContext(stepTimeout, req.LayerContext, append(build, "-")...); err != nil {
		return r, fmt.Errorf("building the caboose layer: %w", err)
	}
	// The same build again, cached, as a filesystem onto the output disk.
	if err := g.runWithContext(stepTimeout, req.LayerContext, append([]string{script, "root"}, args...)...); err != nil {
		return r, fmt.Errorf("writing the root disk: %w", err)
	}
	if r.ImageID, r.Config, err = g.Inspect(req.Image); err != nil {
		return r, err
	}
	return r, nil
}

// Has reports whether the builder's store holds image: a base an earlier
// build made or pulled, since its cache disk is kept.
func (g *Guest) Has(image string) (bool, error) {
	_, _, err := g.Inspect(image)
	var se *StepError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &se) && strings.Contains(strings.ToLower(se.Said), "no such image"):
		return false, nil
	}
	return false, err
}

// Pull pulls image into the builder's store, docker's progress to progress
// (nil for none). What docker says of a failure is in the error only.
func (g *Guest) Pull(image string, progress io.Writer) error {
	return g.runQuiet(baseTimeout, nil, progress, "docker", "pull", image)
}

// Check is the image check on image, which the builder's store holds: its
// ID and the probe's report, as imagecheck.Run gives it under docker --
// an image without /bin/sh is a report saying so, not an error. probe is
// imagecheck.sh; uid and gid are the host's.
func (g *Guest) Check(image string, probe []byte, uid, gid int) (string, *imagecheck.Report, error) {
	id, _, err := g.Inspect(image)
	if err != nil {
		return "", nil, err
	}
	out, err := g.Output(stepTimeout, probeArgv(imagecheck.Args(id, "", probe, uid, gid), g.egressEnv())...)
	var se *StepError
	switch {
	case errors.As(err, &se) && imagecheck.NoShell(se.ExitCode(), se.Said, out):
		return id, imagecheck.NoShellReport(image, uid, gid), nil
	case err != nil:
		return "", nil, fmt.Errorf("cannot check image '%s': %w", image, err)
	}
	rep, err := imagecheck.Parse(out)
	if err != nil {
		return "", nil, fmt.Errorf("checking image '%s': %w", image, err)
	}
	rep.Image = image
	return id, rep, nil
}

// probeArgv is the docker command that runs the probe in the builder:
// imagecheck.Args, with the container on the guest's own network. The
// sandbox under vm is a guest of the same kind, on the same NAT, with no
// docker bridge between it and the network, so the probe's network check
// answers for the sandbox only from the guest's stack; the builder's
// bridge, and its NAT inside the guest, are a path the sandbox never
// takes. env is the outbound proxy's environment, when the sandbox has
// it, for the same reason.
func probeArgv(args, env []string) []string {
	if len(args) == 0 || args[0] != "run" {
		panic("imagecheck.Args is not a docker run")
	}
	argv := []string{"docker", "run", "--network=host"}
	for _, e := range env {
		argv = append(argv, "-e", e)
	}
	return append(argv, args[1:]...)
}

// egressEnv is the outbound proxy's environment, nil without one.
func (g *Guest) egressEnv() []string {
	if g.egress == "" {
		return nil
	}
	return agentproto.EgressEnv(g.egress)
}

// egressArgs are the docker build flags that take a build's RUN steps
// through the outbound proxy, nil without one: the guest's own network,
// since the agent serves the proxy on the guest's loopback, which a step
// on dockerd's bridge cannot reach (dockerd's BuildKit grants
// network.host unless its config says otherwise, and buildx asks for it
// on --network=host), and the proxy's variables as build args, which
// curl, apt and the rest in a step read. They are Docker's predefined
// proxy args: never in the image's history or config, and no part of a
// step's cache key unless the Dockerfile declares them. The network mode
// is part of it, so a step cached without the proxy runs once again with
// it. A user's own build flags come after these, and win.
func (g *Guest) egressArgs() []string {
	if g.egress == "" {
		return nil
	}
	args := []string{"--network=host"}
	for _, e := range g.egressEnv() {
		args = append(args, "--build-arg", e)
	}
	return args
}

// noProvenance keeps BuildKit's default provenance attestation off every
// build: dockerd's containerd image store gives an image with one an index
// for an ID, and the attestation, new on every build (its invocation, its
// times), gives a fully cached rebuild a new ID -- a new base ID, so a new
// label on the layer, and a new root disk, and a sandbox marked stale. The
// guest's docker build is always buildx's (caboose-builder root needs -o),
// which has taken --provenance since v0.10.
const noProvenance = "--provenance=false"

// runWithContext runs argv with dir, as a tar, on its stdin.
func (g *Guest) runWithContext(timeout time.Duration, dir string, argv ...string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, dir)) }()
	defer pr.Close()
	return g.Run(timeout, pr, nil, argv...)
}

// writeTar writes dir's tree as a tar, names relative to it, modes kept:
// a build context. Owners are not: docker gives a COPY root's unless told.
func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
