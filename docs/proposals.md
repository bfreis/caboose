# Proposals: changes a session asks for

A session in the sandbox cannot change what the sandbox is: the image, the
host directories it can see. Those live on the host (`config.toml`, the
environment's `image/`), and they decide how much of your machine the
sandbox reaches — which is exactly what an injected instruction, arriving
in a README or a web page, would want to widen. (What the sandbox keeps of
its own home is another matter: that is its [sandbox config](sandbox-config.md),
which a session edits itself.)

So a session **proposes** the change, and you apply it on the host:

1. In a session: *"install foo"*. The session writes a proposal into
   `~/.caboose-proposals/` and tells you to run `caboose apply`. It can
   stay open.
2. In a host terminal: `caboose apply` shows each proposal whole — the
   Dockerfile change as a diff, the root to mount — and applies it, leaves it pending or deletes it, as you say. Then it
   builds the image if the Dockerfile changed.
3. It offers `caboose restart`, which moves the container onto the changes
   and ends running sessions (it says how many; the default is no). Back
   in, `caboose` starts a session, and `/resume` picks up the conversation.

A launch says when proposals are waiting: `2 pending proposals (foo, other):
'caboose apply' reviews them`. While the container runs, the [host
link](host-link.md) also shows a notification when a session writes a new
one, naming its title and the `caboose apply` to run (at most one every 30
seconds; what arrives in between is named in the next). It only tells you:
nothing is applied until you run `caboose apply` and say yes.

## What can be proposed

| | What it does | Before you say yes |
|---|---|---|
| a Dockerfile section | installs something in the image | shown as a diff of the Dockerfile |
| one root | mounts a host directory at `/work/NAME` | refusals and warnings below, and you type the root's name |

Nothing else. A proposal that names anything more — the docker socket, a
base image, the sync remote, any other setting — is refused whole, not
offered for a yes. So is one with a control character (an escape sequence
could redraw the terminal and hide part of the proposal), an invisible
formatting character such as a bidi override, or a section holding `FROM`,
`ONBUILD`, `RUN --network` or `RUN --security`.

**A section** goes into the environment's `image/Dockerfile`, between
`# caboose:section` markers like caboose's own, replacing a section of the
same name. An environment with no `image/Dockerfile` builds from the one
built into caboose; the first section applied writes caboose's preset, with
the section, as `image/Dockerfile` — `apply` says so first, since from then
on the environment builds from a Dockerfile of its own, and a newer
caboose's changes no longer reach it (`caboose setup image` shows how the
two differ). On `base_image` there is no Dockerfile, and no section can be
applied.

A proposal names the hash of the Dockerfile it was written against, and is
refused when `image/Dockerfile` has changed since — by hand, or by another
proposal applied first. The session can read the current one (below) and
propose again.

**A root** is checked on the host, at its physical path, symlinks resolved;
that path is also what is written into `config.toml`, so a symlink the
sandbox could change later does not decide what is mounted. Refused
outright:

- `/`, your home directory, or any directory holding it;
- anything inside a hidden directory of the home (`~/.ssh`, `~/.config`,
  `~/.local`, ...) or `~/Library`;
- anything holding caboose's own state, or inside it: `~/.caboose`, the
  environment, the data dir (the sandbox could change its own
  configuration, and read the Claude login);
- anything overlapping a root, or a name a root already has;
- a path that does not exist, or is not a directory.

Otherwise `apply` warns when the directory holds what looks like
credentials (`.env` files, `.ssh`, `*.pem`, ...), and asks you to type the
root's name to confirm. Adding a second root to a single one at `/work`
also names that one, moving its projects to `/work/NAME/...`; that is said
and asked first, as in [`caboose setup roots`](configuration.md#setup).

## The files

`~/.caboose-proposals` is `<data>/proposals` on the host. A proposal is
`NAME.toml` there:

```toml
title = "Install foo"
reason = "foo builds the docs; it keeps its config in ~/.config/foo."
dockerfile_sha256 = "…"          # with [section]: from current/state.toml

[section]
name = "foo"
title = "foo 2.3"
body = '''
ARG FOO_VERSION=2.3.0
RUN curl -fsSL https://example.com/foo-${FOO_VERSION}.tgz | tar -xz -C /usr/local/bin foo
'''

[roots]
other = "~/src/other"
```

`current/` next to them is written by the host at every launch and after
every `apply`: `Dockerfile`, the one the next build uses (caboose's preset
when the environment has none of its own), and `state.toml`, with where it
comes from, its hash, and the roots `config.toml` has. It is only information: changing it changes nothing, and `apply`
checks every proposal against the real files. The sandbox's
`~/.claude/CLAUDE.md` tells sessions all of this.

A container created before proposals existed does not mount the directory;
`caboose restart` creates it again with it.
