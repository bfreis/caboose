package sandboxcfg

// defaultText is the sandbox config caboose writes: in full, so what the
// file says is what happens. Default fills in roots.
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
#                  {roots}, as a whole path component, is the directory
#                  Claude Code keeps a project's state in, for any project
#                  under the roots below

format = 1      # this file's structure

# The roots (the host's [roots]) the projects here expect, by their path in
# the sandbox: Claude Code keys a project's state by its path, so a machine
# that mounts no root there never reads the synced memory of its projects,
# and 'caboose doctor' says so. Only the projects under these sync: a root
# added to the host later syncs once it is listed here too.
roots = []

# Claude Code's state. Only some of it syncs: project memory, settings,
# skills, agents and commands -- never transcripts, history or the login.
[[keep]]
path = "~/.claude"
  [[keep.sync]]
  path = "~/.claude/projects/{roots}/memory/MEMORY.md"
  merge = "union"    # an index of one-line pointers
  [[keep.sync]]
  path = "~/.claude/projects/{roots}/memory"
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
