// Package assets gives the rest of the launcher access to the files embedded
// from the repo root: the images' build contexts, the sandbox CLAUDE.md and
// the image probe.
package assets

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	caboose "github.com/bfreis/caboose"
)

// ContextFile is one file of an image build context.
type ContextFile struct {
	Name string
	Mode os.FileMode
}

// The build contexts. Every image caboose runs is a base with the layer on
// top: the base comes from the environment's image profile (packages
// built with apko, a Dockerfile of the user's, or an image of theirs);
// the layer is the same on all of them. They replaced the checkout's
// deny-all-plus-allowlist .dockerignore: a new COPY needs a line here and
// a name in the //go:embed directive in embed.go, and forgetting either
// fails loudly (in go test, or at build time) rather than leaking anything.
var (
	// BaseContext is the embedded Dockerfile, the seed a dockerfile
	// profile's dir starts from (Seed): OS packages and toolchains, no
	// COPY. caboose never builds it from here.
	BaseContext = []ContextFile{
		{Name: "Dockerfile", Mode: 0o644},
	}
	// LayerContext is the layer's Dockerfile and the files it COPYs.
	LayerContext = []ContextFile{
		{Name: LayerDockerfile, Mode: 0o644},
		{Name: "layer-user.sh", Mode: 0o755},
		{Name: "entrypoint.sh", Mode: 0o755},
		{Name: "xdg-open.sh", Mode: 0o755},
		{Name: "tmux.conf", Mode: 0o644},
		{Name: "shellrc.bash", Mode: 0o644},
		// Both, whatever the platform: the layer COPYs the one for its
		// TARGETARCH, and the hash covers the image on either.
		{Name: AgentBinary("amd64"), Mode: 0o755},
		{Name: AgentBinary("arm64"), Mode: 0o755},
	}
)

// AgentBinary is caboose-agent for arch (amd64, arm64), in the embedded
// files and in the layer's context.
func AgentBinary(arch string) string { return "agent-bin/caboose-agent-linux-" + arch }

// LayerDockerfile is the layer's Dockerfile, in the embedded FS and in its
// build context; `docker build -f` names it.
const LayerDockerfile = "layer.Dockerfile"

