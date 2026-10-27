# Proposals: changes a session asks for

A session in the sandbox cannot change what the sandbox is: the image, the
host directories it can see and write, the host commands it may run. Those live on the host (`config.toml`, a
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
   mount, or the change to `config.toml` an alias or a writable part
   makes — and applies it, leaves it pending or deletes it, as you say.
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
| one alias, alone | any | adds an [`[alias.NAME]`](configuration.md#aliasname), a host command sessions may run | refusals and warnings below, the change to `config.toml` as a diff, and you type the alias's name |
| one writable part, alone | any | makes a directory of a root [writable](configuration.md#read-only-roots-and-writable-parts), from the next restart | refusals and warnings below, the change to `config.toml` as a diff, and you type the root's name |
| settings, alone | any (the VM keys: vm only) | sets `tz`, `hostname`, `tmux` in `[session]`, `auto_build` in `[build]`, and `cpus`, `memory` of the `[vm.NAME]` in use | the change to `config.toml` as a diff, and when each key takes effect |

Under a `ref` profile, an image of your own, nothing in the image can be
proposed: a session that needs a tool says what to add to your image.

Nothing else. A proposal that names anything more — the docker socket, a
base image, the sync remote, any other setting than those six — is refused whole, not
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

The [web page](web-ui.md) applies a packages proposal too, alone or with
a root, with **Build and apply**: the daemon builds the image as a job of
the environment's (one at a time: a stop or restart meanwhile is refused
as busy) and the page shows its last lines as it goes. As `apply` does, it
writes nothing unless the image builds; once it has, it plans the proposal
again and writes it only if it still shows what you confirmed and the
profile's packages in `config.toml` did not change while the image was
built, then the lock. The running
sandbox keeps its image until its next `caboose restart`.

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
sandbox could change later does not decide what is mounted. The home and
caboose's own state are judged by the directories' identity on disk, and
names in the home ignoring case, so another name for the same directory (a
bind mount, a firmlink, another case on a case-insensitive disk) is refused
too. Refused outright:

- `/`, your home directory, or any directory holding it;
- anything inside a hidden directory of the home (`~/.ssh`, `~/.config`,
  `~/.local`, ...) or `~/Library`;
- anything holding caboose's own state, or inside it: `~/.caboose`, the
  environment, the data dir (the sandbox could change its own
  configuration, and read the Claude login);
- anything overlapping a root, by path or by identity, or a name a root
  already has;
- anything inside what another environment's sandbox can write, as its
  `config.toml` says or its sandbox mounts now, which that sandbox could
  swap for a symbolic link before the next launch (the very directory it
  mounts writable may be shared);
- a path that does not exist, or is not a directory.

A root a proposal adds is mounted read-only: what of it the sandbox may
write is `writable` in `config.toml`, yours to edit (`caboose setup roots`
asks), or a writable part a session proposes on its own (below). Applying
a root keeps the other roots' writable parts as they are.

Otherwise `apply` warns when the directory holds what looks like
credentials (`.env` files, `.ssh`, `*.pem`, ...), and asks you to type the
root's name to confirm. Adding a second root to a single one at `/work`
also names that one, moving its projects to `/work/NAME/...`; that is said
and asked first, as in [`caboose setup roots`](configuration.md#setup).

**An alias** is a host command that sessions may then run with
`caboose-agent run`, so it has a refusal list of its own, judged on this
machine as `apply` shows it. The proposal is an `[alias.NAME]` table with
exactly `config.toml`'s keys (`command`, `description`, `timeout`, `cwd`,
`policy`, and `[alias.NAME.params]`), alone in its proposal; `policy` is
`"allow"`, `"ask"` or absent (`"allow"`). Refused before it is shown:

- a `command[0]` that is not an absolute, clean path; an element that is
  empty, over 1024 bytes, or holds a control, tab or invisible character;
  more than 64 elements;
- a command that does not exist here, or is not an executable file;
- a command that is, or resolves to, or reaches through a symlink, a shell
  or a program that runs what its arguments say: `sh`, `bash`, `zsh`,
  `fish`, `dash`, `ksh`, `csh`, `tcsh`, `busybox`, `env`, `sudo`, `doas`,
  `su`, `osascript`, `open`, `xargs`, `nohup`, `timeout`, `nice`, `script`,
  `expect`, `python`, `perl`, `ruby`, `node`, `deno`, `bun`, `php`, `lua`,
  `tclsh`, `awk`, `find`, `make`, `ssh`, `screen`, `tmux`, `caboose` and
  the like, in any case and with any version (`python3.12`); a script is
  judged by its `#!` interpreter, so a shell script is refused too;
- a command within a root of any environment (read-only or not), within anything a sandbox of
  any environment can write, within a hidden directory of your home, or
  within caboose's own state (`~/.caboose`, the environment, the data
  dir), as written, resolved, or at any symlink on the way; and, when what
  the sandboxes can write cannot be told (the engine is down), any alias;
- a `cwd` that does not exist, or is within caboose's own state or a hidden
  directory of your home; under `trust = "hostile"`, one within anything a
  sandbox can write;
- a parameter or a value outside `config.toml`'s syntax.

`apply` warns when it runs without asking you (no `policy = "ask"`), when
its `cwd` is somewhere the sandbox can write, when it names a path on this
machine, and when it replaces an alias of that name; it shows the exact
change to `config.toml` and writes it only once you type the alias's name,
and only if `config.toml` has not changed since. An alias needs no build
and no restart: the link reads it within seconds. The aliases you write in
`config.toml` yourself are yours, and held to none of this; every alias,
yours or proposed, is decided again at each run
([configuration](configuration.md#aliasname)).

**A writable part** is a directory of a root that the sandbox may then
change or delete anything in. The proposal is a `[writable]` table with
one key, `path`: the directory as the sandbox sees it, an absolute, clean
path under `/work`, alone in its proposal. `apply` finds it on this
machine through the roots the sandbox mounts (those `config.toml` has,
when there is no sandbox), never through anything the session says of
the host. Refused before it is shown:

- a path not inside a root the sandbox mounts, or inside none
  `config.toml` has;
- one that does not exist, is not a directory, or is reached through a
  symbolic link at any component below the root;
- your home, or a directory holding it; anything in a hidden directory of
  your home or in `~/Library`;
- anything in, at or holding caboose's own state (`~/.caboose`, the
  environment, the data dir), by path and by identity: a second name for
  one of them is the same directory;
- a path writable already: the whole root is, or a writable part holding
  it.

`apply` warns when the directory holds what looks like credentials, and
when the new part holds parts the root lists now (they are replaced by
it). It shows the root's line in `config.toml` with its new `writable`
list, and writes it only once you type the root's name, and only if
`config.toml` has not changed since. Nothing moves in the running sandbox:
the mounts are fixed when it is created, so the part is writable from the
next `caboose restart`, which `apply` offers. Every launch checks the
writable parts again, and refuses one that has become a symbolic link.

**Settings** are six keys of `config.toml`, alone in their proposal, as a
`[settings]` table with the keys' own tables inside it (dotted keys decode
the same):

```toml
[settings.session]
tz = "Europe/Lisbon"   # also hostname, tmux
[settings.build]
auto_build = false
[settings.vm]          # the [vm.NAME] profile config.toml uses
cpus = 6
memory = "12G"
```

The session never names the VM profile: `cpus` and `memory` are written
into the one `isolation` selects. They are refused when the isolation is not
`vm`, when `config.toml` defines no profile, and when they ask for more CPUs
or memory than this machine has. A value `config.toml` already has is left
out with a warning, and a proposal with nothing left changes nothing. Any
other key under `[settings]` refuses the proposal whole, as does any other
part next to it. They widen nothing the sandbox reaches, so `apply` shows
the change to `config.toml` as a diff and asks for a yes, no typed name, and
writes it only if `config.toml` has not changed since. Each key says when
it takes effect: `tz` for sessions started from the next launch (what
already runs keeps its zone until the next restart), `tmux` for sessions
started from the next launch, `auto_build` from the next launch that
creates the sandbox, and `hostname`, `cpus` and `memory` at the next
restart. `apply` offers the restart only when a key needs one.

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

An alias, alone in its proposal:

```toml
title = "Keep the Mac awake during long builds"
reason = "builds run overnight"

[alias.awake]
command = ["/usr/bin/caffeinate", "-i"]
description = "keep the Mac awake until interrupted"
policy = "ask"
```

A writable part, alone in its proposal:

```toml
title = "Let the build write its output"
reason = "make writes into build/"

[writable]
path = "/work/dev/proj/build"
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
`config.toml` has (each a `[roots.NAME]` with its `host`, container
`path`, and `writable`: the parts the sandbox may write), and `[settings]`,
the values a settings proposal changes as `config.toml` has them, with the
isolation in use and the VM profile `cpus` and `memory` go in. Under `apko` it lists the profile's own `packages`, `defaults`, and
every package `installed`; under `dockerfile`, where the Dockerfile comes
from (`dockerfile`, the profile's own, or `seed`, caboose's, when the
profile has none yet) and its hash, with the file itself as `Dockerfile`
beside it. It is only information: changing it changes nothing, and `apply`
checks every proposal against the real files. caboose's
instructions to the sandbox, `/etc/claude-code/CLAUDE.md` inside, and its
`caboose-propose` skill tell sessions all of this.

