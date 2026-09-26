// Package datadir prepares the host-side data dir that the container
// bind-mounts piece by piece: its layout, the sandbox's git config and the
// installed sandbox CLAUDE.md.
//
// Configuration that a tool rewrites is mounted as a DIRECTORY, never as a
// single file. git, jj and gh all save by writing a temp file and renaming
// it over the original, and a rename onto a single-file bind mount fails
// inside the container with EBUSY (`gh auth login`: "could not write config
// file /home/agent/.gitconfig: Device or resource busy"). So they live under
// dot_config/ and reach the container at their XDG paths.
//
// .claude.json is the one single-file mount, and a file mount is pinned to
// the inode it was created from: anything on the host that replaces it by
// rename leaves the container reading a deleted inode. That is why the
// writers here work IN PLACE (see WriteInPlace) rather than using Go's usual
// write-temp-then-rename "atomic write".
package datadir

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
)

// Dirs are the directories bind-mounted into the container, other than the
// platform dir's (PlatformMounts): those depend on the image, and are only
// created when a container is, by EnsurePlatformLayout.
var Dirs = []string{
	".claude",
	"dot_config/git",
	"dot_config/jj",
	PrivateDir,
	SSHDir,
	SyncDir,
	ProposalsDir,
	StartDir,
	ShellDir,
}

// CabooseConfig is the sandbox's ~/.config/caboose, the user's own
// configuration of it: StartDir and ShellDir. Mounted whole, as one
// directory, and written from both sides -- by hand on the host, or by a
// session -- so the host only ever reads it through nofollow.
const CabooseConfig = "dot_config/caboose"

// StartDir holds the scripts the entrypoint runs at container start, in
// name order (run_start_scripts in entrypoint.sh).
const StartDir = CabooseConfig + "/start.d"

// ShellDir holds what every interactive bash reads (shellrc.bash).
const ShellDir = CabooseConfig + "/shell.d"

// ProposalsDir is where sessions leave proposals for 'caboose apply'
// (internal/proposal), mounted so that they can write them. Created here
// for the reason SyncDir is.
const ProposalsDir = "proposals"

// SyncDir is caboose sync's git repo (internal/statesync), mounted so that the
// container's git runs it. Created here, before the container is, since a
// bind-mount source docker has to create itself is root's on a Linux host.
const SyncDir = "sync"

// PrivateDir is gh's config dir, mounted at ~/.config/gh. It holds the
// GitHub token (there is no keyring in the container), so it is created
// 0700 rather than with the umask.
const PrivateDir = "dot_config/gh"

// SSHDir is the sandbox's ~/.ssh: its known_hosts and its own config, kept
// across containers. Mounted whole, as a directory, since ssh-keygen -R
// saves known_hosts by rename. ssh refuses a ~/.ssh others can write, so it
// is created 0700. Private keys do not belong here -- they stay on the host
// and reach the sandbox through the forwarded agent -- and doctor says so
// when it finds one (PrivateKeysIn).
const SSHDir = "dot_ssh"

// Private are the Dirs created 0700 rather than with the umask.
var Private = []string{PrivateDir, SSHDir}

// PersistRoot holds the directories config.toml's [persist] names, each at
// PersistDir(name).
const PersistRoot = "persist"

// PersistDir is where [persist] entry name is kept, relative to the data dir.
func PersistDir(name string) string { return PersistRoot + "/" + name }

// Files are the single files bind-mounted into the container.
var Files = []string{".claude.json"}

// Seeds are what a file in Files starts as when it is new or empty. An empty
// .claude.json is not valid JSON: the first `claude install` in a fresh data
// dir reports it corrupted and fails.
var Seeds = map[string]string{".claude.json": "{}\n"}

// GitConfig is the sandbox's global git config, mounted as part of
// dot_config/git at ~/.config/git/config.
const GitConfig = "dot_config/git/config"

// JJConfig is the sandbox's user-level jj config, mounted as part of
// dot_config/jj at ~/.config/jj/config.toml.
const JJConfig = "dot_config/jj/config.toml"

// Created are files that must exist inside a mounted directory.
//
// git writes `git config --global` to ~/.config/git/config only when that
// file already exists and ~/.gitconfig does not; otherwise it creates
// ~/.gitconfig, which is outside every mount and dies with the container.
// jj has no such rule -- it creates config.toml itself -- so it is not here.
var Created = []string{GitConfig}

// EnsureLayout creates the directories and files the container mounts.
//
// Bind-mounting a file requires the file to exist first, or Docker creates a
// directory in its place. Cheap and idempotent, so it also repairs a data dir
// that lost a piece.
func EnsureLayout(dir string) error {
	for _, d := range Dirs {
		mk := func(p string) error { return os.MkdirAll(p, 0o777) }
		if isPrivate(d) {
			mk = mkdirPrivate
		}
		if err := mk(filepath.Join(dir, d)); err != nil {
			return err
		}
	}
	now := time.Now()
	for _, f := range append(append([]string(nil), Files...), Created...) {
		if err := touch(filepath.Join(dir, f), now); err != nil {
			return err
		}
	}
	for f, seed := range Seeds {
		if err := seedIfEmpty(filepath.Join(dir, f), seed); err != nil {
			return err
		}
	}
	return nil
}

func isPrivate(d string) bool {
	for _, p := range Private {
		if d == p {
			return true
		}
	}
	return false
}

