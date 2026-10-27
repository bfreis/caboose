---
name: caboose-propose
description: Use when the user wants a tool, package or command installed for good in the caboose sandbox, a change to its image or Dockerfile, another host directory mounted (a new root), a directory of a root made writable, a few settings (time zone, hostname, tmux, auto_build, VM size), a named command on the host the sandbox may run (an alias), or any change to the sandbox that a session cannot make itself (ports, URLs, host commands, isolation, VM size, docker run flags, engine socket, network egress). Writes a proposal to ~/.caboose-proposals/ for the user to review with caboose apply on the host, and says which config.toml key the user changes for what cannot be proposed.
---

# Proposing a change to the sandbox

A session cannot change what the sandbox is: the image, the roots, the host's
`config.toml`. It writes a **proposal**; the user reviews it on the host
with `caboose@@CABOOSE_ENV_FLAG@@ apply`, and nothing is applied until they
say yes. What the sandbox keeps of its home (`[[keep]]`), `start.d` and
`shell.d` are not proposals: see the caboose-persist skill.

## 1. Read the current state

Read `~/.caboose-proposals/current/state.toml`, written by the host at every
launch and after every `apply`. It is the authority, not these instructions:

- `image`: the image profile, `"<kind>.<name>"`. The kind decides what can be
  proposed (step 2). At this launch it was `@@CABOOSE_IMAGE@@`.
- apko: the profile's own `packages`, `defaults`, and every package
  `installed`.
