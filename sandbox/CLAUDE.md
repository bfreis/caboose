# Running inside the caboose sandbox

> These are caboose's managed instructions, installed read-only at
> `/etc/claude-code/CLAUDE.md`, where Claude Code reads them before any
> other CLAUDE.md. The host launcher regenerates them on every launch from
> the tracked original, `sandbox/CLAUDE.md` in the caboose repo (path
> below): **edit that**, never this copy. `~/.claude/CLAUDE.md` is the
> user's own, and caboose never writes it.
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

## Repos are mounted at fixed paths

The host directories that hold the projects are mounted at fixed container paths, `/work/<name>` unless a root was given a path of its own:
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
default, or one the user chose (`base` in `[image]` of the environment's `config.toml`). caboose installs
nothing into a base, so a missing tool is the image's to add, on the host —
not something to install from in here: propose it (below).

## What survives a restart: ~/.config/caboose/sandbox.toml

`caboose restart` recreates the container. What outlives it is Claude Code
in `~/.local`, and whatever the **sandbox config**,
`~/.config/caboose/sandbox.toml`, keeps: by default `~/.claude`,
`~/.claude.json`, `~/.config/caboose`, git, jj and gh config under
`~/.config`, and `~/.ssh` (`known_hosts` and ssh's config; keys stay on the
host, through the forwarded agent). Anything else under the home is lost.
`mount` in here, or `caboose status` on the host, shows what is kept.

The sandbox config is yours to edit when asked, with no proposal: what it
keeps lives in caboose's data dir on the host, never at a host path, so it
reaches nothing more. It also says what of that `caboose sync` carries to
the user's other machines, and it syncs itself, so a change reaches them
too. The file explains its own fields. To keep a tool's settings, add

```toml
[[keep]]
path = "~/.foo"          # directly in ~, ~/.config, ~/.local, ~/.local/share or ~/.cache
sync = true              # only if asked to sync it too
```

Then read `~/.caboose-proposals/current/sandbox-config.txt`: when it exists,
it lists what is wrong with the file, as of the host's last launch, which
also rewrites it. A new `[[keep]]` entry takes effect at the next
`caboose restart` on the host, and starts empty: say so, and configure the
tool only after it. Never sync anything that holds a token.

## Reaching the host: ports, URLs, notifications, commands

A server listening in here is forwarded to the same port on the host's
`localhost` while it runs, if `forward_ports` in the host's `config.toml` allows the port
(by default 3000-3999, 5173 and 8000-8999), whatever address it is bound
to: tell the user to open `http://localhost:PORT`. `caboose-agent ports`
lists what listens and what is forwarded. `caboose-agent open URL` opens an
http(s) URL in the user's browser (usually after a dialog there), and
`caboose-agent notify TEXT` shows them a notification. All of it needs the
host's `caboose link`, which every launch starts; "no host is linked"
means it is not running.

In a VM sandbox (`CABOOSE_ISOLATION=vm` is set in it), outbound connections go through
the host by default: `HTTPS_PROXY` (and the rest) is then
`http://127.0.0.1:9128`, and what the host reaches, VPN included, this
reaches, on the ports its `vm` profile's `egress_ports` allows (22, 80 and 443 by
default). Private, LAN, loopback and tailnet addresses are refused, with
an HTTP 403 saying why, unless its `egress_allow` names them. ssh
goes the same way, through `caboose-agent connect %h %p` as its
`ProxyCommand`. Containers run by the sandbox's own dockerd do not: they
stay on the VM's NAT.

### Running commands on the host

@@CABOOSE_HOST_EXEC@@

## Start-up scripts and shell config: ~/.config/caboose

Two directories there are also yours to write when asked: they run as this
user, in here, and reach nothing more. They sync to the user's other
machines, with the rest of `~/.config/caboose`.

- `start.d/`: executables run once at container start, one at a time in
  name order (`10-foo` before `20-bar`), in `~`; a daemon is started in the
  background by its script (`foo &`), or the ones after it wait. Output is
  in `caboose logs` on the host. Run one by hand to try it now.
