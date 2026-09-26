package datadir

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/nofollow"
)

// Git is the slice of git that reading and writing the sandbox's config
// needs.
type Git interface {
	// Get reads key from the config file at path; "" if unset.
	Get(path, key string) string
	// GetGlobal reads key from the host's global config; "" if unset.
	GetGlobal(key string) string
	// Set writes key into the config file at path.
	Set(path, key, value string) error
	// Unset removes every value of key from the config file at path; a
	// key that is not there is no error.
	Unset(path, key string) error
}

// ExecGit is Git backed by the git binary.
type ExecGit struct{ Path string }

// FindGit returns the git on $PATH, or nil when there is none.
func FindGit() Git {
	p, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	return ExecGit{Path: p}
}

func (g ExecGit) out(args ...string) string {
	return g.outIn("", args...)
}

// outIn is out run from dir, or from the current directory when "".
func (g ExecGit) outIn(dir string, args ...string) string {
	cmd := exec.Command(g.Path, args...)
	cmd.Dir = dir
	b, _ := cmd.Output()
	return strings.TrimRight(string(b), "\n")
}

// Get implements Git.
func (g ExecGit) Get(path, key string) string { return g.out("config", "--file", path, "--get", key) }

// GetGlobal implements Git: the host's default, whatever directory the
// launcher runs in.
//
// --includes, which --global otherwise turns off, because an identity or
// signing key kept in an included file is still the host's. But run from
// "/", outside any repo, so that an includeIf (gitdir:, onbranch:,
// hasconfig:) matches nothing: those are per-repo overrides -- a work
// address for work checkouts, say -- and from the project dir caboose was
// run in, one would be offered as the sandbox's identity for every repo.
func (g ExecGit) GetGlobal(key string) string {
	return g.outIn("/", "config", "--global", "--includes", "--get", key)
}

// Set implements Git.
func (g ExecGit) Set(path, key, value string) error {
	cmd := exec.Command(g.Path, "config", "--file", path, key, value)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Unset implements Git. git config exits 5 for a key that is not set.
func (g ExecGit) Unset(path, key string) error {
	cmd := exec.Command(g.Path, "config", "--file", path, "--unset-all", key)
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 5 {
		return nil
	}
	return err
}

// editConfig runs fn on a host-side copy of the sandbox's git config, and
// puts the copy back when fn changed it.
//
// The host's git is never pointed at the file itself: the container writes
// dot_config/git, and git follows a symlink there -- reading, and on a set
// writing, whatever host file it names. The copy is read and written back
// through internal/nofollow instead, so a config that is not a plain file
// is nofollow.ErrNotPlain, and left alone. Written back by rename, which
// is fine for a file reached through a mount of its directory.
func editConfig(dataDir string, fn func(path string) error) error {
	d := nofollow.Dir(dataDir)
	data, mode, err := d.ReadFile(GitConfig)
	if errors.Is(err, fs.ErrNotExist) {
		data, mode, err = nil, 0o666, nil // created, with the umask, if fn sets anything
	}
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "caboose-gitconfig-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	path := filepath.Join(tmp, "config")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if err := fn(path); err != nil {
		return err
	}
	out, err := os.ReadFile(path)
	if err != nil || bytes.Equal(out, data) {
		return err
	}
	return d.WriteFile(GitConfig, out, mode.Perm())
}

// Identity is a git identity: user.name and user.email.
type Identity struct{ Name, Email string }

// Complete reports whether both are set: git refuses to commit with
// either missing ("Author identity unknown").
func (id Identity) Complete() bool { return id.Name != "" && id.Email != "" }

// HostIdentity is the host's default identity, as GetGlobal reads it (an
// includeIf matches nothing): what caboose setup offers an environment
// that has none of its own.
func HostIdentity(git Git) Identity {
	if git == nil {
		return Identity{}
	}
	return Identity{Name: git.GetGlobal("user.name"), Email: git.GetGlobal("user.email")}
}

// SandboxGit is what caboose setup and doctor read of the sandbox's global
// git config: its identity and its signing set-up.
type SandboxGit struct {
	Identity
	Signing SandboxSigning
}

// ReadSandboxGit reads the sandbox's identity and signing from dataDir's
// git config, changing nothing, through a host-side copy as editConfig
// works. No git, or no file, is nothing set.
func ReadSandboxGit(dataDir string, git Git) (SandboxGit, error) {
	var s SandboxGit
	if git == nil {
		return s, nil
	}
	err := readConfig(dataDir, func(path string) {
		s = SandboxGit{
			Identity: Identity{Name: git.Get(path, "user.name"), Email: git.Get(path, "user.email")},
			Signing: SandboxSigning{
				Format:     git.Get(path, "gpg.format"),
				Key:        git.Get(path, "user.signingkey"),
				CommitSign: git.Get(path, "commit.gpgsign"),
			},
		}
	})
	return s, err
}

// Change is one edit to the sandbox's git config: Key set to Value, or
// removed when Unset.
type Change struct {
	Key, Value string
	Unset      bool
}

