package statesync

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// Dir is the sync repo, relative to the data dir.
const Dir = datadir.SyncDir

// ContainerDir is where the container mounts the sync repo, and so the path
// the git that runs it -- the container's -- sees it at.
const ContainerDir = "/home/agent/.caboose-sync"

// LockFile is the lock, in the data dir, that one sync holds throughout.
const LockFile = "sync.lock"

// Branch is the remote branch every machine syncs through.
const Branch = "main"

// baseAttributes start the sync repo's merge rules, in .git/info/attributes.
// That file outranks any .gitattributes, and a remote can send one of
// those: it is not synced (nothing no rule names is), but it sits in the
// repo, and git would obey it -- running filter, merge or diff drivers the
// user's own git config defines (git-lfs, say), converting line endings,
// or changing how memory merges. So the first line unsets, for every path,
// each attribute that can change content or run a program, and the rules'
// own follow it (sandboxcfg.Config.GitAttributes): a JSON file is never
// merged as text -- two machines' edits are merged key by key (MergeJSON),
// and `binary` leaves both sides for that -- and a union file keeps the
// lines each side added rather than conflicting at its end.
const baseAttributes = "* -filter -text -eol -ident -working-tree-encoding -diff merge=text\n"

// rulesFile, in the sync repo's .git, is the sandbox config the last
// successful sync ran with: a repo path its rules did not name, and this
// machine's do now, is one this machine has just started syncing, whose
// file is not here yet -- to be taken from the repo, not read as deleted.
const rulesFile = ".git/caboose-rules.toml"

var (
	// ErrNoRemote is returned when the sync repo has no remote yet.
	ErrNoRemote = errors.New("no sync remote is set")
	// ErrAborted is what a Resolver returns to stop the merge; live state
	// is left as it was.
	ErrAborted = errors.New("sync aborted")
	// ErrLocked is returned while another sync holds the lock.
	ErrLocked = errors.New("another caboose is syncing this data dir")
	// ErrSandboxConfig is returned when the sandbox config cannot be read
	// as it is: a sync with the rules of an older copy could undo what the
	// new one says, on every machine.
	ErrSandboxConfig = errors.New("the sandbox config cannot be used")
)

// Conflict is a file both machines changed in ways that could not be merged.
// A nil side is one where the file does not exist.
type Conflict struct {
	Path               string
	Base, Ours, Theirs []byte
	// JSON is set for a file its rule merges as JSON: what is written
	// to settle it has to parse.
	JSON bool
	// Keys are the JSON keys in conflict; nil for a text file, or a JSON
	// one that did not parse (Note says why).
	Keys []string
	Note string

	s *Syncer
}

// Resolution settles one Conflict: the file's new content, or its deletion.
type Resolution struct {
	Content []byte
	Delete  bool
}

// Take settles c in favour of one side. For JSON only the conflicting keys
// are taken from that side; everything merged cleanly stays merged.
func (c Conflict) Take(s Side) Resolution {
	if c.Keys != nil {
		if out, _, err := MergeJSON(c.Base, c.Ours, c.Theirs, s); err == nil {
			return Resolution{Content: out}
		}
	}
	data := c.Ours
	if s == Theirs {
		data = c.Theirs
	}
	return Resolution{Content: data, Delete: data == nil}
}