// EnsurePersist creates the directory each [persist] entry is kept in, 0700
// like every other place a tool may keep a token, and leaves an existing
// one alone. Like the rest of the layout it has to exist before the
// container is created: a bind-mount source docker creates itself is
// root's on a Linux host. A directory no entry names any more is left
// where it is, with what it holds.
func EnsurePersist(dir string, ps []config.Persist) error {
	for _, p := range ps {
		if err := mkdirPrivate(filepath.Join(dir, PersistRoot, p.Name)); err != nil {
			return err
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

// mkdirPrivate creates path 0700 (its parents with the umask), and leaves
// an existing one alone.
func mkdirPrivate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
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

// Placeholder is what InstallInstructions replaces in sandbox/CLAUDE.md with
// InstructionsLocation.
const Placeholder = "@@CABOOSE_DIR@@"

// RootsPlaceholder is what InstallInstructions replaces in sandbox/CLAUDE.md
// with where each repo root is mounted (DescribeMounts). The file is
// installed for everyone, so it cannot name one person's layout.
const RootsPlaceholder = "@@CABOOSE_ROOTS@@"

// UpstreamURL is where the sandbox is edited when no checkout can be found.
const UpstreamURL = "https://github.com/bfreis/caboose"

// InstructionsLocation is what replaces @@CABOOSE_DIR@@: wherever the
// container can see the caboose checkout, so the installed copy can point at
// it without a hardcoded username -- and says plainly when it is not mounted
// at all, since an agent inside should then say so rather than go looking.
//
// checkout is the host path of the checkout this launcher came from, or ""
// when there is none (an installed binary: install.sh, go install); roots are
// the physical host paths mounted into the container, and where.
func InstructionsLocation(checkout string, roots []config.Root) string {
	if checkout == "" {
		return "NOT MOUNTED — this launcher is an installed binary, not a checkout. The source is " +
			UpstreamURL + ": changes are made there (or in a clone of it on the host), not from in here"
	}
	if p, ok := config.ContainerPath(roots, checkout); ok {
		return p
	}
	return "NOT MOUNTED — its checkout, " + checkout + ", is outside the repo root (" + config.DescribeRoots(roots) +
		"), so it is edited from the host, not from in here"
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

// ExpandInstructions fills in sandbox/CLAUDE.md's placeholders.
func ExpandInstructions(src []byte, checkout string, roots []config.Root) []byte {
	return []byte(strings.NewReplacer(
		Placeholder, InstructionsLocation(checkout, roots),
		RootsPlaceholder, DescribeMounts(roots),
	).Replace(string(src)))
}

// placeholderPattern matches anything shaped like a placeholder.
var placeholderPattern = regexp.MustCompile(`@@[A-Z][A-Z0-9_]*@@`)

// UnknownPlaceholders lists what still looks like a placeholder in
// ExpandInstructions output, i.e. ones this launcher does not know how to
// fill. A checkout's sandbox/CLAUDE.md can be newer than the launcher built
// from it -- pulled, but not rebuilt -- and a placeholder added since would
// otherwise be installed literally.
func UnknownPlaceholders(expanded []byte) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range placeholderPattern.FindAll(expanded, -1) {
		if !seen[string(m)] {
			seen[string(m)] = true
			names = append(names, string(m))
		}
	}
	return names
}

// InstallInstructions installs the sandbox-wide CLAUDE.md into
// dataDir/.claude/CLAUDE.md, expanded by ExpandInstructions, and reports
// whether it had to write.
//
// The data dir sits outside any checkout, so the CLAUDE.md the sandbox reads
// is tracked at sandbox/CLAUDE.md and copied in from here. The launcher is
// the only part of this that sees both the checkout and the data dir, and
// running it per launch rather than per container start means an edit to the
// tracked file reaches the next session without a caboose restart.
//
// Written in place and only when it changed: the destination is inside a
// directory mount, so the inode trap of a single-file mount does not apply
// here -- but a rewrite in place is still the one that cannot surprise a
// live session.
//
// The container writes .claude, so the file is reached through
// internal/nofollow: were it a symlink (or a hard link), a plain write would
// put the instructions over whatever host file it names. Such a file is
// nofollow.ErrNotPlain, and left alone.
func InstallInstructions(src []byte, checkout string, roots []config.Root, dataDir string) (bool, error) {
	// .claude itself is the container's mount point, in the host's own
	// data dir: nothing inside can replace it.
	if err := os.MkdirAll(filepath.Join(dataDir, ".claude"), 0o777); err != nil {
		return false, err
	}
	const dst = ".claude/CLAUDE.md"
	d := nofollow.Dir(dataDir)
	want := ExpandInstructions(src, checkout, roots)
	have, _, err := d.ReadFile(dst)
	switch {
	case err == nil && bytes.Equal(have, want):
		return false, nil
	case err == nil:
		err = d.WriteInPlace(dst, want)
	case errors.Is(err, fs.ErrNotExist):
		err = d.WriteFile(dst, want, 0o666)
	}
	return err == nil, err
}

// Credentials is Claude Code's login in the data dir: ~/.claude/.credentials.json
// inside the container, where it is kept in a file rather than a keychain.
const Credentials = ".claude/.credentials.json"

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
		return false, fmt.Errorf("%s is not a plain file", Credentials)
	}
	return fi.Size() > 0, nil
}