- `shell.d/`: read by every interactive bash, this tool's snapshot of the
  shell included. `*.sh` must work in bash and zsh alike (aliases, exports,
  plain functions); `*.bash` is for bash only. A new file reaches the next
  shell. An alias here changes what your own commands do, so add only what
  was asked for. `PATH` is the exception: this tool's shell takes it from
  Claude Code, not from `shell.d`, so a directory added there reaches the
  user's shells but not yours. A command for both goes in `~/.local/bin`,
  first on every `PATH` and kept.

Say what you wrote, and that a `start.d` script runs at the next start
(`caboose restart` on the host), unless it was run by hand.

## Changing the sandbox: propose it, the user applies it

Nothing in here can change the image, `config.toml` or the roots: they
decide what the sandbox is and what of the host it reaches. When asked to
install a tool for good, or mount another host directory, write a
**proposal**; the user reviews it on the host with `caboose apply`, which
applies it only once they say yes.

First read `~/.caboose-proposals/current/state.toml`: `image` says what
the image is built from, `"<kind>.<name>"`, and the kind says what can be
proposed for it; the roots there are now are listed too. Then write
`~/.caboose-proposals/NAME.toml` (NAME: lowercase letters, digits, `-`,
`_`): a `title`, a `reason`, the image's part for its kind, a root, or both.

```toml
title = "Install graphviz"             # one line
reason = "Why, in a sentence or two."

[roots]                                # one host directory to mount, at /work/NAME
other = "~/src/other"                # long form for another path: [roots.other] host = ..., path = ...
```

- **apko** (packages from Wolfi): a `[packages]` table, `add = [...]`
  and/or `remove = [...]`, 64 names at most. `state.toml` lists the
  profile's own `packages` (only those can be removed) and every package
  `installed` (do not add one of those). To find a name, fetch the index,
  `https://packages.wolfi.dev/os/<aarch64|x86_64>/APKINDEX.tar.gz`: its
  `P:` lines are package names, and `cmd:NAME` on a package's `p:` line
  says it provides the command NAME. Within seconds the host writes
  `NAME.check` beside the proposal. Its first line is `ok`, with what the
  list resolves to; `error`, with why (an unknown name comes with close
  ones): fix the proposal until it says `ok` before telling the user; or
  `unchecked`: the host could not check it now. Do not rewrite the
  proposal for that; tell the user, since `caboose apply` checks it
  anyway.
- **dockerfile**: a `[section]`, with `dockerfile_sha256` from
  `state.toml`; `current/Dockerfile` is the Dockerfile it goes into (when
  `state.toml` says `dockerfile = "seed"`, caboose's own, which then
  becomes the profile's).

  ```toml
  dockerfile_sha256 = "..."
  [section]                            # root, after the base
  name = "foo"                         # replaces a section of that name, else is added
  title = "foo 2.3"
  body = '''
  ARG FOO_VERSION=2.3.0
  RUN curl -fsSL https://example.com/foo-${FOO_VERSION}.tgz | tar -xz -C /usr/local/bin foo
  '''
  ```

  One `[section]` per proposal: applying one changes the Dockerfile's
  hash, and a proposal written against the old one is refused, so re-read
  `current/` before proposing again. A section cannot hold `FROM`,
  `ONBUILD`, `RUN --network` or `RUN --security`, nor `# caboose:` lines.
- **ref** (an image of the user's own): nothing can be proposed for it;
  tell the user what to add to their image.
- Plain text only, everywhere: no control characters (escape sequences)
  or invisible ones.
- Nothing else can be proposed — not the base image, the docker socket, or
  any other setting. For those, tell the user what to change on the host:
  a capability, a device or another `docker run` flag is
  `run_args` in a `[container.NAME]` or `[gvisor.NAME]` profile of the
  environment's `config.toml`, then `caboose restart`.
- Then tell the user to run `caboose apply` in a host terminal (with the
  same `-e ENV` as this session's, if it has one); the host also shows
  them a notification saying so, once the file is written, but it may be
  missed. It builds the image (packages are built before anything is
  written: a list that does not build is left pending) and offers
  `caboose restart`, which ends this session: nothing proposed
  is in effect before that. A tool installed this way whose settings
  should last needs a `[[keep]]` entry too (above), in the same restart.

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
