package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// SSHLookup is where agentFrom looks for the agent ssh would use by
// default: a seam, so that no test runs this machine's ssh or reads its
// ~/.ssh.
type SSHLookup struct {
	// G is what `ssh -G host` prints, or why it printed nothing usable
	// (no ssh, or ssh failing: its first line of stderr).
	G func(host string) ([]byte, error)
	// Home is the home directory ~ stands for.
	Home func() (string, error)
	// FS is the file system ~/.ssh/config, and what it includes, are
	// read from, rooted at /.
	FS fs.FS
}

// hostSSH is the SSHLookup of this machine: its ssh, as PATH finds it,
// its home and its files. TestMain replaces it with one that has none.
var hostSSH = &SSHLookup{G: runSSHG, Home: os.UserHomeDir, FS: os.DirFS("/")}

// sshLookup is a.SSH, else hostSSH.
func (a *App) sshLookup() *SSHLookup {
	if a.SSH != nil {
		return a.SSH
	}
	return hostSSH
}

// sshGTimeout bounds `ssh -G`, which runs a config's `Match exec`.
const sshGTimeout = 5 * time.Second

// agentProbeHost is the host the default agent is looked up for: a
// reserved name (RFC 2606's .invalid) that no Host block of a user's
// names, so only Host * and the lines before any Host decide. A block for
// one host -- "Host github.com" with "IdentityAgent none", say -- is
// that host's choice, not the sandbox's.
const agentProbeHost = "caboose-default-agent.invalid"

// runSSHG runs `ssh -G host` with the ssh PATH finds.
func runSSHG(host string) ([]byte, error) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return nil, errors.New("no ssh on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshGTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ssh, "-G", host)
	var stderr limitBuffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		why := err.Error()
		if ctx.Err() != nil {
			why = "timed out"
		} else if line := firstLine(stderr.String()); line != "" {
			why = daemonproto.Text(line, 200)
		}
		return nil, fmt.Errorf("%s -G %s failed: %s", ssh, host, why)
	}
	return out, nil
}

// sshGIdentityAgent is the identityagent line of `ssh -G` output, and
// whether it has one. ssh prints the value unquoted, spaces and all.
func sshGIdentityAgent(out []byte) (string, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.EqualFold(k, "identityagent") {
			return strings.Trim(strings.TrimSpace(v), `"`), true
		}
	}
	return "", false
}

// maxSSHConfig bounds each config file read.
const maxSSHConfig = 256 << 10

// readBounded reads name of fsys, failing past max bytes.
func readBounded(fsys fs.FS, name string, max int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("larger than %d KiB", max>>10)
	}
	return b, nil
}

// sshMaxDepth is how deep Includes nest, as ssh's own limit.
const sshMaxDepth = 16

// configIdentityAgent reads ~/.ssh/config, and what it includes, for the
// IdentityAgent that applies to agentProbeHost, as ssh would without
// running: the first value in a Host block whose patterns match it (or
// before any Host line), a negated pattern excluding it. It gives up on
// what it cannot decide without running ssh -- a Match line before a
// value, a % token in the value -- with an error saying so; a missing
// file is no value and no error.
func configIdentityAgent(l *SSHLookup) (string, bool, error) {
	home, err := l.Home()
	if err != nil || home == "" {
		return "", false, errors.New("no home directory")
	}
	p := &sshConfigParser{fs: l.FS, home: home}
	v, ok, err := p.file(path.Join(home, ".ssh", "config"), 0)
	if err != nil || !ok {
		return "", false, err
	}
	x, ok := expandSSHTokens(v, home)
	if !ok {
		return "", false, fmt.Errorf("~/.ssh/config: IdentityAgent %q has a %% token only ssh expands", v)
	}
	return x, true, nil
}

// expandSSHTokens expands v's % tokens in one pass, left to right, as ssh
// does: %d the home, %% a %; ok is false at any other.
func expandSSHTokens(v, home string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '%' {
			b.WriteByte(v[i])
			continue
		}
		if i++; i == len(v) {
			return "", false
		}
		switch v[i] {
		case '%':
			b.WriteByte('%')
		case 'd':
			b.WriteString(home)
		default:
			return "", false
		}
	}
	return b.String(), true
}

type sshConfigParser struct {
	fs   fs.FS
	home string
}

// fsPath is the absolute p as a path of p.fs, rooted at /.
func fsPath(p string) string {
	return strings.TrimPrefix(path.Clean(p), "/")
}

