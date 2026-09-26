# How it works

**One long-lived container holds every session.** The agents/fleet feature
registers sessions by PID and by unix socket under `/tmp`, so sessions in
separate containers cannot see each other. Each project gets its own tmux
session inside the single container, so closing the terminal detaches instead
of killing the work, and background agents keep running.

**Claude Code is not in the image.** It installs on first run into
`~/.local` — bind-mounted from the data dir's `dot_local/<platform>`, one
per libc and arch — so it updates itself in place and survives image
rebuilds. The container itself holds no state: everything persistent is
bind-mounted out of the [data dir](#persistent-state-lives-outside-the-checkout),
`~/.caboose/envs/default/data`, which makes `caboose restart` safe at any
time apart from killing running sessions.

**Repos are mounted at fixed paths.** The repo root is `/work` inside the
container, whatever it is on the host, and each session's cwd is the host
cwd's place under it: `/Users/you/dev/you/project` is `/work/you/project`,
and so is `/home/you/dev/you/project` on another machine. Claude Code keys
per-project state (memory, transcripts, trust) off the cwd, so a project
has the same key on every machine, which is what lets
[`caboose sync`](sync.md) carry its memory across as it
is.

## Persistent state lives outside the checkout

Everything the sandbox account keeps — the live OAuth credential, prompt
history, session transcripts, the ~224MB Claude Code binaries, and the
sandbox's own git, jj, gh and ssh config — lives in the environment's data dir,
`~/.caboose/envs/default/data` for the default one (`<data>` below;
`CABOOSE_DATA_DIR` names one outright), bind-mounted in piece by piece:

| on the host | in the container |
|---|---|
| `<data>/.claude` | `~/.claude` |
| `<data>/.claude.json` | `~/.claude.json` |
| `<data>/dot_local/<platform>/bin` | `~/.local/bin` |
| `<data>/dot_local/<platform>/share/claude` | `~/.local/share/claude` |
| `<data>/dot_local/<platform>/cache/claude` | `~/.cache/claude` (update staging) |
| `<data>/dot_config/git` | `~/.config/git` (git's global config) |
| `<data>/dot_config/jj` | `~/.config/jj` (jj's user config) |
| `<data>/dot_config/gh` | `~/.config/gh` (gh's config and token; `0700`) |
| `<data>/dot_ssh` | `~/.ssh` (`known_hosts` and the sandbox's own ssh `config`; `0700`) |
| `<data>/persist/<name>` | each directory [`[persist]`](#keeping-more-of-the-home) names (`0700` when new) |
| `<data>/sync` | `~/.caboose-sync` ([`caboose sync`](sync.md)'s git repo) |

Deleting the container loses nothing; deleting this directory is a fresh
install, login included.

`<platform>` is the image's, as the image check found it: `linux-arm64` for
the default image on Apple silicon, say (see
[Switching between images](images.md#switching-between-images)). Only
Claude Code's own binaries are split that way; everything else is shared by
every image. `caboose status` shows the platform the container has mounted,
its `local dir`, and the disk used by each platform dir.

git, jj and gh config is mounted as **directories** at their XDG paths, not
as `~/.gitconfig` and `~/.jjconfig.toml` files. These tools save by writing a
temp file and renaming it over the original, and a rename onto a single-file
bind mount fails with `EBUSY`.

`<data>/dot_config/git/config` starts empty; `caboose setup git` fills in
the identity and signing (see [Setup](configuration.md#git-identity-and-commit-signing)).
Only those: copying the host's config wholesale would drag in an
osxkeychain credential helper and `includeIf` paths that do not exist in
the container.

`~/.ssh` is kept for its `known_hosts`, so a host key accepted in the
sandbox stays accepted after `caboose restart`, and for an ssh `config` of
the sandbox's own (host aliases, `ProxyJump`, users). It is not the host's:
a Mac's `~/.ssh/config` tends to hold `UseKeychain` or 1Password's
`IdentityAgent`, which Linux's ssh rejects or cannot use. It holds no keys
either: they stay on the host and reach the sandbox through the
[forwarded agent](ssh.md), and `caboose doctor`
notes a private key it finds there.

### Keeping more of the home

Anything else a tool keeps under the container's home is lost when the
container is recreated. To keep a directory, name it in the environment's
`config.toml`:

```toml
[persist]
aws = "~/.aws"
foo = "~/.config/foo"
```

Each entry is kept in `<data>/persist/<name>` and bind-mounted back at its
path when the container is created, so a change needs `caboose restart`
(a launch, `caboose status` and `caboose doctor` say when the container
differs). What is kept lives in the data dir, never at the host's own
`~/.aws`: the sandbox reaches no more of the host than it did, and the
host's files do not become the sandbox's. For a directory shared with the
host, use a [root](configuration.md#the-repo-root).

An entry is a directory written `~/<dir>`, directly in `~`, `~/.config`,
`~/.local`, `~/.local/share` or `~/.cache`: Docker creates a mount's
missing parent directories as root, which would leave the tool unable to
write next to it. It may not be, hold or sit inside one of caboose's own
mounts (`~/.claude`, `~/.config/git`, `~/.ssh`, ...), nor `~/.local/state`,
which is cleared on purpose. The name is the directory's in the data dir,
so renaming one starts it empty; a directory no entry names any more is
left in `persist/`, with what it holds, and `caboose doctor` says so.
[`caboose sync`](sync.md) does not carry these.

The launcher also installs a global `CLAUDE.md` into `<data>/.claude/` on
every launch, telling every session in the sandbox what it is running in. It
comes from `sandbox/CLAUDE.md` — the checkout's copy when the launcher runs
from one, else the copy embedded in the binary — with where the roots are
mounted and the location of caboose's own source filled in.

## tmux is configured to be invisible

tmux here is a session-persistence mechanism wrapped around a full-screen TUI,
not an interactive multiplexer, so it is configured to own nothing:

- **No prefix key.** `C-b` is tmux's default prefix *and* readline's
  backward-char, so tmux intercepting it breaks cursor movement in a way
  that looks like a Claude Code bug. `prefix`/`prefix2` are `None` and the
  prefix key table is emptied, so no keystroke reaches tmux at all.
- **Mouse off.** With `mouse on`, tmux swallows scroll and clicks to drive its
  own copy-mode instead of passing them to the application.
- **No status bar**, and `escape-time 0` so ESC handling is not laggy.
- **Extended keys on**, so modified keys (Shift+Enter, Ctrl+Enter) pass
  through as CSI-u rather than arriving as a bare Enter.
- **Synchronized output on**, so a full repaint is committed as one frame.

Because there is no prefix, detach by closing the terminal (the session
survives) or with `caboose detach` from another one. Any tmux command is
still reachable from the host: `docker exec caboose tmux <cmd>`.

## Terminal rendering

`docker exec -t` hardcodes `TERM=xterm` (8 colors) and drops `COLORTERM`, so
without help a truecolor TUI renders in 8 colors under a tmux status bar. The
launcher forwards the real `TERM` (falling back to `xterm-256color` when the
container has no terminfo entry for it) plus `COLORTERM`, and `tmux.conf`
turns the status bar off and declares RGB support. Inside tmux you get
`TERM=tmux-256color`, `COLORTERM=truecolor`, 256-entry terminfo and truecolor
escapes.

The layer also sets `LANG=C.UTF-8`, unless the base sets its own. Stock
images tend to leave it unset, which puts everything in the C locale;
`tmux -u` covered the normal path, so the gap showed up only under
`CABOOSE_NO_TMUX=1` — the one path you reach for when rendering already
looks wrong.

Terminals that ship their own terminfo (Ghostty, Kitty, WezTerm) are in no
distro package, so the launcher compiles the host's description into the
container's `~/.terminfo` the first time it sees an unknown `TERM`, instead
of downgrading to `xterm-256color` forever. It is best-effort: if the host
cannot describe the terminal, or the image has no `tic`, the generic
fallback still applies.
`~/.terminfo` is not bind-mounted — it is cheap to rebuild and re-imports
after a `caboose restart`.

`tmux.conf` also declares `sync` (synchronized output), so a repaint lands
as one frame rather than tearing, whether or not the terminfo made it in.

If it still looks wrong, `CABOOSE_NO_TMUX=1 caboose` bypasses tmux
entirely to compare against native rendering.
