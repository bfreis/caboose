// Package datadir prepares the host-side data dir that the container
// bind-mounts piece by piece: its layout, the sandbox's git config and
// caboose's instructions and skills for the sandbox's sessions, Claude Code's
// managed CLAUDE.md and skills (ManagedDir).
//
// What the sandbox keeps of its home is under home/, at its path under ~:
// home/.claude is ~/.claude, home/.config/git is ~/.config/git. Which of
// it is kept, and so mounted, is the sandbox config's [[keep]] entries
// (internal/sandboxcfg); caboose's own machinery -- Claude Code's binaries
// (local/), the sync repo, the proposals -- sits beside home/.
//
// Configuration that a tool rewrites is mounted as a DIRECTORY, never as a
// single file. git, jj and gh all save by writing a temp file and renaming
// it over the original, and a rename onto a single-file bind mount fails
// inside the container with EBUSY (`gh auth login`: "could not write config
// file /home/agent/.gitconfig: Device or resource busy").
//
// .claude.json is a single-file mount, and a file mount is pinned to the
// inode it was created from: anything on the host that replaces it by
// rename leaves the container reading a deleted inode. That is why the
// writers here work IN PLACE (see WriteInPlace) rather than using Go's usual
// write-temp-then-rename "atomic write".
package datadir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// HomeDir holds what the sandbox keeps of its home, each [[keep]] entry at
// its path under ~.
const HomeDir = "home"

// Home is home-relative path rel's place in the data dir.
func Home(rel string) string { return HomeDir + "/" + rel }

// Paths in the data dir, under HomeDir, that caboose itself reads or
// writes. Each is in a Required keep entry, or in one of the defaults.
const (
	// ClaudeDir is ~/.claude.
	ClaudeDir = HomeDir + "/.claude"
	// ClaudeJSON is ~/.claude.json, a single-file mount.
	ClaudeJSON = HomeDir + "/.claude.json"
	// Credentials is Claude Code's login: ~/.claude/.credentials.json
	// inside the container, where it is kept in a file rather than a
	// keychain.
	Credentials = ClaudeDir + "/.credentials.json"
	// CabooseConfig is the sandbox's ~/.config/caboose, the user's own
	// configuration of it: the sandbox config, StartDir and ShellDir.
	// Written from both sides -- by hand on the host, or by a session --
	// so the host only ever reads it through nofollow.
	CabooseConfig = HomeDir + "/.config/caboose"
	// SandboxConfig is the sandbox config (internal/sandboxcfg).
	SandboxConfig = HomeDir + "/" + sandboxcfg.Rel
	// StartDir holds the scripts the entrypoint runs at container start,
	// in name order (run_start_scripts in entrypoint.sh).
	StartDir = CabooseConfig + "/start.d"
	// ShellDir holds what every interactive bash reads (shellrc.bash).
	ShellDir = CabooseConfig + "/shell.d"
	// GitConfig is the sandbox's global git config, ~/.config/git/config.
	GitConfig = HomeDir + "/.config/git/config"
	// JJConfig is the sandbox's user-level jj config.
	JJConfig = HomeDir + "/.config/jj/config.toml"
	// SSHDir is the sandbox's ~/.ssh: its known_hosts and its own config.
	// ssh refuses a ~/.ssh others can write, and every kept directory is
	// created 0700. Private keys do not belong here -- they stay on the
	// host and reach the sandbox through the forwarded agent -- and doctor
	// says so when it finds one (PrivateKeysIn).
	SSHDir = HomeDir + "/.ssh"
)

// LastGoodConfig is a copy of the last sandbox config the host could read,
// in the data dir itself, which no container mounts: what a launch uses
// when the sandbox config does not parse, or is of a newer format.
const LastGoodConfig = "sandbox.last-good.toml"

// ProposalsDir is where sessions leave proposals for 'caboose apply'
// (internal/proposal), mounted so that they can write them. Created here
// for the reason SyncDir is.
const ProposalsDir = "proposals"

// SyncDir is caboose sync's git repo (internal/statesync), mounted so that the
// sync, which runs in the sandbox, works on it there. Created here, before the container is, since a
// bind-mount source docker has to create itself is root's on a Linux host.
const SyncDir = "sync"

// ManagedDir is Claude Code's managed settings directory as the sandbox has
// it (ManagedTarget), which holds caboose's instructions to every session
// (ManagedInstructions) and skills (ManagedSkills) and nothing else. Mounted read-only, the one mount
// of the data dir the sandbox cannot write: the instructions are caboose's,
// and the sandbox's ~/.claude/CLAUDE.md stays the user's own. Created here
// for the reason SyncDir is.
const ManagedDir = "claude-code"

// ManagedInstructions is the CLAUDE.md in ManagedDir, written by
// InstallManaged on every launch.
const ManagedInstructions = ManagedDir + "/CLAUDE.md"

