// Package proposal is how a session in the sandbox asks for a change only
// the host can make: a tool installed in the image, or another repo root.
// (What the sandbox keeps of its home is its own to change, in the sandbox
// config: no proposal.) The session writes a
// proposal, a small TOML file, into the data dir's proposals/ (mounted at
// ContainerDir); `caboose apply`, on the host, shows each one and applies
// it when the user says so.
//
// Everything in a proposal is the sandbox's, and so untrusted: it is read
// through nofollow, refused whole when anything in it is out of place --
// an unknown key, a control character or a bidi override that could make
// the terminal show other than what is there, a section that starts a
// stage -- and what it may ask for is an allowlist: a Dockerfile section,
// and one root. Nothing else in config.toml can be proposed at all.
//
// The host also writes what a proposal is made against into proposals/
// current/ (WriteCurrent): the Dockerfile the next build uses, and the
// roots config.toml has, which the sandbox cannot otherwise see. A proposal for a section names the hash of that
// Dockerfile, and is refused when the real one has changed since.
package proposal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
)

const (
	// Dir holds the proposals, in the data dir.
	Dir = datadir.ProposalsDir
	// ContainerDir is where the container mounts Dir.
	ContainerDir = config.ContainerHome + "/.caboose-proposals"
	// CurrentDir, in Dir, is what the host last said a proposal is made
	// against (WriteCurrent).
	CurrentDir = "current"
	// Ext is a proposal's file extension.
	Ext = ".toml"
	// MaxSize is the largest proposal read: a Dockerfile section, not a
	// place to put files.
	MaxSize = 64 << 10
)

// sandboxHint is where what the sandbox keeps is changed instead.
const sandboxHint = "~/.config/caboose/sandbox.toml"

// Section is a Dockerfile section, as assets.SetSection takes it.
type Section struct {
	Name  string `toml:"name"`
	Title string `toml:"title"`
	Body  string `toml:"body"`
}

// Root is a repo root to add: its name, the directory under /work, and
// its host path as proposed.
type Root struct{ Name, Path string }

// Proposal is one proposal, checked.
type Proposal struct {
	// Name is the file's name without Ext.
	Name   string
	Title  string
	Reason string
	// DockerfileSHA256 is the hash of the Dockerfile Section was written
	// against; set exactly when Section is.
	DockerfileSHA256 string
	Section          *Section
	// Root is the root to add, or nil.
	Root *Root
}

// file is a proposal as written.
type file struct {
	Title            string            `toml:"title"`
	Reason           string            `toml:"reason"`
	DockerfileSHA256 *string           `toml:"dockerfile_sha256"`
	Section          *Section          `toml:"section"`
	Roots            map[string]string `toml:"roots"`
}

// Parse reads and checks the proposal in file name (with Ext).
func Parse(name string, data []byte) (*Proposal, error) {
	stem, ok := strings.CutSuffix(name, Ext)
	if !ok || !config.ValidRootName(stem) {
		return nil, fmt.Errorf("%q is not a proposal's name: lowercase letters, digits, - and _, at most 32, then %s", name, Ext)
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("larger than %d KiB", MaxSize>>10)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("not UTF-8")
	}
	var f file
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&f)
	if err != nil {
		return nil, fmt.Errorf("not a proposal: %v", err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		keys := make([]string, len(u))
		for i, k := range u {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown key %s (a proposal has title, reason, dockerfile_sha256, [section] and [roots]; what the sandbox keeps is changed in %s, with no proposal)",
			strings.Join(keys, ", "), sandboxHint)
	}
	p := &Proposal{Name: stem, Title: strings.TrimSpace(f.Title), Reason: strings.TrimSpace(f.Reason)}
	if err := checkText("title", p.Title, false, 100); err != nil {
		return nil, err
	}
	if p.Title == "" {
		return nil, errors.New("no title")
	}
	if err := checkText("reason", p.Reason, true, 2000); err != nil {
		return nil, err
	}

	switch {
	case f.Section == nil && f.DockerfileSHA256 != nil:
		return nil, errors.New("dockerfile_sha256 without a [section]")
	case f.Section != nil && f.DockerfileSHA256 == nil:
		return nil, errors.New("a [section] needs dockerfile_sha256: the hash of the Dockerfile it was written against (" + CurrentDir + "/state.toml)")
	case f.Section != nil:
		s := *f.Section
		s.Title = strings.TrimSpace(s.Title)
		for _, t := range []struct {
			field, s string
			multi    bool
			max      int
		}{{"section.name", s.Name, false, 32}, {"section.title", s.Title, false, 100}, {"section.body", s.Body, true, MaxSize}, {"dockerfile_sha256", *f.DockerfileSHA256, false, 64}} {
			if err := checkText(t.field, t.s, t.multi, t.max); err != nil {
				return nil, err
			}
		}
		// SetSection checks the rest; on an empty Dockerfile, the section
		// alone.
		if _, err := assets.SetSection(nil, s.Name, s.Title, s.Body); err != nil {
			return nil, fmt.Errorf("[section]: %v", err)
		}
		p.Section, p.DockerfileSHA256 = &s, *f.DockerfileSHA256
	}

	switch len(f.Roots) {
	case 0:
	case 1:
		for name, dir := range f.Roots {
			if !config.ValidRootName(name) {
				return nil, fmt.Errorf("'%s' is not a root name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", name)
			}
			if err := checkText("roots."+name, dir, false, 1000); err != nil {
				return nil, err
			}
			if dir == "" {
				return nil, fmt.Errorf("roots.%s names no directory", name)
			}
			p.Root = &Root{Name: name, Path: dir}
		}
	default:
		return nil, errors.New("more than one root: a proposal adds one at most, so that each is confirmed on its own")
	}

	if p.Section == nil && p.Root == nil {
		return nil, errors.New("it proposes nothing: no [section] or [roots]")
	}
	return p, nil
}

