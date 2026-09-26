# Running inside the caboose sandbox

> This file is a copy, installed by the host launcher on every start — edits
> to *it* are overwritten at the next launch. **Edit the tracked original**,
> `sandbox/CLAUDE.md` in the caboose repo (path below).
>
> Keep this file to what is true in *every* project. Anything about how the
> sandbox itself is built or changed belongs in that repo's own CLAUDE.md.

This Claude Code instance is running inside the `caboose` Docker sandbox,
not directly on the host machine.

## The container is long-lived, and sessions are tmux sessions

The container is **not** started and thrown away per session. One container
holds every session, because the agents/fleet feature registers sessions by
PID and by unix socket under `/tmp` — sessions in different containers cannot
see each other. Each project gets a tmux session, so closing the terminal
detaches rather than killing the work, and background agents keep running.

Sessions are per project *and* per terminal. The first `caboose` in a repo
creates `<project>`; a second one opened while the first is still up creates
`<project>-2`, because a session someone is attached to is never taken over.
Close a terminal and run `caboose` there again and it reattaches to the
session it left, so the numbering stays put instead of climbing.

`caboose --session NAME` (or `CABOOSE_SESSION=NAME caboose`) names a
session outright and skips that search — it attaches if the name exists and
creates it otherwise, which is also how to put two terminals on one session
on purpose. Either way every session lives in the same container, so they all
see each other in the fleet roster.

## Repos are mounted under /work

The host directories that hold the projects are mounted at fixed paths:
@@CABOOSE_ROOTS@@. A project lives at its path under one of them, which is
the same on every machine whatever the host's user name or layout — so a
host path someone pastes in is not a path in here: translate it through
that mapping. Nothing in here is reachable at a second path, so the path
you are given (in here) is the path to edit.

## Claude Code is not part of the image

It is installed in `~/.local` (bind-mounted out of `~/.caboose` on the host,
which is where all sandbox state lives — outside any checkout), so it
**updates itself in place** and survives image rebuilds. `claude` on `$PATH`
is that persisted binary.

What else is installed depends on the base the image was built on: caboose's
default, or one the user chose (`CABOOSE_BASE_IMAGE`). caboose installs
nothing into a base, so a missing tool is the image's to add, on the host —
not something to install from in here: propose it (below).

## Most of the home does not survive a restart

`caboose restart` recreates the container. What outlives it is what is
mounted from the data dir: `~/.claude`, `~/.claude.json`, Claude Code in
`~/.local`, git, jj and gh config under `~/.config`, and `~/.ssh`
(`known_hosts` and the sandbox's own ssh `config`; keys stay on the host,
through the forwarded agent). Anything else under the home is lost, unless
the environment's `config.toml` names its directory in `[persist]` — a
host-side setting: propose it (below) rather than work around it from in
here. `mount` in here, or `caboose status` on the host, shows what is kept.

## Changing the sandbox: propose it, the user applies it

Nothing in here can change the image, `config.toml` or the roots: they
decide what the sandbox is and what of the host it reaches. When asked to
install a tool for good, keep a tool's settings across restarts, or mount
another host directory, write a **proposal**; the user reviews it on the
host with `caboose apply`, which applies it only once they say yes.

First read `~/.caboose-proposals/current/`: `state.toml` (where the
Dockerfile comes from, its hash, the roots and `[persist]` entries there
are now) and `Dockerfile` (the one the next build uses). Then write
`~/.caboose-proposals/NAME.toml` (NAME: lowercase letters, digits, `-`,
`_`), with any of the three parts:

```toml
title = "Install foo"                  # one line
reason = "Why, in a sentence or two."
dockerfile_sha256 = "..."              # from state.toml; needed with [section]

[section]                              # a Dockerfile section: root, after the base
name = "foo"                           # replaces a section of that name, else is added
title = "foo 2.3"
body = '''
ARG FOO_VERSION=2.3.0
RUN curl -fsSL https://example.com/foo-${FOO_VERSION}.tgz | tar -xz -C /usr/local/bin foo
'''

[persist]                              # directories of the home to keep
foo = "~/.config/foo"                  # directly in ~, ~/.config, ~/.local, ~/.local/share or ~/.cache

[roots]                                # one host directory to mount, at /work/NAME
other = "~/src/other"
```

- One request, one proposal, and one `[section]` in it: applying a section
  changes the Dockerfile's hash, and a proposal written against the old one
  is refused. Re-read `current/` before proposing again.
- A section cannot hold `FROM`, `ONBUILD`, `RUN --network` or
  `RUN --security`, nor `# caboose:` lines. Plain text only: no control
  characters (escape sequences) or invisible ones anywhere.
- Nothing else can be proposed — not the base image, the docker socket, or
  any other setting. For those, tell the user what to change on the host.
- Then tell the user to run `caboose apply` in a host terminal (with the
  same `-e ENV` as this session's, if it has one). It rebuilds the image
  and offers `caboose restart`, which ends this session: nothing proposed
  is in effect before that, and a directory proposed for `[persist]` starts
  empty, so configure the tool only after the restart.
- No `~/.caboose-proposals` means the container is older than proposals:
  `caboose restart` on the host creates it again with one.

## The sandbox itself is just another repo

The project that defines this container — image, entrypoint, host launcher —
is an ordinary repo, and from in here it is:

    @@CABOOSE_DIR@@

**If asked to fix, change or improve the sandbox, container, image or host
launcher** (as opposed to whatever project is under work), that is where the
changes go. It carries its own CLAUDE.md with the rest — notably that most of
it only takes effect after a rebuild, run *on the host*.

Host launcher commands, all run from a host terminal rather than in here:
`caboose doctor`, `status`, `restart`, `stop`, `shell`, `logs`, `build`,
`check-image`, `version`, `update`, `prune`, `detach`, `sync`, `env`, `help`.
An install made by `install.sh` keeps itself up to date; a newer launcher
works on with this container until `caboose restart` moves it onto a new
image.
`caboose doctor` is the one to point someone at when the sandbox itself
seems wrong: it lists each problem with the command that fixes it.
`caboose claude ARGS` passes ARGS to `claude`; nothing else reaches it, and
`caboose --help` lists the rest. `caboose --env NAME` (or
`CABOOSE_ENV`) picks an environment: a separate caboose, with its own
container, image, data and Claude login, so sessions in another
environment are not in this container and not in the fleet roster.
