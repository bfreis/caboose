package sandboxcfg

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// defaultText is the sandbox config caboose writes, DefaultsVersion of it:
// in full, so what the file says is what happens. Default fills in roots.
const defaultText = `# The sandbox config: what the sandbox keeps across containers, and what of
# that 'caboose sync' carries between machines. It is the sandbox's own: a
# session may edit it, and it syncs like any other file. A new or changed
# [[keep]] entry takes effect at the next 'caboose restart'; a sync rule, at
# the next sync. Problems with it are in ~/.caboose-proposals/current/
# sandbox-config.txt, and 'caboose doctor' on the host.
#
# [[keep]]   path  a directory directly in ~, ~/.config, ~/.local,
#                  ~/.local/share or ~/.cache (file = true for a file)
#            sync  true: all of it syncs; or a table of the options below;
#                  or [[keep.sync]] entries, each a path inside it (globs
#                  allowed: * within a name, ** across directories), the
#                  first one matching a file deciding how it merges:
#                    merge    "text" (default), "json" (key by key) or
#                             "union" (a list of lines: both sides kept)
#                    keys     with "json": only these keys sync
#                    exclude  paths (~/...) or names (*.tmp) left out

format = 1      # this file's structure
defaults = 1    # the caboose defaults it was written from

# The roots (the host's [roots]) the projects here expect, by their path in
# the sandbox: Claude Code keys a project's state by its path, so a machine
# that mounts no root there never reads the synced memory of its projects,
# and 'caboose doctor' says so.
roots = []

# Claude Code's state. Only some of it syncs: project memory, settings,
# skills, agents and commands -- never transcripts, history or the login.
[[keep]]
path = "~/.claude"
  [[keep.sync]]
  path = "~/.claude/projects/-work*/memory/MEMORY.md"
  merge = "union"    # an index of one-line pointers
  [[keep.sync]]
  path = "~/.claude/projects/-work*/memory"
  [[keep.sync]]
  path = "~/.claude/settings.json"
  merge = "json"
  [[keep.sync]]
  path = "~/.claude/skills"
  exclude = ["~/.claude/skills/synced"]    # Claude Code's cache of the account's skills
  [[keep.sync]]
  path = "~/.claude/agents"
  [[keep.sync]]
  path = "~/.claude/commands"

# Only its MCP servers sync; the rest (the account, caches) stays here.
[[keep]]
path = "~/.claude.json"
file = true
sync = { merge = "json", keys = ["mcpServers"] }

# This file, start.d and shell.d.
[[keep]]
path = "~/.config/caboose"
sync = true

# To share part of the git config, put it in a file of its own that the
# config includes ([include] path = shared), and sync that file alone:
#   [[keep.sync]]
#   path = "~/.config/git/shared"
[[keep]]
path = "~/.config/git"

[[keep]]
path = "~/.config/jj"

# gh's token is in here: never synced.
[[keep]]
path = "~/.config/gh"

# known_hosts and ssh's config. Private keys stay on the host, reaching the
# sandbox through the forwarded agent.
[[keep]]
path = "~/.ssh"
`

// Addition is an entry a DefaultsVersion added to the defaults, offered to
// a file written from an older one.
type Addition struct {
	// Version is the DefaultsVersion that added it.
	Version int
	// Path is its keep path; it is not offered when the file already has
	// an entry for that path.
	Path string
	// Summary says what it is, in a line.
	Summary string
	// Text is the entry, as appended to the file.
	Text string
}

// Additions are the entries each DefaultsVersion after the first added, in
// order. None yet.
var Additions []Addition

// Pending are the Additions c has not had: newer than its Defaults, for a
// path it has no entry for.
func Pending(c *Config) []Addition {
	var out []Addition
	for _, a := range Additions {
		if a.Version <= c.Defaults {
			continue
		}
		if rel, err := keepRel(a.Path); err == nil && c.find(rel) >= 0 && c.Keep[c.find(rel)].Listed {
			continue
		}
		out = append(out, a)
	}
	return out
}

// migrations bring a file of format n to format n+1, by n. None yet: every
// file is format 1.
var migrations = map[int]func([]byte) ([]byte, error){}

// Migrate brings data, of format from, to Format.
func Migrate(data []byte, from int) ([]byte, error) {
	for n := from; n < Format; n++ {
		m, ok := migrations[n]
		if !ok {
			return nil, fmt.Errorf("no way to bring format %d up to %d", n, n+1)
		}
		var err error
		if data, err = m(data); err != nil {
			return nil, fmt.Errorf("format %d to %d: %v", n, n+1, err)
		}
	}
	return data, nil
}

// Append adds a's entry at the end of data.
func Append(data []byte, a Addition) []byte {
	s := strings.TrimRight(string(data), "\n")
	return []byte(s + "\n\n" + strings.TrimRight(a.Text, "\n") + "\n")
}

// Stamp sets the top-level format and defaults of data to the given
// values: each key's line replaced, or added above the first table.
func Stamp(data []byte, format, defaults int) []byte {
	lines := strings.Split(string(data), "\n")
	for _, kv := range []struct {
		key string
		val int
	}{{"defaults", defaults}, {"format", format}} {
		re := regexp.MustCompile(`^(\s*` + kv.key + `\s*=\s*)[^\s#]*`)
		top := firstHeader(lines)
		if i := slices.IndexFunc(lines[:top], re.MatchString); i >= 0 {
			// Only the value: spacing and a comment after it stay.
			lines[i] = re.ReplaceAllString(lines[i], "${1}"+fmt.Sprint(kv.val))
			continue
		}
		line := fmt.Sprintf("%s = %d", kv.key, kv.val)
		// Above the first table, after the leading comment block.
		at := 0
		for at < top && strings.HasPrefix(strings.TrimSpace(lines[at]), "#") {
			at++
		}
		lines = slices.Insert(lines, at, line)
	}
	return []byte(strings.Join(lines, "\n"))
}

var headerRE = regexp.MustCompile(`^\s*\[`)

// firstHeader is the index of the first table header in lines, or
// len(lines).
func firstHeader(lines []string) int {
	for i, l := range lines {
		if headerRE.MatchString(l) {
			return i
		}
	}
	return len(lines)
}
