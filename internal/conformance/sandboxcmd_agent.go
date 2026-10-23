package conformance

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The commands of the host-command, alias and trust checks: caboose-agent
// in the sandbox, and lines appended to a few of the sandbox's own files.
// As with the file commands, every word is a literal the suite wrote:
// alias names and parameters of a fixed alphabet, canary paths (SPath), and
// the fixed files below. Each word is single-quoted, and none may hold a
// quote, so nothing in it is expanded.

// agentBin is the sandbox's caboose-agent, by name: the image puts it on
// the PATH.
const agentBin = "caboose-agent"

// wordRe is a word of a caboose-agent command: an alias name, a
// parameter, a host command and its arguments, a notification. It allows
// a slash, a dot and a space so that bad alias names can be tried.
var wordRe = regexp.MustCompile(`^[A-Za-z0-9 ./=_-]{1,64}$`)

func quoteWords(ws []string) (string, error) {
	q := make([]string, len(ws))
	for i, w := range ws {
		if !wordRe.MatchString(w) {
			return "", fmt.Errorf("conformance: %q is not a word the suite may send", w)
		}
		q[i] = "'" + w + "'"
	}
	return strings.Join(q, " "), nil
}

// AgentRun runs the host's alias name with args (KEY=VALUE, or anything
// else the check means to be refused).
func AgentRun(name string, args ...string) (Cmd, error) {
	w, err := quoteWords(append([]string{name}, args...))
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{body: agentBin + " run " + w}, nil
}

// AgentRunFor runs alias name and ends the caller after secs seconds
// (timeout's SIGTERM), leaving whatever the alias started to the host.
func AgentRunFor(secs int, name string) (Cmd, error) {
	if secs < 1 || secs > 10 {
		return Cmd{}, errors.New("conformance: a caller is ended after 1 to 10 seconds")
	}
	w, err := quoteWords([]string{name})
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{body: fmt.Sprintf("timeout %d %s run %s", secs, agentBin, w)}, nil
}

// hostWordRe is a word of a host command: a command name and its plain
// arguments, no path.
var hostWordRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// AgentHost asks the host to run argv, in the host directory of dir when
// it is set (a canary directory).
func AgentHost(dir *SPath, argv ...string) (Cmd, error) {
	if len(argv) == 0 {
		return Cmd{}, errors.New("conformance: no host command")
	}
	for _, a := range argv {
		if !hostWordRe.MatchString(a) {
			return Cmd{}, fmt.Errorf("conformance: %q is not a word the suite may send", a)
		}
	}
	w, _ := quoteWords(argv)
	if dir == nil {
		return Cmd{body: agentBin + " host -- " + w}, nil
	}
	if !dir.valid() {
		return Cmd{}, errors.New("conformance: a host command's directory is a canary's path")
	}
	return Cmd{body: agentBin + " host -C " + dir.p + " -- " + w}, nil
}

// AgentAliases lists the host's aliases as JSON, on stderr: the wrapper
// discards stdout, and the result's Out keeps all of stderr.
func AgentAliases() Cmd { return Cmd{body: agentBin + " aliases --json 1>&2"} }

// AgentNotify shows text on the host as a notification.
func AgentNotify(text string) (Cmd, error) {
	w, err := quoteWords([]string{text})
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{body: agentBin + " notify " + w}, nil
}

// HomeFile is one of the sandbox's own files a check writes to: fixed
// paths of the product's, or names built from the run's nonce.
type HomeFile struct{ p string }

func (h HomeFile) String() string { return h.p }

// sandboxHome is the agent's home in every image caboose builds.
const sandboxHome = "/home/agent"

// SandboxConfigFile is the sandbox config, ~/.config/caboose/sandbox.toml.
var SandboxConfigFile = HomeFile{sandboxHome + "/.config/caboose/sandbox.toml"}

var n8Re = regexp.MustCompile(`^[0-9a-f]{8}$`)
var fileNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// ProposalFile is a proposal, ~/.caboose-proposals/conf-<n8>-<name>.toml.
func ProposalFile(n8, name string) (HomeFile, error) {
	if !n8Re.MatchString(n8) || !fileNameRe.MatchString(name) {
		return HomeFile{}, errors.New("conformance: a proposal's name is the nonce and a role")
	}
	return HomeFile{sandboxHome + "/.caboose-proposals/conf-" + n8 + "-" + name + ".toml"}, nil
}

// SyncFile is a file in the sync repository, ~/.caboose-sync/conf-<n8>-<name>.toml.
func SyncFile(n8, name string) (HomeFile, error) {
	if !n8Re.MatchString(n8) || !fileNameRe.MatchString(name) {
		return HomeFile{}, errors.New("conformance: a sync file's name is the nonce and a role")
	}
	return HomeFile{sandboxHome + "/.caboose-sync/conf-" + n8 + "-" + name + ".toml"}, nil
}

// lineRe is a line of TOML a check writes: tables, keys, strings and
// arrays of absolute paths, never a quote of the shell's or an expansion.
var lineRe = regexp.MustCompile(`^[A-Za-z0-9 ._/\[\]=",-]{1,300}$`)

// AppendLines appends lines to f.
func AppendLines(f HomeFile, lines ...string) (Cmd, error) {
	if f.p == "" || len(lines) == 0 {
		return Cmd{}, errors.New("conformance: nothing to write")
	}
	parts := make([]string, len(lines))
	for i, l := range lines {
		if !lineRe.MatchString(l) {
			return Cmd{}, fmt.Errorf("conformance: %q is not a line the suite may write", l)
		}
		parts[i] = "echo '" + l + "' >> " + f.p
	}
	return Cmd{body: strings.Join(parts, " && ")}, nil
}
