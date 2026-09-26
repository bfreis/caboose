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

// The two build contexts. Every image caboose runs is a base with the layer
// on top: the base is the embedded Dockerfile's image by default, or
// CABOOSE_BASE_IMAGE, which caboose never builds; the layer is the same for
// both. They replaced the checkout's deny-all-plus-allowlist .dockerignore:
// a new COPY needs a line here and a name in the //go:embed directive in
// embed.go, and forgetting either fails loudly (in go test, or at build time)
// rather than leaking anything.
var (
	// BaseContext is the default base: OS packages and toolchains, no COPY.
	BaseContext = []ContextFile{
		{Name: "Dockerfile", Mode: 0o644},
	}
	// LayerContext is the layer's Dockerfile and the files it COPYs.
	LayerContext = []ContextFile{
		{Name: LayerDockerfile, Mode: 0o644},
		{Name: "layer-user.sh", Mode: 0o755},
		{Name: "entrypoint.sh", Mode: 0o755},
		{Name: "tmux.conf", Mode: 0o644},
	}
)

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
	LabelVersion = "io.github.bfreis.caboose.version"
	// LabelLayerHash is LayerHash, the layer's context.
	LabelLayerHash = "io.github.bfreis.caboose.layer-hash"
	// LabelBaseHash is BaseHash when the base was the embedded Dockerfile's,
	// DirHash of the environment's image/ dir when it was built from that,
	// and empty on a user's base, which is identified by LabelBaseID
	// instead. Set on the default base itself too, which is what makes it
	// matter that the layer sets it explicitly (see LayerLabels).
	LabelBaseHash = "io.github.bfreis.caboose.base-hash"
	// LabelBaseKind is which kind of base the layer was built on:
	// BaseKindDefault, BaseKindEnv or BaseKindBYO. Said outright rather
	// than read off LabelBaseHash, which a label inherited through FROM
	// could fake.
	LabelBaseKind = "io.github.bfreis.caboose.base-kind"
	// LabelBaseName is the base as it was named: the default base's tag, or
	// CABOOSE_BASE_IMAGE as set.
	LabelBaseName = "io.github.bfreis.caboose.base-name"
	// LabelBaseID is the image ID of the base the layer was built on, the
	// one the image check passed. On a CABOOSE_BASE_IMAGE it is what tells
	// a pull or rebuild of the base since.
	LabelBaseID = "io.github.bfreis.caboose.base-id"
	// LabelPlatform is the Claude Code platform (linux-x64, linux-arm64-musl,
	// ...) the image check found the image to be, which names the data dir's
	// dot_local/<platform> the launcher mounts.
	LabelPlatform = "io.github.bfreis.caboose.platform"
	// LabelUID and LabelGID are the host IDs the layer's agent user was
	// made with: an image built for another host user is not this one's.
	LabelUID = "io.github.bfreis.caboose.uid"
	LabelGID = "io.github.bfreis.caboose.gid"
	// LabelCompat is Compat as the launcher that built the layer had it:
	// what a launcher reads off a running container to tell whether it can
	// still work with it.
	LabelCompat = "io.github.bfreis.caboose.compat"
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

// The values of LabelBaseKind.
const (
	BaseKindDefault = "default"
	BaseKindBYO     = "byo"
	// BaseKindEnv is a base built from the environment's own image/ dir,
	// identified by DirHash in LabelBaseHash.
	BaseKindEnv = "env"
)

// LayerLabels are every label caboose puts on the layer, in the order the
// build passes them. A label is inherited through FROM, and a user's base
// may well be built FROM a caboose image -- the default base, or a whole
// sandbox -- so a label the layer left unset would read as the base's. Every
// one is therefore set, to "" where it does not apply (LabelBaseHash on a
// user's base), and the launcher reads an empty value as unset.
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

// WriteBaseContext writes the default base's build context into dir, which
// should be a fresh, empty directory.
func WriteBaseContext(dir string) error {
	return writeContext(caboose.Files, BaseContext, dir)
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
			return fmt.Errorf("embedded %s: %w", f.Name, err)
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

// Hash tags: the scheme each hash is made under, so that none of them can
// collide with another, or with a later scheme.
const (
	tagContext = "caboose-context-v1"
	tagBase    = "caboose-base-v1"
	tagLayer   = "caboose-layer-v1"
	tagDir     = "caboose-envimage-v1"
)

// ContextHash is the sha256, in hex, of everything the launcher builds on
// the default base: the base's context and the layer's, together. It
// panics if the context cannot be read, which only a broken build of the
// launcher can cause, and which TestContextsHoldEveryCopySource already
// guards; so do the other hashes.
func ContextHash() string {
	return mustHash(tagContext, append(append([]ContextFile{}, BaseContext...), LayerContext...))
}

// BaseHash is the hash of the default base's context alone.
func BaseHash() string { return mustHash(tagBase, BaseContext) }

// LayerHash is the hash of the layer's context alone.
func LayerHash() string { return mustHash(tagLayer, LayerContext) }

// ContextHashFor is ContextHash on the default base, and LayerHash on a
// user's (byo): the embedded files a build in that mode is made of, which
// caboose version shows as its context.
func ContextHashFor(byo bool) string {
	if byo {
		return LayerHash()
	}
	return ContextHash()
}

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
			return "", fmt.Errorf("embedded %s: %w", f.Name, err)
		}
		frame([]byte(f.Name))
		binary.BigEndian.PutUint32(n[:4], uint32(f.Mode))
		h.Write(n[:4])
		frame(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DirHash is the sha256, in hex, of a build context on disk: an
// environment's image/ dir. Every entry under dir, in path order, goes in
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