// Labels caboose build puts on the image, so a launcher can tell which build of
// itself made the image it is about to run, and on what. The version is
// informational; the hashes are what decide whether the image is out of
// date, since two builds with the same version string (dev, say) can embed
// different files, and a new release that changed nothing in the context
// need not force a rebuild.
const (
	LabelVersion = "dev.bfreis.caboose.version"
	// LabelLayerHash is LayerHash, the layer's context.
	LabelLayerHash = "dev.bfreis.caboose.layer-hash"
	// LabelBaseHash is what identifies the base the layer was built on,
	// by its kind: an apko lock's hash (apkobuild.Lock.Hash), DirHash of a
	// dockerfile profile's dir, and empty on a ref, which is identified by
	// LabelBaseID instead.
	LabelBaseHash = "dev.bfreis.caboose.base-hash"
	// LabelBaseKind is which kind of base the layer was built on:
	// BaseKindApko, BaseKindDockerfile or BaseKindRef. Said outright rather
	// than read off LabelBaseHash, which a label inherited through FROM
	// could fake.
	LabelBaseKind = "dev.bfreis.caboose.base-kind"
	// LabelBaseName is the base as it was named: the tag caboose gave the
	// base it built, or a ref profile's image as set.
	LabelBaseName = "dev.bfreis.caboose.base-name"
	// LabelBaseID is the image ID of the base the layer was built on, the
	// one the image check passed. On a ref it is what tells a pull or
	// rebuild of the base since.
	LabelBaseID = "dev.bfreis.caboose.base-id"
	// LabelPlatform is the Claude Code platform (linux-x64, linux-arm64-musl,
	// ...) the image check found the image to be, which names the data dir's
	// dot_local/<platform> the launcher mounts.
	LabelPlatform = "dev.bfreis.caboose.platform"
	// LabelUID and LabelGID are the host IDs the layer's agent user was
	// made with: an image built for another host user is not this one's.
	LabelUID = "dev.bfreis.caboose.uid"
	LabelGID = "dev.bfreis.caboose.gid"
	// LabelCompat is Compat as the launcher that built the layer had it:
	// what a launcher reads off a running container to tell whether it can
	// still work with it.
	LabelCompat = "dev.bfreis.caboose.compat"
	// LabelRunArgs is on the container, not the image: the user's own
	// docker run arguments it was created with (config.RunArgs), as
	// a JSON list, "" for none. Set on every container, so one inherited
	// from a base never reads as the container's.
	LabelRunArgs = "dev.bfreis.caboose.run-args"
	// LabelIsolation and LabelUser are on the container too: the isolation
	// kind it was created with (config.Isolation) and the user it runs as, ""
	// for the image's agent user, "0:0" where gVisor gives the agent no way
	// to write its mounts. Set on every container, as LabelRunArgs is.
	LabelIsolation = "dev.bfreis.caboose.isolation"
	LabelUser      = "dev.bfreis.caboose.user"
	// LabelProfile is on the container too: the isolation profile it was
	// created with, "<kind>.<name>" as config.toml names it, or just the
	// kind (say "container") when config.toml defines no profile. Set on
	// every container, as LabelRunArgs is.
	LabelProfile = "dev.bfreis.caboose.profile"
	// LabelEgress is on the container too: "on" for a VM created with the
	// outbound proxy in its environment (a vm profile's egress), ""
	// otherwise. Set
	// on every container, as LabelRunArgs is.
	LabelEgress = "dev.bfreis.caboose.egress"
	// LabelHostname is on the container too: the hostname it was created
	// with. Set on every container, as LabelRunArgs is.
	LabelHostname = "dev.bfreis.caboose.hostname"
	// LabelRoots is on the container too: the container paths of the roots
	// it was created with, as a JSON list, which tells its root mounts from
	// the rest, since a root may be mounted anywhere. Set on every
	// container, as LabelRunArgs is.
	LabelRoots = "dev.bfreis.caboose.roots"
)

// Compat is the version of what the launcher expects of a container it did
// not necessarily create -- the entrypoint's protocol (--cc-prune,
// --cc-supervise, the ready marker), the paths and mounts it execs into,
// what the image provides. A launcher updates itself, and then meets a
// container an older one created, with sessions in it: as long as Compat
// is the same, it works with that container as it is, and the new image
// waits for the next restart. Raise it only when a change breaks that --
// then a launch refuses the old container and says 'caboose restart' --
// never merely because the image's files changed: the hashes already say
// that, and a rebuild at the next restart is all it takes.
const Compat = 1

// The values of LabelBaseKind: the image kinds of config.toml.
const (
	BaseKindApko       = "apko"
	BaseKindDockerfile = "dockerfile"
	BaseKindRef        = "ref"
)

// LayerLabels are every label caboose puts on the layer, in the order the
// build passes them. A label is inherited through FROM, and a user's base
// may well be built FROM a caboose image -- a base caboose built, or a
// whole sandbox -- so a label the layer left unset would read as the base's. Every
// one is therefore set, to "" where it does not apply (LabelBaseHash on a
// ref), and the launcher reads an empty value as unset.
var LayerLabels = []string{
	LabelVersion, LabelLayerHash, LabelBaseKind, LabelBaseHash,
	LabelBaseName, LabelBaseID, LabelPlatform, LabelUID, LabelGID, LabelCompat,
}

// SandboxInstructionsPath is where the sandbox-wide CLAUDE.md sits, both in
// the embedded FS and in a checkout.
const SandboxInstructionsPath = "sandbox/CLAUDE.md"

// SandboxInstructions returns the embedded copy of sandbox/CLAUDE.md.
func SandboxInstructions() ([]byte, error) {
	return fs.ReadFile(caboose.Files, SandboxInstructionsPath)
}

// SandboxSkillsPath is where the sandbox-wide skills sit, both in the
// embedded FS and in a checkout: one directory per skill.
const SandboxSkillsPath = "sandbox/skills"

