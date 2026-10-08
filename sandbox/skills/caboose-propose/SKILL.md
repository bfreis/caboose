---
name: caboose-propose
description: Use when the user wants a tool, package or command installed for good in the caboose sandbox, a change to its image or Dockerfile, another host directory mounted (a new root), or any change to the sandbox that a session cannot make itself (ports, URLs, host commands, isolation, VM size, docker run flags, engine socket, network egress). Writes a proposal to ~/.caboose-proposals/ for the user to review with caboose apply on the host, and says which config.toml key the user changes for what cannot be proposed.
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
- `[roots.NAME]`: each root's `host` path and container `path`.

## 2. Write ~/.caboose-proposals/NAME.toml

NAME: lowercase letters, digits, `-` and `_`, starting with a letter or
digit, at most 32. One request, one proposal. A proposal holds a `title`
(one line, at most 100 characters), a `reason` (a sentence or two), and the
image's part for its kind, one root, or both.

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

If `state.toml` shows a single root mounted at `/work` itself, adding one
moves that root to `/work/NAME`: every project path changes, and Claude
Code's per-project state (memory, resumable conversations) stays under the
old path. Tell the user before they apply it.

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
| an image profile | `[apko.NAME]` `packages`, `defaults`; `[dockerfile.NAME]` (its Dockerfile in the environment's `dockerfile/NAME/`); `[ref.NAME]` `image` |
| a capability, a device, DNS, any other `docker run` flag | `run_args` in `[container.NAME]` or `[gvisor.NAME]` |
| the host's Docker engine inside | `engine_socket = true` in `[container.NAME]` or `[gvisor.NAME]`: root-equivalent on the host |
| VM size | `cpus`, `memory` in `[vm.NAME]` |
| VM outbound proxy | `egress`, `egress_ports`, `egress_allow` in `[vm.NAME]` (the last two apply without a restart) |
| forwarded ports, URL opening, host commands | `forward_ports`, `open_urls`, `host_exec` in `[link]` (within seconds) |
| which SSH agent | `ssh_agent` in `[link]`: within seconds under vm; on Linux under container or gvisor a mount, so a restart; unused with OrbStack or Docker Desktop |
| roots | `[roots]`, or a proposal as above |
| tmux, time zone, hostname | `tmux`, `tz`, `hostname` in `[session]` (`tmux`, `tz`: the next session; `hostname`: a restart) |

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
