---
name: caboose-persist
description: Use when something in the caboose sandbox must outlive a restart or be set up at every start - keeping a tool's settings, cache or login directory (a [[keep]] entry in ~/.config/caboose/sandbox.toml), syncing it across the user's machines with caboose sync, starting a daemon or running setup at sandbox start (start.d), or adding aliases, functions, environment variables or a prompt for every shell (shell.d), or a command on every PATH (~/.local/bin). These need no proposal.
---

# Keeping state, start-up scripts and shell config

Everything here lives under `~/.config/caboose`, which is kept and, once the
user has set up `caboose sync`, synced to their other machines by default. A session may write it when
asked, with no proposal: it runs as the sandbox's user, in here, and reaches
nothing more. Say what you wrote, and when it takes effect.

@@IF isolation=vm@@
Under vm the root filesystem is fresh at every boot, so this is the only way
to have anything outside the roots and kept paths come back: a `[[keep]]`
for data, a `start.d` script for setup (it runs at every boot).
@@END@@

## Keeping a path: ~/.config/caboose/sandbox.toml

The sandbox config says what of the home is kept across restarts, and what
of that `caboose@@CABOOSE_ENV_FLAG@@ sync` carries between machines. The file explains its own
fields. To keep a tool's directory:

```toml
[[keep]]
path = "~/.foo"
sync = true              # only if the user asked to sync it too
```

- `path` is directly in `~`, `~/.config`, `~/.local`, `~/.local/share` or
  `~/.cache` (not `~/.local` or `~/.cache` themselves). For something
  deeper, keep its parent.
- Prefer a directory. `file = true` keeps a single file (`~/.foorc`), but a
  tool that saves by writing a temp file and renaming it over the original
  fails on it ("Device or resource busy").
- Not allowed: an entry overlapping another, or `~/.local/bin`,
  `~/.local/share/claude`, `~/.cache/claude`, `~/.local/state`,
  `~/.caboose-sync`, `~/.caboose-proposals`. `~/.claude`, `~/.claude.json`
  and `~/.config/caboose` are always kept.
- **Never sync anything that holds a token or a key.** Sync can also take
  only part of an entry (`[[keep.sync]]` rules with `path`, `merge`,
  `keys`, `exclude`); see @@CABOOSE_UPSTREAM@@/blob/main/docs/sandbox-config.md.
- What is kept lives in caboose's data dir on the host, never at the host's
  own `~/.foo`. For a directory shared with the host, propose a root
  (caboose-propose skill).

After editing, read `~/.caboose-proposals/current/sandbox-config.txt`: when
it exists it lists what is wrong with the file, as of the host's last
launch. A bad entry is skipped, not the whole file.

**A new entry takes effect at the next `caboose@@CABOOSE_ENV_FLAG@@ restart`, and starts
empty.** Tell the user so; configure the tool only after the restart, or
what you write now is lost. To carry existing settings over, copy them into
the entry's path after the restart (keep a copy somewhere kept, such as a
root, if needed).

On the host, `caboose@@CABOOSE_ENV_FLAG@@ sync add PATH` and `caboose@@CABOOSE_ENV_FLAG@@ sync rm PATH` edit the
same file, and `caboose@@CABOOSE_ENV_FLAG@@ sync status` shows what a sync would send and take.

## start.d: run at every sandbox start

`~/.config/caboose/start.d/` holds executables run at every start of the
@@IF isolation=vm@@
sandbox (every boot of the VM),
@@ELSE@@
sandbox (`caboose@@CABOOSE_ENV_FLAG@@ restart`, a stop and start, the engine coming back),
@@END@@
after it is ready, so they never hold up a launch:

- one at a time, in byte order of name (`10-foo` before `20-bar`), in `~`,
  with no stdin, as the session's user;
- a script that does not end holds up the ones after it, so start a daemon
  in the background in its script, redirecting its output:
  `foo >/dev/null 2>&1 &`;
- skipped: files that are not executable (`chmod +x` it), dotfiles, names
  ending in `~`, directories;
- output goes to `caboose@@CABOOSE_ENV_FLAG@@ logs` on the host; a failure is logged and the
  next one runs. Nothing restarts a daemon that dies.

To try one now, run it by hand. Otherwise it first runs at the next start:
say so.

## shell.d: every interactive bash

`~/.config/caboose/shell.d/` is read by every interactive bash: tmux windows,
`caboose@@CABOOSE_ENV_FLAG@@ shell`, and the snapshot of the shell your Bash tool takes.

- `*.sh` must work in bash and zsh alike: `alias`, `export`, plain
  functions. `*.bash` is for bash only (`shopt`, `PS1`, completions). One
  order across both, by name, the `.sh` before the `.bash` of the same name.
- A new file reaches the next shell; no restart.
- An alias here changes what your own commands do too: add only what was
  asked for.
- `PATH` is the exception: your Bash tool takes `PATH` from Claude Code, not
  from `shell.d`, so a directory added there reaches the user's shells, not
  yours. A command both should find goes in (or is linked into)
  `~/.local/bin`, which is first on every `PATH` and kept.