// ManagedSkills is where Claude Code looks for managed skills, inside
// ManagedDir: one directory per skill, each with a SKILL.md. InstallManaged
// owns everything under it.
const ManagedSkills = ManagedDir + "/.claude/skills"

// ManagedTarget is where the sandbox has ManagedDir: where Claude Code on
// Linux looks for its managed policy, whose CLAUDE.md it reads before the
// user's and the project's, and which no setting excludes. A directory
// mount, so that a file the image has there cannot mix with caboose's (the
// image check refuses a base that has one at all).
const ManagedTarget = "/etc/claude-code"

// Machinery are the directories caboose mounts for itself, beside HomeDir.
var Machinery = []string{SyncDir, ProposalsDir}

// seeds are what a kept file starts as when it is new or empty, by home-
// relative path. An empty .claude.json is not valid JSON: the first
// `claude install` in a fresh data dir reports it corrupted and fails.
var seeds = map[string]string{".claude.json": "{}\n"}

// created are files that must exist inside a kept directory, by its
// home-relative path.
//
// git writes `git config --global` to ~/.config/git/config only when that
// file already exists and ~/.gitconfig does not; otherwise it creates
// ~/.gitconfig, which is outside every mount and dies with the container.
// jj has no such rule -- it creates config.toml itself -- so it is not here.
var created = map[string][]string{".config/git": {"config"}}

// createdDirs are directories made inside a kept one, by its home-relative
// path: start.d and shell.d, so there is somewhere obvious to put a script.
var createdDirs = map[string][]string{".config/caboose": {"start.d", "shell.d"}}

// EnsureLayout creates what the container mounts: each kept directory
// (0700 when new, like every place a tool may keep a token) or file, and
// caboose's own directories. An existing one is left as it is.
//
// Bind-mounting a file requires the file to exist first, or Docker creates a
// directory in its place; and a mount source docker creates itself is
// root's on a Linux host. Cheap and idempotent, so it also repairs a data
// dir that lost a piece. A directory no entry names any more is left where
// it is, with what it holds.
func EnsureLayout(dir string, keep []sandboxcfg.Keep) error {
	for _, d := range Machinery {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o777); err != nil {
			return err
		}
	}
	// Readable by every user the sandbox may run as, writable by none of
	// them: the mount is read-only anyway.
	if err := os.MkdirAll(filepath.Join(dir, ManagedDir), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, HomeDir), 0o700); err != nil {
		return err
	}
	now := time.Now()
	for _, k := range keep {
		p := filepath.Join(dir, HomeDir, filepath.FromSlash(k.Rel))
		if k.File {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			if err := touch(p, now); err != nil {
				return err
			}
			if seed, ok := seeds[k.Rel]; ok {
				if err := seedIfEmpty(p, seed); err != nil {
					return err
				}
			}
			continue
		}
		if err := mkdirPrivate(p); err != nil {
			return err
		}
		for _, sub := range createdDirs[k.Rel] {
			if err := os.MkdirAll(filepath.Join(p, sub), 0o777); err != nil {
				return err
			}
		}
		for _, f := range created[k.Rel] {
			if err := touch(filepath.Join(p, f), now); err != nil {
				return err
			}
		}
	}
	return nil
}

// PrivateKeysIn lists the files directly in the sandbox's ~/.ssh (SSHDir)
// that hold a private key, by name. Read through nofollow: the container
// writes that directory, so nothing in it is followed.
func PrivateKeysIn(dir string) ([]string, error) {
	d := nofollow.Dir(dir)
	entries, err := d.ReadDir(SSHDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, _, err := d.ReadFile(SSHDir + "/" + e.Name())
		if err != nil {
			continue // a link, or gone since: not a key file ssh would use
		}
		if privateKeyHeader.Match(data) {
			keys = append(keys, e.Name())
		}
	}
	return keys, nil
}

// StartScripts lists StartDir as the entrypoint's run_start_scripts will
// see it, in the order it runs them: run holds what it runs, skipped the
// files it passes over for not being executable. Dotfiles, backups (*~)
// and directories are neither. A symlink is listed in run: the container
// follows it, and the host, reading through nofollow, does not look.
func StartScripts(dir string) (run, skipped []string, err error) {
	entries, err := nofollow.Dir(dir).ReadDir(StartDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
			continue
		}
		switch t := e.Type(); {
		case t&fs.ModeSymlink != 0:
			run = append(run, name)
		case t.IsRegular():
			if info, err := e.Info(); err == nil && info.Mode()&0o111 != 0 {
				run = append(run, name)
			} else {
				skipped = append(skipped, name)
			}
		}
	}
	return run, skipped, nil
}

