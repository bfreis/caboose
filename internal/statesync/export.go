package statesync

import (
	"bytes"
	"errors"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/bfreis/caboose/internal/nofollow"
)

// File is one file as the sync repo holds it.
type File struct {
	Data []byte
	Exec bool
}

// Export is what the live data dir holds of the allowlist.
type Export struct {
	// Files are the repo paths to write, with their contents.
	Files map[string]File
	// Keep are repo paths to leave as the repo has them: their live source
	// exists but could not be read as it should (a .claude.json mid-write,
	// a symlink), and must not read as deleted. A path ending in "/" keeps
	// everything under it.
	Keep map[string]bool
	// Unsynced are project keys with memory that are not under /work, so do
	// not sync.
	Unsynced []string
	// Refused are data dir paths that would have synced but are not plain
	// files and directories: symlinks, hard links (see internal/nofollow).
	// They are never followed; what the repo has of them is kept.
	Refused []string
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

// refuse records data dir path rel, synced as repo path repoPath, as not
// plain.
func (e *Export) refuse(rel, repoPath string) {
	e.Refused = append(e.Refused, rel)
	e.Keep[repoPath] = true
}

// ExportLive reads the allowlist out of data dir dir. The container writes
// .claude, so nothing there is trusted to be what its name says: a symlink
// or a hard link is refused, never followed, anywhere on a path -- one to
// .credentials.json would otherwise export the Claude login.
func ExportLive(dir string) (*Export, error) {
	e := &Export{Files: map[string]File{}, Keep: map[string]bool{}}
	d := nofollow.Dir(dir)

	if err := e.addFile(d, ".claude/settings.json", repoSettings); err != nil {
		return nil, err
	}
	for _, t := range TreeDirs {
		skip := func(rel string) bool { return !synced(t + "/" + rel) }
		if err := e.addTree(d, ".claude/"+t, repoClaude+t+"/", skip); err != nil {
			return nil, err
		}
	}

	projects, err := d.ReadDir(".claude/projects")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		mem := ".claude/projects/" + p.Name() + "/memory"
		fi, err := d.Lstat(mem)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !SyncedKey(p.Name()) {
			e.Unsynced = append(e.Unsynced, p.Name())
			continue
		}
		repoMem := repoProjects + p.Name() + "/memory/"
		if !fi.IsDir() {
			e.refuse(mem, repoMem)
			continue
		}
		if err := e.addTree(d, mem, repoMem, nil); err != nil {
			return nil, err
		}
	}

	if err := e.addClaudeJSON(d, ".claude.json"); err != nil {
		return nil, err
	}
	sort.Strings(e.Refused)
	return e, nil
}

func (e *Export) addFile(d nofollow.Dir, rel, repoPath string) error {
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
	e.Files[repoPath] = File{Data: data, Exec: mode&0o111 != 0}
	return nil
}

// addTree exports every regular file under the directory rel as prefix +
// its path under rel. skip, when set, prunes a directory by that path.
func (e *Export) addTree(d nofollow.Dir, rel, prefix string, skip func(sub string) bool) error {
	return d.Walk(rel, func(p string, ent fs.DirEntry) error {
		sub := strings.TrimPrefix(p, rel+"/")
		repoPath := prefix + sub
		switch {
		case ent.Name() == ".DS_Store" || (ent.IsDir() && ent.Name() == ".git"):
			if ent.IsDir() {
				return fs.SkipDir
			}
			return nil
		case ent.IsDir():
			if skip != nil && skip(sub) {
				return fs.SkipDir
			}
			return nil
		case !safeRel(repoPath):
			return nil
		case ent.Type()&fs.ModeSymlink != 0:
			// A file or a directory: whatever the repo has of either stays.
			e.refuse(p, repoPath)
			e.Keep[repoPath+"/"] = true
			return nil
		case !ent.Type().IsRegular():
			return nil // a socket, a FIFO: never synced
		}
		return e.addFile(d, p, repoPath)
	})
}

// addClaudeJSON exports ClaudeJSONKeys of .claude.json. With none of them
// set there is nothing to export, and the repo's copy reads as deleted;
// a file that does not parse is kept as the repo has it instead, since
// Claude Code may be halfway through rewriting it.
func (e *Export) addClaudeJSON(d nofollow.Dir, rel string) error {
	data, _, err := d.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if errors.Is(err, nofollow.ErrNotPlain) {
		e.refuse(rel, RepoClaudeJSON)
		return nil
	}
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	v, err := decode(data)
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		e.Keep[RepoClaudeJSON] = true
		return nil
	}
	picked := map[string]any{}
	for _, k := range ClaudeJSONKeys {
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
	e.Files[RepoClaudeJSON] = File{Data: out}
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