- dockerfile: `dockerfile` (`"dockerfile"`, the profile's own, or `"seed"`,
  caboose's, when the profile has none yet), its `dockerfile_sha256`, and the
  file itself as `current/Dockerfile`.
- `[roots.NAME]`: each root's `host` path, container `path`, and
  `writable`: the parts of it the sandbox may write (`"."` for all of it);
  the rest is read-only.
- `[settings]`: the values a settings proposal changes, as `config.toml` has
  them now (`tz` and `hostname` are `""` when unset), `isolation` (the
  kind in use) and `vm_profile` (the `[vm.NAME]` that `cpus` and `memory`
  are set in, `""` when `config.toml` defines none).

## 2. Write ~/.caboose-proposals/NAME.toml

NAME: lowercase letters, digits, `-` and `_`, starting with a letter or
digit, at most 32. One request, one proposal. A proposal holds a `title`
(one line, at most 100 characters), a `reason` (a sentence or two), and the
image's part for its kind, one root, or both; or one alias, alone; or one
writable part, alone; or settings, alone.

@@IF image=apko@@
### apko image: packages

```toml
title = "Install graphviz"
reason = "graphviz renders the project's diagrams."

[packages]
add = ["graphviz"]
remove = []
```

- Wolfi package names, 64 at most across `add` and `remove`. Do not add one
  that is in `installed`; remove only one of the profile's own `packages`
  (dropping one of caboose's groups is `caboose@@CABOOSE_ENV_FLAG@@ setup image`, on the host).
- To find a name, fetch the index,
@@IF isolation=vm@@
  `https://packages.wolfi.dev/os/aarch64/APKINDEX.tar.gz` (the VM is arm64).
@@ELSE@@
  `https://packages.wolfi.dev/os/<aarch64|x86_64>/APKINDEX.tar.gz` (`uname -m`).
@@END@@
  Its `P:` lines are package names; `cmd:NAME` on a package's `p:` line
  says it provides the command NAME.
- Within seconds the host link writes `NAME.check` beside the proposal. Its
  first line is `ok` (with what the list resolves to), `error` (with why; an
  unknown name comes with close ones: fix the proposal until it says `ok`),
  or `unchecked` (the host could not check now: do not rewrite the proposal
  for that, tell the user; `apply` checks anyway). No `.check` appears while
  the link is down.
@@END@@
@@IF image=dockerfile@@
### dockerfile image: a section

```toml
title = "Install foo 2.3"
reason = "The project's build needs foo."
dockerfile_sha256 = "..."        # from current/state.toml

[section]
name = "foo"                     # replaces the section of that name, else is added
title = "foo 2.3"
body = '''
ARG FOO_VERSION=2.3.0
RUN curl -fsSL https://example.com/foo-${FOO_VERSION}.tgz | tar -xz -C /usr/local/bin foo
'''
```

- Read `current/Dockerfile` first: that is what the section goes into. With
  `dockerfile = "seed"` it is caboose's seed, which becomes the profile's
  own Dockerfile on apply. A new section goes at the end of the file, so it
  runs as root only if no earlier `USER` line says otherwise.
- `name`: `[a-z0-9-]`, 1 to 32 characters. `title`: one line, at most 100
  characters, not starting with `off`.
- The section must build on its own: its own `ARG`s and `ENV`s. It cannot
  hold `FROM`, `ONBUILD`, `RUN --network`, `RUN --security`, nor
  `# caboose:` lines.
- One `[section]` per proposal, and never with `[packages]`. Applying one
  changes the Dockerfile's hash, so a proposal written against the old one
  is refused: re-read `current/` before proposing again.
@@END@@
@@IF image=ref@@
### ref image: nothing in the image

A ref profile is an image of the user's own. Nothing in it can be proposed:
tell the user what to add to their image (a package name, the lines of their
Dockerfile). A root can still be proposed.
@@END@@

### If state.toml names another kind

The image may have changed since this launch. In brief:

@@IF image!=apko@@
- **apko**: a `[packages]` table, `add` and `remove`, Wolfi names, 64 at
  most; not one already `installed`, and remove only the profile's own.
  Look names up in `https://packages.wolfi.dev/os/<aarch64|x86_64>/APKINDEX.tar.gz`
  (`P:` names, `cmd:NAME` on `p:` lines), then read `NAME.check` beside the
  proposal and fix it until its first line is `ok` (`unchecked`: leave it).
@@END@@
@@IF image!=dockerfile@@
- **dockerfile**: `dockerfile_sha256` from `state.toml` and a `[section]`
  with `name`, `title` and a `body` of Dockerfile lines that builds on its
  own (its own `ARG`s), into `current/Dockerfile`. No `FROM`, `ONBUILD`,
  `RUN --network`, `RUN --security` or `# caboose:` lines; one section per
  proposal, never with `[packages]`.
@@END@@
@@IF image!=ref@@
- **ref**: the user's own image; nothing in it can be proposed. Tell them
  what to add to it.
@@END@@

### A root: another host directory

```toml
[roots]
other = "~/src/other"            # mounted at /work/other
```

One root per proposal, by name (lowercase letters, digits, `-`, `_`) and
an absolute host path or one starting with `~/`. The host refuses `/`, the
home itself or anything holding it, anything in a hidden directory of the
home or in `~/Library`, caboose's own state, an overlap with a root or a
mount, a name already used, and a path that does not exist or is not a
directory. It warns about what looks like credentials, and asks the user to
type the name.

A proposed root is mounted read-only. To make a directory of it writable,
propose that on its own once the root is there (below).


If `state.toml` shows a single root mounted at `/work` itself, adding one
moves that root to `/work/NAME`: every project path changes, and Claude
Code's per-project state (memory, resumable conversations) stays under the
old path. Tell the user before they apply it.

### An alias: a host command sessions may run

```toml
title = "Keep the host awake during long builds"
reason = "the overnight build stops when the Mac sleeps."

[alias.awake]
command = ["/usr/bin/caffeinate", "-i"]   # absolute program path, then fixed arguments
description = "keep the host awake until interrupted"
policy = "ask"                            # "ask" or "allow"; absent is "allow"
# timeout = "2h"                          # a Go duration; absent or "0": none
# cwd = "~/src/proj"                      # absent: a fresh empty directory

# [alias.awake.params]                    # enum parameters: "{mode}" in command
# mode = ["idle", "disk"]                 # is replaced by the value asked for
```

An alias is alone in its proposal. Its command is a program on the host,
run with exactly that argv (each `{KEY}` element replaced by a listed
value), never through a shell. The host refuses, before the user sees it:
a command that is a shell or runs what its arguments say (`sh`, `bash`,
`env`, `python`, `node`, `osascript`, `open`, `xargs`, `make`, `ssh`, ...),
one that does not exist there, one in a root, in anything a sandbox can
write, in a hidden directory of the home or in caboose's state; a `cwd` in
caboose's state or a hidden directory of the home; and anything beyond
these keys. So propose the program itself with its fixed flags, not a
script in a project. `caboose-agent aliases` lists the aliases there are
(names and parameters, never commands); `caboose-agent run NAME
[KEY=VALUE...]` runs one once applied, no restart needed. It exits 75 with
`pending #N` when the host asks the user first: note the number and carry
on.

### A writable part: a directory of a root the sandbox may write

```toml
title = "Let the build write its output"
reason = "make writes into build/, which is read-only now."

[writable]
path = "/work/dev/proj/build"    # the directory as the sandbox sees it
```

Alone in its proposal: one path, absolute and clean, under `/work`, as it
is seen in here. Check `writable` in `state.toml` first: a path already
writable (the whole root is, or a part holding it) is refused. The host
also refuses a path in no root, one that does not exist or is not a
directory, one reached through a symbolic link, the home or anything
holding it, anything in a hidden directory of the home or `~/Library`, and
caboose's own state. It warns about what looks like credentials, and asks
the user to type the root's name. Nothing changes in this sandbox: the
directory is writable only after `caboose@@CABOOSE_ENV_FLAG@@ restart`,
which ends every session. Ask for the narrowest directory the task needs.

### Settings: a few keys of config.toml

```toml
title = "Use Lisbon time, and a bigger VM"
reason = "build logs are in UTC, and the builds swap."

[settings.session]
tz = "Europe/Lisbon"       # also hostname = "sbx", tmux = false
[settings.build]
auto_build = false
[settings.vm]              # the VM profile config.toml uses; you never name it
cpus = 6
memory = "12G"
```

Alone in its proposal, and only these six keys (dotted keys, like
`session.tz = "UTC"` under `[settings]`, are the same): `session.tz` (a zone
name such as `Europe/Lisbon`), `session.hostname` (one DNS label),
`session.tmux`, `build.auto_build`, `vm.cpus` and `vm.memory`. Any other key,
or any other `config.toml` setting, refuses the proposal whole. The VM keys
are refused when the isolation in `state.toml` is not `vm`, when it has no
`vm_profile`, and when they ask for more CPUs or memory than the host has;
a value `config.toml` has already is left out. The host shows the change to
`config.toml` as a diff and says when each key takes effect: `tz`, `tmux`
and `auto_build` need no restart (new sessions, the next launch), `hostname`
and the VM's size take `caboose@@CABOOSE_ENV_FLAG@@ restart`.

### Everywhere

Plain text only: no control characters (escape sequences) and no invisible
ones (bidi overrides and the like). A proposal with anything else, or any
other key, is refused whole.

## 3. Tell the user

Say what you proposed and that they run `caboose@@CABOOSE_ENV_FLAG@@ apply`
in a host terminal (the host also shows a notification, which may be
missed). `apply` shows the proposal whole, builds packages before writing
anything (a list that does not build stays pending), and offers
`caboose@@CABOOSE_ENV_FLAG@@ restart`. **That restart ends every session, this one included**:
nothing proposed is in effect before it. A tool whose settings should last
needs a `[[keep]]` entry too (caboose-persist skill), added before that
same restart.

## What cannot be proposed

Everything else is the host's `config.toml`, which only the user edits (or
`caboose@@CABOOSE_ENV_FLAG@@ setup` asks about). Name the table and key.
`[link]` keys apply within seconds; the rest take
`caboose@@CABOOSE_ENV_FLAG@@ restart` unless the table says otherwise.

| What | Where in config.toml |
|---|---|
| which image profile, which isolation | top-level `image = "<kind>.<name>"`, `isolation = "<kind>.<name>"` |
| the adversary assumed | top-level `trust = "fallible"` or `"hostile"` (hostile: vm only) |
| what the host checkpoints | `paths` in `[checkpoints]` (no restart: the daemon reads it again) |
| whether creating the sandbox builds a missing or stale image | `auto_build` in `[build]`, or a `[settings]` proposal as above |
| an image profile | `[apko.NAME]` `packages`, `defaults`; `[dockerfile.NAME]` (its Dockerfile in the environment's `dockerfile/NAME/`); `[ref.NAME]` `image` |
| a capability, a device, DNS, any other `docker run` flag | `run_args` in `[container.NAME]` or `[gvisor.NAME]` |
| the host's Docker engine inside | `engine_socket = true` in `[container.NAME]` or `[gvisor.NAME]`: root-equivalent on the host |
| VM size | `cpus`, `memory` in `[vm.NAME]`, or a `[settings]` proposal as above |
| VM outbound proxy | `egress`, `egress_ports`, `egress_allow`, `egress_deny` in `[vm.NAME]` (all but `egress` apply without a restart) |
| forwarded ports, URL opening, any host command, checkpoints asked for from here | `forward_ports`, `open_urls`, `open_urls_allow`, `host_exec`, `checkpoint_requests` in `[link]` (within seconds); for one port, `caboose-agent request port --port N` also asks the host (caboose-requests skill) |
| a named host command | `[alias.NAME]`, or a proposal as above (within seconds) |
| which SSH agent | `ssh_agent` in `[link]`: within seconds under vm; on Linux under container or gvisor a mount, so a restart; with OrbStack or Docker Desktop only `"none"` counts (a restart) |
| roots | `[roots]`, or a proposal as above |
| what of a root the sandbox may write | `writable` in its `[roots]` entry, `caboose setup roots`, or a proposal as above; a restart |
| tmux, time zone, hostname | `tmux`, `tz`, `hostname` in `[session]` (`tmux`, `tz`: the next session; `hostname`: a restart), or a `[settings]` proposal as above |

@@IF isolation=vm@@
Under vm there is no `run_args` and no engine socket: root in the VM already
has every capability, and the VM runs its own dockerd. The kernel is
caboose's own build, fixed: TUN, FUSE, BPF and user namespaces are on;
modules and io_uring are off. A missing kernel feature is a change to caboose
itself, not to the user's configuration.
@@END@@
@@IF isolation=container,gvisor@@
Under vm (another isolation) there is no `run_args` or engine socket: a VM
profile sizes the VM and runs its own dockerd.
@@END@@