// file reads one config file from its start, every line active until a
// Host line says otherwise.
func (p *sshConfigParser) file(name string, depth int) (string, bool, error) {
	if depth > sshMaxDepth {
		return "", false, errors.New("~/.ssh/config: Include nests too deep")
	}
	b, err := readBounded(p.fs, fsPath(name), maxSSHConfig)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%s: %v", tildePath(name, p.home), err)
	}
	active := true
	for n, line := range strings.Split(string(b), "\n") {
		key, args := splitSSHLine(line)
		if key == "" || !active && key != "host" && key != "match" {
			continue
		}
		switch key {
		case "host":
			active = hostMatches(args, agentProbeHost)
		case "match":
			return "", false, fmt.Errorf("%s line %d: Match, which only ssh can decide", tildePath(name, p.home), n+1)
		case "identityagent":
			if len(args) > 0 {
				return args[0], true, nil
			}
		case "include":
			for _, pat := range args {
				v, ok, err := p.include(pat, depth)
				if err != nil || ok {
					return v, ok, err
				}
			}
		}
	}
	return "", false, nil
}

// include reads the files pat names, in order: under ~/.ssh when it is
// relative, as ssh takes a user config's.
func (p *sshConfigParser) include(pat string, depth int) (string, bool, error) {
	switch {
	case pat == "~" || strings.HasPrefix(pat, "~/"):
		pat = path.Join(p.home, pat[1:])
	case !path.IsAbs(pat):
		pat = path.Join(p.home, ".ssh", pat)
	}
	names, err := fs.Glob(p.fs, fsPath(pat))
	if err != nil {
		return "", false, fmt.Errorf("~/.ssh/config: Include %q: %v", pat, err)
	}
	for _, n := range names {
		if hiddenMatch(fsPath(pat), n) {
			continue
		}
		v, ok, err := p.file("/"+n, depth+1)
		if err != nil || ok {
			return v, ok, err
		}
	}
	return "", false, nil
}

// hiddenMatch is whether name, a match of pat, has a component that
// starts with a dot where pat's does not: glob(3), which ssh's Include
// uses, never matches one with a wildcard, as path.Match does.
func hiddenMatch(pat, name string) bool {
	pc, nc := strings.Split(pat, "/"), strings.Split(name, "/")
	if len(pc) != len(nc) {
		return true
	}
	for i := range nc {
		if strings.HasPrefix(nc[i], ".") && !strings.HasPrefix(pc[i], ".") {
			return true
		}
	}
	return false
}

// tildePath is name with home written as ~, for a message.
func tildePath(name, home string) string {
	if rest, ok := strings.CutPrefix(name, home+"/"); ok {
		return "~/" + rest
	}
	return name
}

// splitSSHLine is a config line's keyword, lowercased, and its
// arguments, as ssh splits them: "Key value", "Key=value", double or
// single quotes around an argument with spaces, a backslash escaping the
// next character, # starting a comment. A blank or comment line has no
// keyword.
func splitSSHLine(line string) (string, []string) {
	line = strings.TrimSpace(strings.TrimRight(line, "\r"))
	if line == "" || line[0] == '#' {
		return "", nil
	}
	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		return strings.ToLower(line), nil
	}
	key, rest := strings.ToLower(line[:end]), strings.TrimLeft(line[end:], " \t")
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	var args []string
	for rest != "" {
		if rest[0] == '#' {
			break
		}
		var b strings.Builder
		var quote byte
		i := 0
	scan:
		for ; i < len(rest); i++ {
			c := rest[i]
			switch {
			case c == '\\' && i+1 < len(rest):
				i++
				b.WriteByte(rest[i])
			case quote != 0 && c == quote:
				quote = 0
			case quote != 0:
				b.WriteByte(c)
			case c == '"' || c == '\'':
				quote = c
			case c == ' ' || c == '\t':
				break scan
			default:
				b.WriteByte(c)
			}
		}
		args = append(args, b.String())
		rest = strings.TrimLeft(rest[i:], " \t")
	}
	return key, args
}

// hostMatches is whether a Host line's patterns take host: one matches
// and no negated one does. As ssh, host is lowercased and the patterns
// are not: "Host GitHub.com" never matches.
func hostMatches(patterns []string, host string) bool {
	host = strings.ToLower(host)
	matched := false
	for _, pat := range patterns {
		if neg, ok := strings.CutPrefix(pat, "!"); ok {
			if wildMatch(neg, host) {
				return false
			}
		} else if wildMatch(pat, host) {
			matched = true
		}
	}
	return matched
}

// wildMatch is ssh's pattern match: * any run of characters, ? any one.
func wildMatch(pat, s string) bool {
	for pat != "" {
		switch pat[0] {
		case '*':
			for pat != "" && pat[0] == '*' {
				pat = pat[1:]
			}
			if pat == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if wildMatch(pat, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
		default:
			if s == "" || s[0] != pat[0] {
				return false
			}
		}
		pat, s = pat[1:], s[1:]
	}
	return s == ""
}