// WriteSandboxGit applies changes to the sandbox's global git config,
// dataDir/dot_config/git/config, creating it if need be. Only through
// editConfig's host-side copy: a config that is a link is
// nofollow.ErrNotPlain, and nothing is written.
//
// caboose setup is the only writer. A launch never seeds the host's
// identity and signing, which would give a new environment the host's
// identity without a word; it only says when there is none.
func WriteSandboxGit(dataDir string, git Git, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	return editConfig(dataDir, func(path string) error {
		for _, c := range changes {
			var err error
			if c.Unset {
				err = git.Unset(path, c.Key)
			} else {
				err = git.Set(path, c.Key, c.Value)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", c.Key, err)
			}
		}
		return nil
	})
}

// SigningChanges make the sandbox sign with key, a literal SSH public key,
// through the forwarded agent. Whether a plain commit signs is
// commit.gpgsign, set apart.
func SigningChanges(key string) []Change {
	return []Change{{Key: "gpg.format", Value: "ssh"}, {Key: "user.signingkey", Value: key}}
}

// NoSigningChanges turn the sandbox's signing off: without a key, a
// commit.gpgsign or tag.gpgsign left behind would only make commits fail.
func NoSigningChanges() []Change {
	var c []Change
	for _, k := range []string{"user.signingkey", "gpg.format", "commit.gpgsign", "tag.gpgsign"} {
		c = append(c, Change{Key: k, Unset: true})
	}
	return c
}

// HostSigningKey is the key the host signs commits with over SSH, as the
// literal public key the sandbox can be given, or why it cannot be; both
// "" when the host does not sign with SSH.
//
// Only SSH signing (gpg.format=ssh): the sandbox signs through the
// forwarded agent (see the launcher's sshAgentArgs), so the private key
// never enters it. gpg.ssh.program is never carried over -- on a Mac it
// is 1Password's op-ssh-sign, which does not exist in the container; git's
// default, ssh-keygen, signs there with the agent.
func HostSigningKey(git Git) (key, skipped string) {
	if git == nil || !strings.EqualFold(git.GetGlobal("gpg.format"), "ssh") {
		return "", ""
	}
	hostKey := git.GetGlobal("user.signingkey")
	if hostKey == "" {
		return "", ""
	}
	return LiteralSSHKey(hostKey)
}

// SandboxSigning is the sandbox's own signing set-up, as its global git
// config has it.
type SandboxSigning struct {
	// Format is gpg.format; Key user.signingkey, literal ("key::" and all)
	// or a path; CommitSign is commit.gpgsign, as git spells it.
	Format, Key, CommitSign string
}

// Signs reports whether a plain `git commit` in the sandbox signs.
func (s SandboxSigning) Signs() bool {
	switch strings.ToLower(s.CommitSign) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

// PublicKey is the key's literal public key line, or "" when Key is a
// path (or unset): the half of it an agent's listing can be matched with.
func (s SandboxSigning) PublicKey() string {
	v := strings.TrimSpace(s.Key)
	if rest, ok := strings.CutPrefix(v, "key::"); ok {
		v = strings.TrimSpace(rest)
	}
	if isPublicKey(v) {
		return v
	}
	return ""
}

// ReadSandboxSigning reads the sandbox's signing settings from dataDir's
// git config, changing nothing: through a host-side copy, as editConfig
// works, so the host's git never opens the file the container writes. No
// git, or no file, is no settings.
func ReadSandboxSigning(dataDir string, git Git) (SandboxSigning, error) {
	s, err := ReadSandboxGit(dataDir, git)
	return s.Signing, err
}

// readConfig is editConfig for a read: fn sees a copy, which is thrown
// away.
func readConfig(dataDir string, fn func(path string)) error {
	return editConfig(dataDir, func(path string) error {
		fn(path)
		return nil
	})
}

// sshKeyTypes are the prefixes of an OpenSSH public key line.
var sshKeyTypes = []string{"ssh-", "ecdsa-", "sk-ssh-", "sk-ecdsa-"}

// IsPublicKey reports whether s starts like an OpenSSH public key line.
func IsPublicKey(s string) bool { return isPublicKey(s) }

func isPublicKey(s string) bool {
	for _, p := range sshKeyTypes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// LiteralSSHKey turns a user.signingkey -- a literal key, "key::" and
// all, or the path of a .pub file on the host -- into the literal public
// key the sandbox can use, or says why it cannot. A private key file is
// refused: using it would mean copying the key in, which is what the agent
// is for.
func LiteralSSHKey(v string) (key, skipped string) {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutPrefix(v, "key::"); ok {
		v = strings.TrimSpace(rest)
	}
	if isPublicKey(v) {
		return v, ""
	}
	path := v
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "user.signingkey " + v + " is in a home directory caboose cannot find"
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "user.signingkey " + v + " cannot be read"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	if isPublicKey(line) {
		return strings.TrimSpace(line), ""
	}
	return "", "user.signingkey " + v + " is a private key file; point it at the .pub, with the key in your agent"
}
