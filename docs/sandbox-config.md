# The sandbox config

`~/.config/caboose/sandbox.toml`, in the sandbox, says two things:

- what of the sandbox's home is **kept** across containers (anything else a
  tool writes under the home is gone at the next `caboose restart`), and
- what of that [`caboose sync`](sync.md) **carries** between machines.

It is the sandbox's own file. A session may edit it, and it syncs like any
other file, so a change made on one machine reaches the others. On the host
it is `<data>/home/.config/caboose/sandbox.toml` (`<data>` is the
environment's data dir, `~/.caboose/envs/default/data` for the default one).
The host's own settings — the roots, the image, the engine — are in
[`config.toml`](configuration.md), which never syncs and which the sandbox
cannot write: nothing in the sandbox config can name a host path or an
image.

The first launch writes it from caboose's defaults, in full, so what the
file says is what happens. With a [sync remote](sync.md) set, the first sync
writes it instead, unless another machine's arrives: two machines writing
their own would only conflict.

## Keep entries

```toml
[[keep]]
path = "~/.cargo"

[[keep]]
path = "~/.npmrc"
file = true
```

Each entry is a directory under `~`, kept in the data dir at its path under
`<data>/home/` and bind-mounted back when the container is created. So a
new or changed entry takes effect at the next `caboose restart`; a launch,
`caboose status` and `caboose doctor` say when the container differs.

- **Where:** directly in `~`, `~/.config`, `~/.local`, `~/.local/share` or
  `~/.cache`. Docker creates a mount's missing parent directories as root,
  which would leave the tool unable to write next to it. To keep part of a
  deeper directory, keep its parent and sync only what you want of it.
- **Not caboose's own:** an entry may not be, hold or sit inside
  `~/.local/bin`, `~/.local/share/claude`, `~/.cache/claude`,
  `~/.local/state` (cleared on purpose), `~/.caboose-sync` or
  `~/.caboose-proposals`. Entries may not overlap each other either: one
  mount cannot sit inside another, so the later one is skipped.
- **Directories, preferably:** `file = true` keeps a single file, for a file
  directly in `~` (`~/.npmrc`), whose directory, the home itself, cannot be
  kept. Many tools save a file by writing a new one and renaming it over the
  old, which fails on a single-file mount with "Device or resource busy";
  `caboose doctor` notes each file entry for that reason.
- **Required:** `~/.claude`, `~/.claude.json` and `~/.config/caboose` are
  kept whatever the file says; one that is missing from it is added, and
  `caboose doctor` says so.
- **Never at the host's path:** what is kept lives in the data dir, never at
  the host's own `~/.cargo`: the sandbox reaches no more of the host than it
  did. For a directory shared with the host, use a
  [root](configuration.md#the-repo-root).

The defaults keep `~/.claude`, `~/.claude.json`, `~/.config/caboose` (this
file, [`start.d` and `shell.d`](startup.md)), `~/.config/git`,
`~/.config/jj`, `~/.config/gh` (gh's token: never synced) and `~/.ssh`
(`known_hosts` and ssh's config; keys stay on the host, reaching the sandbox
through the [forwarded agent](ssh.md)).

A directory no entry names any more is left in `<data>/home/`, with what it
holds.

## Sync rules

A keep entry says what of it syncs:

```toml
[[keep]]
path = "~/.config/caboose"
sync = true                       # all of it

[[keep]]
path = "~/.claude.json"
file = true
sync = { merge = "json", keys = ["mcpServers"] }   # all of it, with options

[[keep]]
path = "~/.claude"
  [[keep.sync]]                   # only what these match
  path = "~/.claude/projects/-work*/memory/MEMORY.md"
  merge = "union"
  [[keep.sync]]
  path = "~/.claude/projects/-work*/memory"
  [[keep.sync]]
  path = "~/.claude/skills"
  exclude = ["~/.claude/skills/synced", "*.tmp"]
```

With no `sync` (or `sync = false`), the entry is kept here and never leaves
the machine.

| field | values | default | meaning |
|---|---|---|---|
| `path` | a full path under `~`, globs allowed | required | a file, or a directory and everything under it; it must start with its keep entry's path, spelled out |
| `merge` | `text`, `json`, `union` | `text` | `text` is git's three-way merge, line by line; `json` merges two documents key by key; `union` keeps the lines each side added, for a file that is a list |
| `keys` | top-level key names | all | with `json`: only these keys of each file sync, the rest stay on the machine |
| `exclude` | paths (`~/...`) or names (`*.tmp`) | none | left out of what the rule matches; a name matches at any depth |

**Matching.** `*` matches within one path component, `**` across any number
of them, and both match names starting with a dot. A pattern matches a file,
or a directory and everything under it. **The first rule of an entry that
matches a file decides** how it merges: list a special case before the
general one. `caboose doctor` notes a rule that never decides anything
because earlier ones take all it matches.

Adding a merge driver is a change to caboose; adding a path is a line here.
`caboose sync add PATH` and `caboose sync rm PATH` edit the file for you,
and `caboose sync status` shows what a sync would send and take.

Whatever a rule says, `~/.claude/.credentials.json` (the Claude login) never
syncs, and every file a sync would send is scanned for credentials first.

### Syncing part of a tool's config

When only part of a file should travel, split it with the tool's own include
mechanism rather than a merge rule. For git:

```ini
# ~/.config/git/config: kept, not synced; this machine's own
[include]
    path = shared
[user]
    signingkey = ssh-ed25519 AAAA...

# ~/.config/git/shared: synced whole, merged as text
[user]
    name = You
    email = you@example.com
[alias]
    st = status
```

```toml
[[keep]]
path = "~/.config/git"
  [[keep.sync]]
  path = "~/.config/git/shared"
```

The same works for ssh (`Include`), jj (its config directory) and shells
(`source`).

## Roots

```toml
roots = ["dev", "oss"]
```

The names of the [roots](configuration.md#the-repo-root) the projects
expect. Claude Code keys a project's memory by its path, `/work/<root>/...`
in the container, so memory synced from a machine with a root `oss` is never
read on one without it. `caboose doctor` says when this machine lacks one;
`caboose setup roots` adds it.

## Versions

```toml
format = 1      # this file's structure
defaults = 1    # the caboose defaults it was written from
```

When a newer caboose adds a default, `caboose doctor` says so, and
`caboose sandbox-config update` offers each one; the file is never changed
without asking. A file of a newer `format` than a caboose reads is not used:
that caboose keeps using the last copy it could read, syncs nothing, and
says `caboose update`.

## When something is wrong with it

The file arrives by sync, so a mistake made on one machine must not stop
another. A bad entry or rule is skipped and the rest applies. A file that
does not parse at all is replaced by the last copy the host could read (or
the defaults), and nothing syncs until it is fixed, so the old rules cannot
undo the new ones on every machine.

What is wrong is reported where both sides look: `caboose doctor`, a line at
launch, and, for sessions, `~/.caboose-proposals/current/sandbox-config.txt`,
which the host rewrites at every launch.
