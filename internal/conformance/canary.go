package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/nofollow"
)

// Kind is what shape a canary has.
type Kind int

const (
	File Kind = iota // one regular file
	Dir              // an empty directory
	Tree             // a directory of 3 directories and 6 files
)

// Canary is a file or directory the suite made under B for a check to
// touch: the only kind of thing a check ever touches.
type Canary struct {
	Role string
	Kind Kind
	// Rel is its path relative to B, slash-separated.
	Rel string
	// Paths are every path it is made of, relative to B (Rel first).
	Paths []string
}

// Identity is what the suite records of a path, and compares afterwards
// from the host: a sandbox's own report of what it did is never taken.
type Identity struct {
	Dev, Ino, Nlink uint64
	Mode            fs.FileMode
	Size            int64
	MtimeNs         int64
	// Hash is the sha256 of a regular file's contents or a symlink's
	// target, in hex; "" for anything else.
	Hash string
}

// sameAs compares two identities. A directory's size, link count and
// mtime move with its entries, which are compared on their own, so they
// are left out for directories.
func (a Identity) sameAs(b Identity) bool {
	if a.Mode.IsDir() && b.Mode.IsDir() {
		return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode
	}
	return a == b
}

type canaryRecord struct {
	canary Canary
	id     Identity
	gone   bool // removed by a check it was handed to
}

var roleRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// CanaryName is the name of the canary of role: conf-<nonce8>-<role>.
func (g *Guard) CanaryName(role string) string { return "conf-" + g.Nonce8() + "-" + role }

// CanaryContent is what a canary file of role holds; it begins with
// "caboose-conformance <nonce>".
func (g *Guard) CanaryContent(role string) []byte {
	return []byte("caboose-conformance " + g.nonce + " " + role + "\n")
}

// MakeCanary makes the canary of role in the directory parent (relative
// to B, slash-separated) from the host side, never following a symlink,
// records the identity of every path in it, and returns it. A role may be
// made once per run; parent must exist, and be under B but not the marker.
func (g *Guard) MakeCanary(parent, role string, kind Kind) (Canary, error) {
	if g == nil || !g.made {
		return Canary{}, refuse("guard", "not an environment this suite created")
	}
	if !roleRe.MatchString(role) {
		return Canary{}, fmt.Errorf("conformance: bad canary role %q", role)
	}
	if parent == "" || path.Clean(parent) != parent || strings.HasPrefix(parent, "/") || parent == ".." || strings.HasPrefix(parent, "../") {
		return Canary{}, fmt.Errorf("conformance: canary parent %q is not a clean relative path", parent)
	}
	d := nofollow.Dir(g.base)
	if fi, err := d.Lstat(parent); err != nil || !fi.IsDir() {
		return Canary{}, fmt.Errorf("conformance: canary parent %s is not a directory", parent)
	}
	name := g.CanaryName(role)
	rel := parent + "/" + name
	c := Canary{Role: role, Kind: kind, Rel: rel}
	g.mu.Lock()
	for _, r := range g.canaries {
		if r.canary.Role == role {
			g.mu.Unlock()
			return Canary{}, fmt.Errorf("conformance: canary %q made twice", role)
		}
	}
	g.mu.Unlock()
	if _, err := d.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
		return Canary{}, fmt.Errorf("conformance: %s already exists", rel)
	}
	content := g.CanaryContent(role)
	switch kind {
	case File:
		if err := d.WriteFile(rel, content, 0o644); err != nil {
			return Canary{}, err
		}
		c.Paths = []string{rel}
	case Dir:
		if err := mkdirNoFollow(g.base, rel); err != nil {
			return Canary{}, err
		}
		c.Paths = []string{rel}
	case Tree:
		c.Paths = []string{rel}
		dirs := []string{rel, rel + "/" + name + "-a", rel + "/" + name + "-a/" + name + "-b"}
		for i, dir := range dirs {
			if err := mkdirNoFollow(g.base, dir); err != nil {
				return Canary{}, err
			}
			if i > 0 {
				c.Paths = append(c.Paths, dir)
			}
			for j := 1; j <= 2; j++ {
				f := fmt.Sprintf("%s/%s-%d", dir, name, 2*i+j)
				if err := d.WriteFile(f, content, 0o644); err != nil {
					return Canary{}, err
				}
				c.Paths = append(c.Paths, f)
			}
		}
	default:
		return Canary{}, fmt.Errorf("conformance: unknown canary kind %d", kind)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range c.Paths {
		id, err := identityOf(d, p)
		if err != nil {
			return Canary{}, err
		}
		g.canaries[p] = canaryRecord{canary: c, id: id}
	}
	return c, nil
}

