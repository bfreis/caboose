# Proposals: changes a session asks for

A session in the sandbox cannot change what the sandbox is: the image, the
host directories it can see. Those live on the host (`config.toml`, a
Dockerfile profile's directory), and they decide how much of your machine the
sandbox reaches — which is exactly what an injected instruction, arriving
in a README or a web page, would want to widen. (What the sandbox keeps of
its own home is another matter: that is its [sandbox config](sandbox-config.md),
which a session edits itself.)

So a session **proposes** the change, and you apply it on the host:

1. In a session: *"install graphviz"*. The session writes a proposal into
   `~/.caboose-proposals/` and tells you to run `caboose apply`. It can
   stay open.
2. In a host terminal: `caboose apply` shows each proposal whole — the
   packages to add or remove, the Dockerfile change as a diff, the root to
   mount — and applies it, leaves it pending or deletes it, as you say.
   Packages are built as they are applied; after a Dockerfile change it
   offers to build.
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

What a proposal can change in the image depends on the kind of the
[image profile](configuration.md#image-profiles) in use:

| | Under | What it does | Before you say yes |
|---|---|---|---|
| packages to add or remove | `apko` | changes the profile's `packages` | shown as names (`+ graphviz`, `- jq`); built before anything is written |
| a Dockerfile section | `dockerfile` | installs something in the image | shown as a diff of the Dockerfile |
| one root | any | mounts a host directory at `/work/NAME` | refusals and warnings below, and you type the root's name |

Under a `ref` profile, an image of your own, nothing in the image can be
proposed: a session that needs a tool says what to add to your image.

Nothing else. A proposal that names anything more — the docker socket, a
base image, the sync remote, any other setting — is refused whole, not
offered for a yes. So is one with a control character (an escape sequence
could redraw the terminal and hide part of the proposal), an invisible
formatting character such as a bidi override, or a section holding `FROM`,
`ONBUILD`, `RUN --network` or `RUN --security`.

**Packages** apply under an apko image only: a `[packages]` table with
`add` and `remove`, Wolfi package names, 64 at most. A name added that the
image already has, or removed that is not one of the profile's own (to
drop one of caboose's groups, run `caboose setup image`), is refused. On
yes, `apply` builds the image with the new list first, writing nothing,
so a list that does not build changes nothing and stays pending. Then it
reads `config.toml` again: if the profile's `packages` or `defaults`
changed there while the image was built, it writes nothing and leaves the
proposal pending, to be applied again to the profile as it is now.
Otherwise it writes the profile's `packages` into `config.toml`, and only
once that is written, the lock the build resolved. Should `config.toml`
not be written, the old lock stays, and the image is newer than
`config.toml` says: the next launch rebuilds it to match `config.toml`.
While the container runs, the host link checks each proposal as it
arrives, a `[packages]` one against the repository indexes alone (no
package is downloaded), and writes what it found as `NAME.check` beside
it (when, below). Its first line is `ok`, with what the list resolves to
(`resolves to 162 packages, 2.3 GB installed (+14 packages, +210 MB)`);
`error`, with why the proposal is wrong as it stands (a misspelt name
comes with close ones); or `unchecked`, when the host could not check it
then (too many checks in the last hour, the network, a timeout, its own
configuration), which says nothing against the proposal. The session
reads it and fixes the proposal on `error` before asking you. The check
is the link's, and only says: `apply` checks the proposal again, and
builds.

**A section** applies under a `dockerfile` profile only. It goes into the
profile's Dockerfile, between `# caboose:section` markers like those of
caboose's seed, replacing a section of the same name. A profile whose
directory has no Dockerfile yet gets caboose's seed, with the section, as
its Dockerfile: `apply` says so first, since the file is yours from then
on, as one [`caboose setup image`](images.md#a-dockerfile) writes is.

A proposal names the hash of the Dockerfile it was written against, and is
refused when the Dockerfile has changed since — by hand, or by another
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
title = "Install graphviz"
reason = "graphviz renders the docs' diagrams."

[packages]                       # under apko
add = ["graphviz"]
remove = []

[roots]
other = "~/src/other"
```

Under a `dockerfile` profile the image's part is a section instead, with
the hash of the Dockerfile it goes into:

```toml
dockerfile_sha256 = "…"          # from current/state.toml

[section]
name = "foo"
title = "foo 2.3"
body = '''
ARG FOO_VERSION=2.3.0
RUN curl -fsSL https://example.com/foo-${FOO_VERSION}.tgz | tar -xz -C /usr/local/bin foo
'''
```

`NAME.check` is the host link's check of `NAME.toml`, written while the
container runs: for a proposal with a `[packages]` table, and for any file
that cannot be read as a proposal at all (with why `apply` would refuse
it); a proposal with neither gets none, and a stale one is removed. Its
first line is `ok`, `error` or `unchecked`, then what it found. The link
checks a bounded number of proposals in an hour; past that it writes no
checks until the hour is over.

`current/` next to them is written by the host at every launch and after
every `apply`: `state.toml`, with the image profile in use (`image`,
`"<kind>.<name>"`) and what can be proposed for it, and the roots
`config.toml` has (each a `[roots.NAME]` with its `host` and container
`path`). Under `apko` it lists the profile's own `packages`, `defaults`, and
every package `installed`; under `dockerfile`, where the Dockerfile comes
from (`dockerfile`, the profile's own, or `seed`, caboose's, when the
profile has none yet) and its hash, with the file itself as `Dockerfile`
beside it. It is only information: changing it changes nothing, and `apply`
checks every proposal against the real files. caboose's
instructions to the sandbox, `/etc/claude-code/CLAUDE.md` inside, and its
`caboose-propose` skill tell sessions all of this.