// SandboxSkills returns the embedded copy of sandbox/skills, rooted there.
func SandboxSkills() (fs.FS, error) {
	return fs.Sub(caboose.Files, SandboxSkillsPath)
}

// WriteLayerContext writes the layer's build context into dir, which should
// be a fresh, empty directory.
func WriteLayerContext(dir string) error {
	return writeContext(caboose.Files, LayerContext, dir)
}

func writeContext(fsys fs.FS, files []ContextFile, dir string) error {
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f.Name)
		if err != nil {
			return fmt.Errorf("embedded %s: %w%s", f.Name, err, missingHint(f.Name))
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f.Name)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), data, f.Mode); err != nil {
			return err
		}
		// WriteFile's mode is cut by the umask, and COPY carries the mode
		// into the image: entrypoint.sh has to arrive executable.
		if err := os.Chmod(filepath.Join(dir, f.Name), f.Mode); err != nil {
			return err
		}
	}
	return nil
}

// missingHint explains a missing agent binary: a launcher built without
// `make agent` first.
func missingHint(name string) string {
	if filepath.Dir(name) == "agent-bin" {
		return " (this launcher was built without its agent: build it with make, which runs `make agent` first)"
	}
	return ""
}

// Hash tags: the scheme each hash is made under, so that none of them can
// collide with another, or with a later scheme.
const (
	tagLayer = "caboose-layer-v1"
	tagDir   = "caboose-envimage-v1"
)

// LayerHash is the sha256, in hex, of the layer's context. It panics if
// the context cannot be read, which only a broken build of the launcher
// can cause, and which TestContextsHoldEveryCopySource already guards.
func LayerHash() string { return mustHash(tagLayer, LayerContext) }

func mustHash(tag string, files []ContextFile) string {
	h, err := contextHash(caboose.Files, tag, files)
	if err != nil {
		panic(err)
	}
	return h
}

// contextHash hashes each file in list order as its name, its mode and its
// content, after tag. Name and content are length-prefixed, so the framing
// is unambiguous: bytes moved from the end of one file to the start of the
// next, or a file renamed, change the hash. The mode is the one the context
// is written with, since that is what the image sees (entrypoint.sh losing
// its x bit is a different image). The leading tag lets the scheme change
// later without colliding with hashes made under this one.
func contextHash(fsys fs.FS, tag string, files []ContextFile) (string, error) {
	h := sha256.New()
	h.Write([]byte(tag + "\x00"))
	var n [8]byte
	frame := func(b []byte) {
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f.Name)
		if err != nil {
			return "", fmt.Errorf("embedded %s: %w%s", f.Name, err, missingHint(f.Name))
		}
		frame([]byte(f.Name))
		binary.BigEndian.PutUint32(n[:4], uint32(f.Mode))
		h.Write(n[:4])
		frame(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DirHash is the sha256, in hex, of a build context on disk: a dockerfile
// profile's dir. Every entry under dir, in path order, goes in
// as its kind, its path, its permission bits and its content -- a file's
// bytes, a symlink's target (never followed: docker sends it as a link) --
// so any edit, added file, rename or chmod changes it. Everything counts,
// a README or a .dockerignore too: a change that does not change the image
// costs a rebuild, and one that does is never missed.
func DirHash(dir string) (string, error) {
	h := sha256.New()
	h.Write([]byte(tagDir + "\x00"))
	var n [8]byte
	frame := func(b []byte) {
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	root := os.DirFS(dir)
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		var kind string
		var content []byte
		switch {
		case d.IsDir():
			kind = "d"
		case d.Type()&fs.ModeSymlink != 0:
			kind = "l"
			target, err := os.Readlink(filepath.Join(dir, filepath.FromSlash(p)))
			if err != nil {
				return err
			}
			content = []byte(target)
		case d.Type().IsRegular():
			kind = "f"
			if content, err = fs.ReadFile(root, p); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: not a file, directory or symlink", filepath.Join(dir, p))
		}
		frame([]byte(kind + p))
		binary.BigEndian.PutUint32(n[:4], uint32(fi.Mode().Perm()))
		h.Write(n[:4])
		frame(content)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