// IsCanary reports whether rel (relative to B) is a path of a canary.
func (g *Guard) IsCanary(rel string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.canaries[rel]
	return ok
}

// CanaryIdentity is the identity recorded when the canary path rel was made.
func (g *Guard) CanaryIdentity(rel string) (Identity, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.canaries[rel]
	return r.id, ok
}

// Canaries are every canary made, each once, sorted by Rel.
func (g *Guard) Canaries() []Canary {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Canary
	for p, r := range g.canaries {
		if p == r.canary.Rel {
			out = append(out, r.canary)
		}
	}
	slices.SortFunc(out, func(a, b Canary) int { return strings.Compare(a.Rel, b.Rel) })
	return out
}

// CanaryRecord is what the guard last recorded of the canary path rel:
// its identity, and whether a check it was handed to removed it. ok is
// false for a path that is no canary's.
func (g *Guard) CanaryRecord(rel string) (id Identity, gone, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.canaries[rel]
	return r.id, r.gone, ok
}

// canaryPaths are every canary path, sorted.
func (g *Guard) canaryPaths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for p := range g.canaries {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// maxHashed bounds the size of a file the suite hashes.
const maxHashed = 64 << 20

// identityOf reads rel's identity through d, never following a symlink.
func identityOf(d nofollow.Dir, rel string) (Identity, error) {
	fi, err := d.Lstat(rel)
	if err != nil {
		return Identity{}, err
	}
	dev, ino, nlink, ok := sysIdentity(fi)
	if !ok {
		return Identity{}, fmt.Errorf("conformance: %s: no device and inode", rel)
	}
	id := Identity{Dev: dev, Ino: ino, Nlink: nlink, Mode: fi.Mode(), Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	switch {
	case fi.Mode().IsRegular():
		if fi.Mode().Perm()&0o400 == 0 {
			// Unreadable to its owner: compared without its contents.
			id.Hash = "unreadable"
			break
		}
		b, _, err := d.ReadFileMax(rel, maxHashed)
		if errors.Is(err, nofollow.ErrNotPlain) {
			// A hard-linked file: compared by its link count and the rest.
			id.Hash = "not plain"
			break
		}
		if err != nil {
			return Identity{}, fmt.Errorf("conformance: hashing %s: %w", rel, err)
		}
		id.Hash = hashOf(b)
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := readlink(string(d), rel)
		if err != nil {
			return Identity{}, err
		}
		id.Hash = hashOf([]byte(t))
	}
	return id, nil
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// readlink reads the symlink rel under base: its parent reached through
// os.Root, which never leaves base.
func readlink(base, rel string) (string, error) {
	r, err := os.OpenRoot(base)
	if err != nil {
		return "", err
	}
	defer r.Close()
	return r.Readlink(rel)
}

// mkdirNoFollow makes the directory rel under base (and the missing ones
// above it), walking one plain directory at a time and never through a
// symlink; an existing directory at rel is not an error.
func mkdirNoFollow(base, rel string) error {
	r, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { r.Close() }()
	els := strings.Split(rel, "/")
	for i, el := range els {
		if el == "" || el == "." || el == ".." {
			return fmt.Errorf("conformance: %q is not a clean relative path", rel)
		}
		fi, err := r.Lstat(el)
		if errors.Is(err, fs.ErrNotExist) {
			if err = r.Mkdir(el, 0o755); err != nil {
				return err
			}
			fi, err = r.Lstat(el)
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("conformance: %s is not a plain directory", strings.Join(els[:i+1], "/"))
		}
		sub, err := r.OpenRoot(el)
		if err != nil {
			return err
		}
		if st, err := sub.Stat("."); err != nil || !os.SameFile(fi, st) {
			sub.Close()
			return fmt.Errorf("conformance: %s changed while it was opened", strings.Join(els[:i+1], "/"))
		}
		r.Close()
		r = sub
	}
	return nil
}