// checkText refuses what could make a terminal show other than what s
// holds -- control characters (escape sequences start with one), and the
// invisible format characters, bidi overrides among them -- and more than
// max runes. A newline is allowed where multi is, and a tab anywhere.
func checkText(field, s string, multi bool, max int) error {
	n := 0
	for i, r := range s {
		n++
		switch {
		case r == '\t', r == '\n' && multi:
		case r < 0x20 || r >= 0x7f && r < 0xa0,
			unicode.Is(unicode.Cf, r), r == 0x2028, r == 0x2029:
			return fmt.Errorf("%s has the character %U at byte %d, which a terminal would not show as it is", field, r, i)
		}
	}
	if n > max {
		return fmt.Errorf("%s is longer than %d characters", field, max)
	}
	return nil
}

// Hash is the hash a proposal names a Dockerfile by: sha256, in hex.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Entry is one file in Dir: its proposal, or why it is not one.
type Entry struct {
	// File is its name in Dir.
	File     string
	Proposal *Proposal
	Err      error
	Modified time.Time
}

// List reads every proposal in dataDir's Dir, in name order. Hidden files,
// directories and files not ending in Ext are not proposals; anything else
// that cannot be read as one is listed with its error.
func List(dataDir string) ([]Entry, error) {
	d := nofollow.Dir(dataDir)
	es, err := d.ReadDir(Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range es {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.IsDir() || !strings.HasSuffix(name, Ext) {
			continue
		}
		ent := Entry{File: name}
		ent.Proposal, ent.Modified, ent.Err = read(d, name)
		out = append(out, ent)
	}
	return out, nil
}

// read reads and parses Dir/name, refusing one too large before reading
// it.
func read(d nofollow.Dir, name string) (*Proposal, time.Time, error) {
	rel := path.Join(Dir, name)
	fi, err := d.Lstat(rel)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fi.ModTime(), errors.New("not a plain file")
	}
	if fi.Size() > MaxSize {
		return nil, fi.ModTime(), fmt.Errorf("larger than %d KiB", MaxSize>>10)
	}
	data, _, err := d.ReadFile(rel)
	if err != nil {
		return nil, fi.ModTime(), err
	}
	p, err := Parse(name, data)
	return p, fi.ModTime(), err
}

// Remove deletes proposal file from dataDir's Dir.
func Remove(dataDir, file string) error {
	return nofollow.Dir(dataDir).Remove(path.Join(Dir, file))
}

// Names are the names of the proposals in entries, for a message.
func Names(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.File, Ext))
	}
	return out
}

// Printable is s with every character checkText refuses written as its
// \u escape: for saying what the sandbox wrote -- a file's name, an error
// quoting a proposal -- on the host's terminal.
func Printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != ' ' && (r < 0x20 || r >= 0x7f && r < 0xa0 || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 || r == utf8.RuneError) {
			fmt.Fprintf(&b, `\u%04X`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
