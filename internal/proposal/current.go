package proposal

import (
	"bytes"
	"errors"
	"io/fs"
	"path"

	"github.com/BurntSushi/toml"

	"github.com/bfreis/caboose/internal/nofollow"
)

// Where the Dockerfile a section goes into comes from, as State.Source
// says it. Only a dockerfile image profile has one.
const (
	// SourceDockerfile is the dockerfile profile's own Dockerfile.
	SourceDockerfile = "dockerfile"
	// SourceSeed is caboose's seed, which the first proposal for a section
	// writes into the dockerfile profile's dir: it has no Dockerfile yet.
	SourceSeed = "seed"
)

// State is what a proposal is made against, as the host last saw it.
type State struct {
	// Image is the image profile in use, "<kind>.<name>".
	Image string
	// Source is where the Dockerfile comes from, a Source constant; ""
	// under an apko or ref profile, which has none, so no section can be
	// proposed.
	Source string
	// Dockerfile is the next build's Dockerfile; nil with no Source.
	Dockerfile []byte
	// Apko is an apko profile's packages; nil under any other kind.
	Apko *ApkoState
	// Roots are config.toml's roots, by name, the host path as written.
	Roots map[string]StateRoot
}

// ApkoState is what an apko image profile is made of.
type ApkoState struct {
	// Packages are the profile's own packages, which a proposal may remove
	// from; Defaults is whether caboose's default groups come with them.
	Packages []string
	Defaults bool
	// Installed is every package the profile stands for: caboose's and its
	// own (pkgset.Spec.List), which a proposal adds to.
	Installed []string
}

// StateRoot is one root in state.toml: its host path as config.toml has
// it, and its path in the sandbox.
type StateRoot struct {
	Host string `toml:"host"`
	Path string `toml:"path"`
}

// stateFile is State as state.toml has it.
type stateFile struct {
	Image            string               `toml:"image"`
	Packages         *[]string            `toml:"packages,omitempty"`
	Defaults         *bool                `toml:"defaults,omitempty"`
	Installed        *[]string            `toml:"installed,omitempty"`
	Dockerfile       string               `toml:"dockerfile,omitempty"`
	DockerfileSHA256 string               `toml:"dockerfile_sha256,omitempty"`
	Roots            map[string]StateRoot `toml:"roots,omitempty"`
}

const stateHeader = `# Written by caboose on the host, at every launch and after 'caboose apply':
# what a proposal is made against. Changing it here changes nothing; see
# /etc/claude-code/CLAUDE.md for how to propose a change.
#
# image: the image profile the sandbox is built from, "<kind>.<name>".
# What a proposal can change in it depends on the kind:
#
# apko: a [packages] proposal (add, remove). packages are the profile's
# own, which remove takes from; defaults is whether caboose's default
# groups come with them; installed is every package the image has asked
# for, caboose's and the profile's, which add must not repeat.
#
# dockerfile: a [section] proposal. dockerfile says where the Dockerfile
# it goes into comes from -- "dockerfile" (the profile's own) or "seed"
# (none yet: a proposed section is added to caboose's seed, which becomes
# the profile's Dockerfile). Dockerfile, next to this file, is it, and
# dockerfile_sha256 its hash, which a proposal names.
#
# ref: an image of the user's own; nothing in it can be proposed.

`

// WriteCurrent writes s into dataDir's Dir/CurrentDir: state.toml, and
// the Dockerfile when there is one (a stale one removed when not). Every
// write goes through nofollow: the container can write the directory too.
func WriteCurrent(dataDir string, s State) error {
	sf := stateFile{Image: s.Image, Dockerfile: s.Source, Roots: s.Roots}
	if a := s.Apko; a != nil {
		pkgs := append([]string{}, a.Packages...)
		inst := append([]string{}, a.Installed...)
		def := a.Defaults
		sf.Packages, sf.Defaults, sf.Installed = &pkgs, &def, &inst
	}
	if s.Dockerfile != nil {
		sf.DockerfileSHA256 = Hash(s.Dockerfile)
	}
	var b bytes.Buffer
	b.WriteString(stateHeader)
	if err := toml.NewEncoder(&b).Encode(sf); err != nil {
		return err
	}
	d := nofollow.Dir(dataDir)
	dir := path.Join(Dir, CurrentDir)
	if err := d.WriteFile(path.Join(dir, "state.toml"), b.Bytes(), 0o644); err != nil {
		return err
	}
	if s.Dockerfile == nil {
		if err := d.Remove(path.Join(dir, "Dockerfile")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return d.WriteFile(path.Join(dir, "Dockerfile"), s.Dockerfile, 0o644)
}