// privateKeyHeader is how every private key format ssh reads starts:
// OpenSSH's own, PEM (RSA, EC, DSA, PKCS#8), encrypted or not.
var privateKeyHeader = regexp.MustCompile(`^\s*-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

// mkdirPrivate creates path 0700 (its parents too, when missing), and
// leaves an existing one alone.
func mkdirPrivate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	err := os.Mkdir(path, 0o700)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Exactly 0700, whatever the umask.
	return os.Chmod(path, 0o700)
}

// touch creates the file if missing, like touch(1), with the umask applied
// to 0666. An existing file is never opened for writing -- it may well be
// read-only, which touch(1) does not mind -- and never truncated or
// replaced. Its times are bumped best-effort: nothing depends on them, so
// being unable to is not an error.
func touch(path string, now time.Time) error {
	if err := CreateIfMissing(path); err != nil {
		return err
	}
	_ = os.Chtimes(path, now, now)
	return nil
}

// seedIfEmpty writes seed into path when it is an empty regular file, in
// place, so a running container's file mount of it stays valid. Anything with
// content is left alone, valid or not: it is Claude's to repair.
func seedIfEmpty(path, seed string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() != 0 {
		return nil
	}
	return WriteInPlace(path, []byte(seed))
}

// CreateIfMissing creates an empty file at path unless something is already
// there, as `[ -e "$f" ] || : > "$f"` does, with the umask applied to 0666.
// An existing file is left entirely alone.
func CreateIfMissing(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// WriteInPlace truncates path and writes data into the same inode, as the
// shell's `cat tmp > dst` does. It never renames, so a single-file bind
// mount of path (.claude.json) stays valid.
func WriteInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// UpstreamURL is where the sandbox is edited when no checkout can be found.
const UpstreamURL = "https://github.com/bfreis/caboose"

// Module is caboose's module path, which a clone's go.mod names.
const Module = "github.com/bfreis/caboose"

// Limits on FindClone, which runs at every launch: how deep below a root it
// looks, and how many directories it reads in all.
var (
	cloneDepth = 3
	cloneDirs  = 4000
)

// FindClone is the host path of a clone of caboose's source under the
// roots, or "" (one whose path is not cleanPath is not a candidate): a repository (a directory with .git or .jj) whose go.mod
// declares Module, at most cloneDepth below a root. A repository is
// checked and never descended into, hidden directories and symlinks are
// skipped, and the search gives up after cloneDirs directories, so a large
// root costs a bounded read. The roots are the sandbox's to write, so a
// symlink there is never followed.
func FindClone(roots []config.Root) string {
	budget := cloneDirs
	type dir struct {
		path  string
		depth int
	}
	var queue []dir
	for _, r := range roots {
		queue = append(queue, dir{r.Host, 0})
	}
	for len(queue) > 0 && budget > 0 {
		d := queue[0]
		queue = queue[1:]
		budget--
		ents, err := os.ReadDir(d.path)
		if err != nil {
			continue
		}
		repo := false
		for _, e := range ents {
			if (e.Name() == ".git" || e.Name() == ".jj") && e.Type()&fs.ModeSymlink == 0 {
				repo = true
			}
		}
		if repo {
			if cleanPath(d.path) && declaresModule(filepath.Join(d.path, "go.mod")) {
				return d.path
			}
			continue
		}
		if d.depth == cloneDepth {
			continue
		}
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				queue = append(queue, dir{filepath.Join(d.path, e.Name()), d.depth + 1})
			}
		}
	}
	return ""
}

// cleanPath is whether p is fit to be written into the instructions: the
// sandbox chooses a clone's name, and it goes into a read-only file every
// session reads. Printable characters only, no backtick (it would end the
// Markdown code span a path is written in) and no "@@".
func cleanPath(p string) bool {
	if strings.Contains(p, "@@") || strings.Contains(p, "`") {
		return false
	}
	for _, r := range p {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// declaresModule is whether path is a regular file, not a symlink, whose
// module line names Module.
func declaresModule(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`) == Module
		}
	}
	return false
}

// DescribeMounts is what replaces @@CABOOSE_ROOTS@@: each root's host path
// and where the container has it, as Markdown.
func DescribeMounts(roots []config.Root) string {
	var parts []string
	for _, r := range roots {
		parts = append(parts, "`"+r.Host+"` at `"+r.Container+"`")
	}
	return strings.Join(parts, ", ")
}

// LoggedIn reports whether the data dir holds a Claude login: Credentials
// as a plain, non-empty file. It is looked at, never read, and reached
// through plain directories only (the container writes .claude). Anything
// else there -- a link, a directory -- is an error, not a login.
func LoggedIn(dataDir string) (bool, error) {
	fi, err := nofollow.Dir(dataDir).Lstat(Credentials)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("~/.claude/.credentials.json is not a plain file")
	}
	return fi.Size() > 0, nil
}