// Markers is c as a file with diff3 conflict markers, to edit by hand. The
// three sides are written inside the repo's .git, the one place both this
// process and the git it runs (in a container, say) can see.
func (c Conflict) Markers() ([]byte, error) {
	if c.s == nil {
		return nil, errors.New("conflict has no repo")
	}
	const tmp = ".git/caboose-merge"
	repo := c.s.repo()
	var names []string
	defer func() {
		for _, n := range names {
			_ = repo.Remove(n)
		}
		_ = repo.Remove(tmp)
	}()
	for i, data := range [][]byte{c.Ours, c.Base, c.Theirs} {
		n := tmp + "/" + fmt.Sprint(i) // relative to git -C, too
		if err := repo.WriteFile(n, data, 0o600); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	out, err := c.s.git().out(append([]string{"merge-file", "-p", "--diff3",
		"-L", "this machine", "-L", "last sync", "-L", "remote"}, names...)...)
	// merge-file exits with the number of conflicts; only a negative
	// status (255 here) is a failure.
	if code := exitCode(err); err != nil && (code < 0 || code > 127) {
		return nil, err
	}
	return out, nil
}

// HasMarkers reports whether data still holds conflict markers.
func HasMarkers(data []byte) bool {
	return markerRE.Match(data)
}

var markerRE = regexp.MustCompile(`(?m)^(<<<<<<<|>>>>>>>|\|\|\|\|\|\|\|)( |$)`)

// Resolver settles a conflict, or returns ErrAborted.
type Resolver func(Conflict) (Resolution, error)

// Syncer syncs one data dir.
type Syncer struct {
	DataDir string
	// Host names this machine in commits.
	Host string
	// Resolve settles conflicts MergeJSON cannot; nil aborts on the first.
	Resolve Resolver
	// Stderr shows fetch and push output (and any credential prompt);
	// nil keeps it quiet.
	Stderr *os.File
	// Git runs git; nil is the host's (LocalGit). GitDir is the repo's path
	// as that git sees it, "" for RepoDir.
	Git    Command
	GitDir string
	// Sandbox is the sandbox config whose rules say what syncs; nil reads
	// the data dir's (datadir.LoadSandboxConfig).
	Sandbox *datadir.Sandbox
	// Roots are the container paths of this machine's roots: the roots
	// of the defaults in effect while there is no sandbox config yet.
	Roots []string
	// Defaults, when set, is written as the sandbox config when a sync
	// that took what the remote has still leaves none: no machine had one
	// to bring. It goes out in the same sync.
	Defaults []byte

	g *git
}

// sandbox is the sandbox config in effect, read once.
func (s *Syncer) sandbox() (*datadir.Sandbox, error) {
	if s.Sandbox == nil {
		sb, err := datadir.LoadSandboxConfig(s.DataDir, s.Roots)
		if err != nil {
			return nil, err
		}
		s.Sandbox = sb
	}
	return s.Sandbox, nil
}

// rules is the sandbox config's rules, when it can be used as it is.
func (s *Syncer) rules() (*sandboxcfg.Config, error) {
	sb, err := s.sandbox()
	if err != nil {
		return nil, err
	}
	if sb.Err != nil {
		return nil, fmt.Errorf("%w: %s %v; fix it (or 'caboose update', for a newer format) and sync again", ErrSandboxConfig, sandboxcfg.HomePath, sb.Err)
	}
	return sb.Config, nil
}

// prevRules is the sandbox config the last successful sync ran with, nil
// when there is none to read.
func (s *Syncer) prevRules() *sandboxcfg.Config {
	data, _, err := s.repo().ReadFile(rulesFile)
	if err != nil {
		return nil
	}
	c, err := sandboxcfg.Parse(data)
	if err != nil {
		return nil
	}
	return c
}

// Report is what a sync did.
type Report struct {
	// Committed is set when this machine had changes since the last sync.
	Committed bool
	// Merged is set when the remote had changes this machine took.
	Merged bool
	// Applied are the data dir paths the sync wrote or deleted.
	Applied []string
	// Ignored are repo paths this machine does not sync (another
	// machine's rules, a newer caboose's, or not valid here); they are
	// left in the repo untouched.
	Ignored []string
	// SandboxConfig is set when the sync changed the sandbox config: a
	// keep entry it added takes effect at the next restart.
	SandboxConfig bool
	// Shadowed are rules that never decide anything (Export.Shadowed).
	Shadowed []string
	// Refused are data dir paths not synced for not being plain files and
	// directories (Export.Refused), both ways: never read, never written.
	Refused []string
	// Pushed is set when the remote was updated.
	Pushed bool
}

// RepoDir is the sync repo's path.
func (s *Syncer) RepoDir() string { return filepath.Join(s.DataDir, Dir) }

// repo and live are the sync repo and the data dir, as this process touches
// them: the container writes both (the repo is all its git's; .claude is
// its own), so never through a symlink.
func (s *Syncer) repo() nofollow.Dir { return nofollow.Dir(s.RepoDir()) }
func (s *Syncer) live() nofollow.Dir { return nofollow.Dir(s.DataDir) }

// inRepo reports whether rel exists in the sync repo, as itself.
func (s *Syncer) inRepo(rel string) (bool, error) {
	_, err := s.repo().Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *Syncer) git() *git {
	if s.g == nil {
		dir := s.GitDir
		if dir == "" {
			dir = s.RepoDir()
		}
		s.g = &git{dir: dir, command: s.Git, Stderr: s.Stderr}
	}
	return s.g
}

// Lock takes the sync lock without waiting.
func (s *Syncer) Lock() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(s.DataDir, LockFile), os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if err == syscall.EWOULDBLOCK {
		f.Close()
		return nil, ErrLocked
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Init creates the sync repo if there is none, and (re)writes its local
// settings. A new repo starts with one empty commit: the "last sync" a
// first export is compared with, so a new machine's files read as added,
// not as the remote's deleted.
func (s *Syncer) Init() error {
	g := s.git()
	// The repo's own dir is the data dir's, and the container's mount
	// point: nothing inside can replace it.
	if err := os.MkdirAll(s.RepoDir(), 0o700); err != nil {
		return err
	}
	if has, err := s.inRepo(".git"); err != nil {
		return err
	} else if !has {
		if err := g.run("init", "-q", "-b", Branch); err != nil {
			return err
		}
	}
	host := hostLabel(s.Host)
	for k, v := range map[string]string{
		"user.name":  "caboose on " + host,
		"user.email": "caboose@" + host,
	} {
		if err := g.run("config", k, v); err != nil {
			return err
		}
	}
	attrs := baseAttributes
	if c, err := s.rules(); err == nil {
		attrs += c.GitAttributes(RepoHome)
	}
	if err := s.repo().WriteFile(".git/info/attributes", []byte(attrs), 0o644); err != nil {
		return err
	}
	if has, _ := g.ok("rev-parse", "-q", "--verify", "HEAD"); !has {
		return g.run("commit", "-q", "--allow-empty", "-m", "Start syncing on "+host)
	}
	return nil
}

var hostRE = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func hostLabel(h string) string {
	h = hostRE.ReplaceAllString(h, "-")
	if h == "" {
		return "unknown-host"
	}
	return h
}

// SetRemote points the sync repo at url.
func (s *Syncer) SetRemote(url string) error {
	g := s.git()
	if _, err := g.str("remote", "get-url", "origin"); err == nil {
		return g.run("remote", "set-url", "origin", url)
	}
	return g.run("remote", "add", "origin", url)
}

// MaybeRemote reports whether the sync repo's git config names the remote,
// without running git: cheap enough to ask on every launch. It can be
// fooled -- the container writes the repo -- so Remote is the answer; a
// false here is only ever "no remote".
func (s *Syncer) MaybeRemote() bool {
	data, _, err := s.repo().ReadFile(".git/config")
	return err == nil && bytes.Contains(data, []byte(`[remote "origin"]`))
}

// RemoteHint is the remote's URL as the repo's git config spells it, read
// without git, for showing: like MaybeRemote it can be fooled, and is no
// input to anything. "" when there is none to read.
func (s *Syncer) RemoteHint() string {
	data, _, err := s.repo().ReadFile(".git/config")
	if err != nil {
		return ""
	}
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = line == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(line, "="); in && ok && strings.TrimSpace(k) == "url" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Remote is the sync repo's remote URL, or "" when there is none.
func (s *Syncer) Remote() string {
	if has, _ := s.inRepo(".git"); !has {
		return ""
	}
	url, err := s.git().str("remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return url
}

// Sync runs one sync: export, commit, fetch, merge, apply, push. The caller
// holds the lock and has made sure nothing is writing the data dir.
func (s *Syncer) Sync() (*Report, error) {
	if err := s.Init(); err != nil {
		return nil, err
	}
	if s.Remote() == "" {
		return nil, ErrNoRemote
	}
	if _, err := s.rules(); err != nil {
		return nil, err
	}
	r := &Report{}
	if err := s.clean(); err != nil {
		return nil, err
	}
	if err := s.commitExport(r); err != nil {
		return nil, err
	}
	// A push that lost a race with another machine's is rejected, and
	// then the remote has something new to merge: go round again. Any
	// other failure (auth, network) is reported as it is, not retried.
	for attempt := 0; ; attempt++ {
		if err := s.pull(r); err != nil {
			return r, err
		}
		if err := s.adopt(r); err != nil {
			return r, err
		}
		if err := s.writeDefaults(r); err != nil {
			return r, err
		}
		err := s.push(r)
		if err == nil {
			return r, s.saveRules()
		}
		if attempt == 2 || !s.remoteMoved() {
			return r, err
		}
	}
}

// writeDefaults writes Defaults as the sandbox config when there is still
// none, and commits it.
func (s *Syncer) writeDefaults(r *Report) error {
	if s.Defaults == nil {
		return nil
	}
	if _, err := s.live().Lstat(datadir.SandboxConfig); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := s.live().WriteFile(datadir.SandboxConfig, s.Defaults, 0o644); err != nil {
		return err
	}
	return s.commitExport(r)
}

// saveRules records the sandbox config this sync ran with (rulesFile).
func (s *Syncer) saveRules() error {
	sb, err := s.sandbox()
	if err != nil {
		return err
	}
	return s.repo().WriteFile(rulesFile, sb.Data, 0o644)
}

// remoteMoved fetches, and reports whether the remote branch now has
// commits HEAD lacks.
func (s *Syncer) remoteMoved() bool {
	g := s.git()
	if g.talk("fetch", "-q", "origin") != nil {
		return false
	}
	have, err := g.ok("merge-base", "--is-ancestor", "refs/remotes/origin/"+Branch, "HEAD")
	return err == nil && !have
}

// clean puts the work tree back at HEAD, finishing off a sync that died
// halfway. Nothing live is in it: the export is redone from the data dir.
func (s *Syncer) clean() error {
	g := s.git()
	if merging, err := s.inRepo(".git/MERGE_HEAD"); err != nil {
		return err
	} else if merging {
		if err := g.run("merge", "--abort"); err != nil {
			return err
		}
	}
	if err := g.run("reset", "-q", "--hard", "HEAD"); err != nil {
		return err
	}
	return g.run("clean", "-q", "-fdx")
}

// commitExport mirrors what syncs into the work tree and commits it.
func (s *Syncer) commitExport(r *Report) error {
	c, err := s.rules()
	if err != nil {
		return err
	}
	e, err := ExportLive(s.DataDir, c)
	if err != nil {
		return err
	}
	r.Refused, r.Shadowed = e.Refused, e.Shadowed
	if err := e.ScanSecrets(); err != nil {
		return err
	}
	g := s.git()
	tracked, err := g.out("ls-files", "-z")
	if err != nil {
		return err
	}
	prev := s.prevRules()
	for _, p := range zsplit(tracked) {
		if _, ok := e.Files[p]; ok || e.kept(p) {
			continue
		}
		// Only paths this machine synced last time too can read as
		// deleted here; the rest are another machine's, a newer
		// caboose's, or new to this one (adopt takes those).
		if !deletable(c, prev, p) {
			continue
		}
		if err := s.repo().Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for p, f := range e.Files {
		if err := s.repo().WriteFile(p, f.Data, fileMode(f.Exec)); err != nil {
			return err
		}
	}
	if err := g.run("add", "-A"); err != nil {
		return err
	}
	same, err := g.ok("diff", "--cached", "--quiet", "--no-ext-diff", "--no-textconv")
	if err != nil || same {
		return err
	}
	r.Committed = true
	return g.run("commit", "-q", "-m", "Sync from "+hostLabel(s.Host))
}

// deletable reports whether repo path p, whose live file is gone, reads as
// deleted: this machine syncs it now (c), and did at its last sync (prev).
// One it has only just started syncing is on its way in, not out.
func deletable(c, prev *sandboxcfg.Config, p string) bool {
	if _, err := LiveTarget(c, p); err != nil {
		return false
	}
	if prev == nil {
		return false
	}
	_, err := LiveTarget(prev, p)
	return err == nil
}

func fileMode(exec bool) os.FileMode {
	if exec {
		return 0o755
	}
	return 0o644
}

// pull fetches the remote and merges it in, then applies what changed.
func (s *Syncer) pull(r *Report) error {
	g := s.git()
	if err := g.talk("fetch", "-q", "origin"); err != nil {
		return err
	}
	remote := "refs/remotes/origin/" + Branch
	if has, _ := g.ok("rev-parse", "-q", "--verify", remote); !has {
		return nil // an empty remote: the push creates the branch
	}
	if have, err := g.ok("merge-base", "--is-ancestor", remote, "HEAD"); err != nil || have {
		return err
	}
	old, err := g.str("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if ff, err := g.ok("merge-base", "--is-ancestor", "HEAD", remote); err != nil {
		return err
	} else if ff {
		if err := g.run("merge", "-q", "--ff-only", remote); err != nil {
			return err
		}
	} else if err := s.merge(remote); err != nil {
		return err
	}
	r.Merged = true
	return s.applyAll(old, r)
}

// applyAll applies what changed since old; and when that changed the
// sandbox config itself, applies it again under the new rules, which may
// sync what the old ones left in the repo.
func (s *Syncer) applyAll(old string, r *Report) error {
	if err := s.apply(old, r); err != nil {
		return err
	}
	if !slices.Contains(r.Applied, datadir.SandboxConfig) {
		return nil
	}
	r.SandboxConfig = true
	sb, err := datadir.LoadSandboxConfig(s.DataDir, s.Roots)
	if err != nil || sb.Err != nil {
		// Kept by the old rules: doctor and the next launch say why.
		return nil
	}
	s.Sandbox, r.Ignored = sb, nil
	return s.apply(old, r)
}

// adopt takes from the repo each file this machine has only just started
// syncing -- its rules name it, the last sync's did not -- that it does not
// have: those are other machines' files, not ones deleted here.
func (s *Syncer) adopt(r *Report) error {
	c, err := s.rules()
	if err != nil {
		return err
	}
	prev := s.prevRules()
	modes, err := s.modes()
	if err != nil {
		return err
	}
	for _, p := range slices.Sorted(maps.Keys(modes)) {
		t, err := LiveTarget(c, p)
		if err != nil || modes[p] != "100644" && modes[p] != "100755" {
			continue
		}
		if prev != nil {
			if _, err := LiveTarget(prev, p); err == nil {
				continue
			}
		}
		if _, err := s.live().Lstat(t.Rel); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		data, err := s.git().out("cat-file", "blob", "HEAD:"+p)
		if err != nil {
			return err
		}
		switch err := s.write(t, data, false, modes[p] == "100755"); {
		case errors.Is(err, nofollow.ErrNotPlain):
			r.Refused = appendNew(r.Refused, t.Rel)
		case err != nil:
			return fmt.Errorf("applying %s: %w", p, err)
		default:
			r.Applied = appendNew(r.Applied, t.Rel)
		}
	}
	return nil
}

// merge merges the remote into HEAD, settling conflicts. On ErrAborted (or
// any error) the merge is undone and HEAD is left at this machine's export.
func (s *Syncer) merge(remote string) (err error) {
	g := s.git()
	// Exit 1 is "stopped with conflicts"; MERGE_HEAD tells it from failure.
	mergeErr := g.run("merge", "-q", "--no-ff", "--no-commit", "--allow-unrelated-histories", remote)
	if merging, _ := s.inRepo(".git/MERGE_HEAD"); !merging {
		if mergeErr != nil {
			return mergeErr
		}
		return fmt.Errorf("git merge left no merge in progress")
	}
	defer func() {
		if err != nil {
			_ = g.run("merge", "--abort")
		}
	}()
	out, err := g.out("diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return err
	}
	for _, p := range zsplit(out) {
		if err := s.settle(p); err != nil {
			return err
		}
	}
	return g.run("commit", "-q", "--no-edit", "-m", "Merge on "+hostLabel(s.Host))
}

// settle resolves one conflicted path: JSON by key if it merges cleanly,
// anything else through the Resolver.
func (s *Syncer) settle(p string) error {
	c, err := s.conflict(p)
	if err != nil {
		return err
	}
	var res Resolution
	if rules, err := s.rules(); err == nil {
		if t, err := LiveTarget(rules, p); err == nil {
			c.JSON = t.Rule.Merge == sandboxcfg.MergeJSON
		}
	}
	if c.JSON && c.Ours != nil && c.Theirs != nil {
		merged, keys, err := MergeJSON(c.Base, c.Ours, c.Theirs, NoSide)
		switch {
		case err != nil:
			c.Note = "not valid JSON on both sides: " + err.Error()
		case keys == nil:
			return s.resolve(p, Resolution{Content: merged})
		default:
			c.Keys = keys
		}
	}
	if s.Resolve == nil {
		return fmt.Errorf("%w: %s changed on both sides", ErrAborted, conflictDesc(c))
	}
	res, err = s.Resolve(c)
	if err != nil {
		return err
	}
	return s.resolve(p, res)
}

func conflictDesc(c Conflict) string {
	if len(c.Keys) > 0 {
		return fmt.Sprintf("%s (%s)", c.Path, strings.Join(c.Keys, ", "))
	}
	return c.Path
}

// conflict reads a conflicted path's three stages.
func (s *Syncer) conflict(p string) (Conflict, error) {
	g := s.git()
	out, err := g.out("ls-files", "-u", "-z", "--", p)
	if err != nil {
		return Conflict{}, err
	}
	c := Conflict{Path: p, s: s}
	for _, line := range zsplit(out) {
		meta, _, _ := strings.Cut(line, "\t")
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		blob, err := g.out("cat-file", "blob", f[1])
		if err != nil {
			return Conflict{}, err
		}
		if blob == nil {
			blob = []byte{}
		}
		switch f[2] {
		case "1":
			c.Base = blob
		case "2":
			c.Ours = blob
		case "3":
			c.Theirs = blob
		}
	}
	return c, nil
}

// resolve records a resolution in the index and work tree.
func (s *Syncer) resolve(p string, res Resolution) error {
	g := s.git()
	if res.Delete {
		return g.run("rm", "-q", "-f", "--", p)
	}
	if err := s.repo().WriteFile(p, res.Content, 0o644); err != nil {
		return err
	}
	return g.run("add", "--", p)
}

// apply writes to the data dir what changed in the repo since old, which is
// what the data dir held when this sync exported it.
func (s *Syncer) apply(old string, r *Report) error {
	c, err := s.rules()
	if err != nil {
		return err
	}
	g := s.git()
	out, err := g.out("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-status", "-z", old, "HEAD")
	if err != nil {
		return err
	}
	fields := zsplit(out)
	modes, err := s.modes()
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(fields); i += 2 {
		status, p := fields[i], fields[i+1]
		t, err := LiveTarget(c, p)
		// Only files: a symlink (120000) or a submodule a remote
		// committed is nothing this machine writes.
		if mode := modes[p]; err != nil || (status != "D" && mode != "100644" && mode != "100755") {
			r.Ignored = append(r.Ignored, p)
			continue
		}
		var data []byte
		if status != "D" {
			if data, err = g.out("cat-file", "blob", "HEAD:"+p); err != nil {
				return err
			}
		}
		err = s.write(t, data, status == "D", modes[p] == "100755")
		if errors.Is(err, errKeptFile) {
			r.Ignored = appendNew(r.Ignored, p)
			continue
		}
		if errors.Is(err, nofollow.ErrNotPlain) {
			// Left as it is; the next export keeps the repo's copy
			// rather than reading it as changed.
			r.Refused = appendNew(r.Refused, t.Rel)
			continue
		}
		if err != nil {
			return fmt.Errorf("applying %s: %w", p, err)
		}
		r.Applied = appendNew(r.Applied, t.Rel)
	}
	sort.Strings(r.Ignored)
	sort.Strings(r.Refused)
	return nil
}

// modes maps each path in HEAD to its git mode.
func (s *Syncer) modes() (map[string]string, error) {
	out, err := s.git().out("ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range zsplit(out) {
		meta, p, ok := strings.Cut(line, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 {
			m[p] = f[0]
		}
	}
	return m, nil
}

func appendNew(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// errKeptFile is write's refusal to delete a keep entry that is a file: a
// single-file bind mount, which the container would go on reading, deleted.
// The next export sends it again.
var errKeptFile = errors.New("a kept file is never deleted by a sync")

// write puts one file into the data dir, or deletes it. A path that is, or
// runs through, a symlink or a hard link is nofollow.ErrNotPlain, and left
// alone: a symlink someone made is theirs, and is never written through.
func (s *Syncer) write(t Target, data []byte, del, exec bool) error {
	if t.Keys() {
		return s.writeKeys(t, data, del)
	}
	live := s.live()
	fi, err := live.Lstat(t.Rel)
	switch {
	case err == nil && !fi.Mode().IsRegular():
		return &fs.PathError{Op: "write", Path: t.Rel, Err: nofollow.ErrNotPlain}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	case del && t.InPlace:
		return errKeptFile
	case del:
		if err := live.Remove(t.Rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		s.pruneEmpty(path.Dir(t.Rel), t.Stop)
		return nil
	case t.InPlace && err == nil:
		// A file mount is pinned to its inode: never replaced.
		return live.WriteInPlace(t.Rel, data)
	}
	// Written beside it and renamed over it, so a symlink swapped in since
	// the check is replaced, not followed.
	return live.WriteFile(t.Rel, data, fileMode(exec))
}

// pruneEmpty removes dir and its empty parents, up to but not including
// stop (Target.Stop), all relative to the data dir.
func (s *Syncer) pruneEmpty(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop+"/") {
		if s.live().Remove(dir) != nil {
			return
		}
		dir = path.Dir(dir)
	}
}

// writeKeys merges the synced keys of a JSON file (t.Rule.Keys) into the
// live one: every key not synced is this machine's alone. In place for a
// kept file, a single-file bind mount that a rename over would break.
func (s *Syncer) writeKeys(t Target, data []byte, del bool) error {
	cur, _, err := s.live().ReadFile(t.Rel)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	live := map[string]any{}
	if len(bytes.TrimSpace(cur)) > 0 {
		v, err := decode(cur)
		m, ok := v.(map[string]any)
		if err != nil || !ok {
			return fmt.Errorf("%s does not parse as a JSON object; left as it is", t.Rel)
		}
		live = m
	}
	synced := map[string]any{}
	if !del {
		v, err := decode(data)
		m, ok := v.(map[string]any)
		if err != nil || !ok {
			return fmt.Errorf("the synced ~/%s is not a JSON object", t.Home)
		}
		synced = m
	}
	for _, k := range t.Rule.Keys {
		if v, ok := synced[k]; ok {
			live[k] = v
		} else {
			delete(live, k)
		}
	}
	out, err := Encode(live)
	if err != nil {
		return err
	}
	if bytes.Equal(out, cur) {
		return nil
	}
	if missing || !t.InPlace {
		return s.live().WriteFile(t.Rel, out, 0o644)
	}
	return s.live().WriteInPlace(t.Rel, out)
}

// push sends HEAD to the remote branch.
func (s *Syncer) push(r *Report) error {
	g := s.git()
	remote := "refs/remotes/origin/" + Branch
	if has, _ := g.ok("rev-parse", "-q", "--verify", remote); has {
		if same, _ := g.ok("merge-base", "--is-ancestor", "HEAD", remote); same {
			return nil // the remote has it all already
		}
	}
	if err := g.talk("push", "-q", "origin", "HEAD:refs/heads/"+Branch); err != nil {
		return err
	}
	r.Pushed = true
	return nil
}
