package statesync

import (
	"bytes"
	"errors"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// File is one file as the sync repo holds it.
type File struct {
	Data []byte
	Exec bool
}

// Export is what the live home holds of what syncs.
type Export struct {
	// Files are the repo paths to write, with their contents.
	Files map[string]File
	// Keep are repo paths to leave as the repo has them: their live source
	// exists but could not be read as it should (a JSON file mid-write,
	// a symlink), and must not read as deleted. A path ending in "/" keeps
	// everything under it.
	Keep map[string]bool
	// Refused are home paths that would have synced but are not plain
	// files and directories: symlinks, hard links (see internal/nofollow).
	// They are never followed; what the repo has of them is kept.
	Refused []string
	// Shadowed are sync rules that matched files, but only files an
	// earlier rule of their entry took: first match wins, so they never
	// decide anything. A rule written in the wrong order, most likely.
	Shadowed []string
}

// kept reports whether repo path p is to be left as the repo has it.
func (e *Export) kept(p string) bool {
	if e.Keep[p] {
		return true
	}
	for k := range e.Keep {
		if strings.HasSuffix(k, "/") && strings.HasPrefix(p, k) {
			return true
		}
	}
	return false
}

// refuse records home path rel, synced as repo path repoPath, as not
// plain.
func (e *Export) refuse(rel, repoPath string) {
	e.Refused = append(e.Refused, rel)
	e.Keep[repoPath] = true
}

// ExportLive reads what c's rules sync out of home dir. Anything in the
// sandbox writes the whole home, so nothing there is trusted to be what its
// name says: a symlink or a hard link is refused, never followed, anywhere on a
// path -- one to .credentials.json would otherwise export the Claude login.
func ExportLive(dir string, c *sandboxcfg.Config) (*Export, error) {
	e := &Export{Files: map[string]File{}, Keep: map[string]bool{}}
	d := nofollow.Dir(dir)
	first, later := map[*sandboxcfg.Rule]bool{}, map[*sandboxcfg.Rule]bool{}
	for i := range c.Keep {
		k := &c.Keep[i]
		if len(k.Rules) == 0 {
			continue
		}
		if k.File {
			if r, ok := c.Match(k.Rel); ok {
				if err := e.addFile(d, k.Rel, RepoHome+k.Rel, r); err != nil {
					return nil, err
				}
			}
			continue
		}
		err := d.Walk(k.Rel, func(p string, ent fs.DirEntry) error {
			rel := p
			repoPath := RepoHome + rel
			switch {
			case ent.Name() == ".DS_Store" || (ent.IsDir() && ent.Name() == ".git"):
				if ent.IsDir() {
					return fs.SkipDir
				}
				return nil
			case ent.IsDir():
				if !c.Reaches(rel) {
					return fs.SkipDir
				}
				return nil
			case !safeRel(repoPath):
				return nil
			case ent.Type()&fs.ModeSymlink != 0:
				if c.Reaches(rel) {
					// A file or a directory: whatever the repo has of either stays.
					e.refuse(p, repoPath)
					e.Keep[repoPath+"/"] = true
				}
				return nil
			case !ent.Type().IsRegular():
				return nil // a socket, a FIFO: never synced
			}
			r, ok := c.Match(rel)
			if !ok {
				return nil
			}
			first[r] = true
			for j := range k.Rules {
				if o := &k.Rules[j]; o != r && o.Matches(rel) {
					later[o] = true
				}
			}
			return e.addFile(d, p, repoPath, r)
		})
		if err != nil {
			return nil, err
		}
	}
	for r := range later {
		if !first[r] {
			e.Shadowed = append(e.Shadowed, r.Path)
		}
	}
	sort.Strings(e.Refused)
	sort.Strings(e.Shadowed)
	return e, nil
}

// addFile exports the file at home path rel as repoPath, as rule r
// says: with Keys, only those keys of it. A JSON file that does not parse
// is kept as the repo has it instead, since its program may be halfway
// through rewriting it; one with none of the keys has nothing to export,
// and the repo's copy reads as deleted.
func (e *Export) addFile(d nofollow.Dir, rel, repoPath string, r *sandboxcfg.Rule) error {
	data, mode, err := d.ReadFile(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case errors.Is(err, nofollow.ErrNotPlain):
		e.refuse(rel, repoPath)
		return nil
	case err != nil:
		return err
	}
	if r.Merge != sandboxcfg.MergeJSON || len(r.Keys) == 0 {
		e.Files[repoPath] = File{Data: data, Exec: mode&0o111 != 0}
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	v, err := decode(data)
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		e.Keep[repoPath] = true
		return nil
	}
	picked := map[string]any{}
	for _, k := range r.Keys {
		if v, ok := m[k]; ok {
			picked[k] = v
		}
	}
	if len(picked) == 0 {
		return nil
	}
	out, err := Encode(picked)
	if err != nil {
		return err
	}
	e.Files[repoPath] = File{Data: out}
	return nil
}

// secretPatterns match credentials that must never be committed, whatever
// the allowlist lets through: Anthropic keys and OAuth tokens, GitHub
// tokens, AWS access keys, private keys, and a JSON accessToken or
// refreshToken field such as .credentials.json holds.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9]+-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`"(accessToken|refreshToken)"\s*:\s*"[^"]{16,}"`),
}

// SecretsError names exported files that look like they hold a credential.
type SecretsError struct{ Paths []string }

func (e *SecretsError) Error() string {
	return "refusing to sync; these look like they hold a credential: " +
		strings.Join(e.Paths, ", ") + " (remove it and sync again; nothing was committed)"
}

// ScanSecrets refuses an export holding anything secretPatterns match.
func (e *Export) ScanSecrets() error {
	var hits []string
	for p, f := range e.Files {
		for _, re := range secretPatterns {
			if re.Match(f.Data) {
				hits = append(hits, p)
				break
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Strings(hits)
	return &SecretsError{Paths: hits}
}
