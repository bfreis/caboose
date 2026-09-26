package proposal

import (
	"bytes"
	"errors"
	"io/fs"
	"path"

	"github.com/BurntSushi/toml"

	"github.com/bfreis/caboose/internal/nofollow"
)

// Where the next build's Dockerfile comes from, as State.Source says it.
const (
	// SourceImageDir is the environment's own image/Dockerfile.
	SourceImageDir = "image/Dockerfile"
	// SourcePreset is caboose's preset, which the first proposal for a
	// section writes as image/Dockerfile: the environment has none yet.
	SourcePreset = "preset"
	// SourceBaseImage is CABOOSE_BASE_IMAGE: an image, not a Dockerfile,
	// so no section can be proposed.
	SourceBaseImage = "base_image"
)

// State is what a proposal is made against, as the host last saw it.
type State struct {
	// Source is where the Dockerfile comes from: a Source constant.
	Source string
	// Dockerfile is the next build's Dockerfile; nil on SourceBaseImage.
	Dockerfile []byte
	// BaseImage is CABOOSE_BASE_IMAGE, on SourceBaseImage.
	BaseImage string
	// RepoRoot, or else Roots, and Persist are config.toml's, as written.
	RepoRoot string
	Roots    map[string]string
	Persist  map[string]string
}

// stateFile is State as state.toml has it.
type stateFile struct {
	Dockerfile       string            `toml:"dockerfile"`
	DockerfileSHA256 string            `toml:"dockerfile_sha256,omitempty"`
	BaseImage        string            `toml:"base_image,omitempty"`
	RepoRoot         string            `toml:"repo_root,omitempty"`
	Roots            map[string]string `toml:"roots,omitempty"`
	Persist          map[string]string `toml:"persist,omitempty"`
}

const stateHeader = `# Written by caboose on the host, at every launch and after 'caboose apply':
# what a proposal is made against. Changing it here changes nothing; see
# ~/.claude/CLAUDE.md for how to propose a change.
#
# dockerfile: where the next build's Dockerfile comes from -- "image/Dockerfile"
# (the environment's own), "preset" (none yet: a proposed section is added to
# caboose's preset, which becomes image/Dockerfile) or "base_image" (an image,
# not a Dockerfile: no section can be proposed). Dockerfile, next to this file,
# is it, and dockerfile_sha256 its hash, which a proposal names.

`

// WriteCurrent writes s into dataDir's Dir/CurrentDir: state.toml, and
// the Dockerfile when there is one (a stale one removed when not). Every
// write goes through nofollow: the container can write the directory too.
func WriteCurrent(dataDir string, s State) error {
	sf := stateFile{Dockerfile: s.Source, BaseImage: s.BaseImage, RepoRoot: s.RepoRoot, Roots: s.Roots, Persist: s.Persist}
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
