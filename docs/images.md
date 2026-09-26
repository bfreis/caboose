# Use your own image

By default the sandbox is built on the `Dockerfile` embedded in the launcher:
Ubuntu with git, gh, jj, Node, Bun, Go and the docker CLI. It changes with
caboose. There are two ways to build on something else.

**An environment's own Dockerfile.** `caboose setup image` writes
`~/.caboose/envs/<env>/image/Dockerfile` from that same Dockerfile, with
the parts you choose: Node, Bun, Go, gh, jj and the docker CLI (all in by
default), and Rust (out by default). The base — Ubuntu, and what the
sandbox needs — is always in. From then on the file is yours: a newer
caboose never changes it, `image/` is the build context (files next to
the Dockerfile can be `COPY`ed), and `caboose setup image` run again shows
how it differs from a fresh preset before replacing it. The image is
labelled with a hash of everything in `image/`, so an edit reads as out
of date:

```sh
$EDITOR ~/.caboose/envs/default/image/Dockerfile
caboose build              # builds the base from image/, checks it, adds the layer
caboose restart            # moves the container onto it; kills sessions, asks first
```

An edit is a change you made, as a switched base is: the next launch that
creates the container rebuilds for it by itself. A launch that finds the
container running only says so. `image/` is on the host and never mounted
into the container, so nothing in the sandbox can change what it is built
from.

**An image of your own.** To run on something else entirely — your own
toolchains, another distro, Alpine — point `CABOOSE_BASE_IMAGE` at it (an
environment with an `image/` dir as well is refused: keep one):

```sh
export CABOOSE_BASE_IMAGE=my/image:tag
caboose restart            # builds on it, then recreates the container; kills running sessions, asks first
```

It has to be another image than `CABOOSE_IMAGE`, the one caboose builds on
top of it — in any spelling (`img`, `img:latest`, `docker.io/library/img`):
a build and a launch refuse the two being the same. An image built `FROM`
a caboose one, the default base (`caboose-base`) included, is fine.

**The image is yours.** caboose never installs anything into it — no apt,
apk or dnf, ever — so what is in the sandbox is exactly what you put there.
It documents what an image must contain, checks that it does, and adds only
what cannot be part of a generic image, as a thin layer on top
(`layer.Dockerfile`, `layer-user.sh`): the `agent` user with your UID and
GID and its home, the entrypoint, and `tmux.conf`. The default base goes
through the same path; it is just one image that passes the check.

## Requirements for your own image

Any image, glibc or musl based, that has all of this — exactly what
`caboose check-image` checks:

| requirement | why |
|---|---|
| `/bin/sh` | the check and the user setup run with plain `sh` |
| `bash` | the entrypoint and Claude Code's installer are bash scripts; the launcher runs `docker exec … bash` |
| `curl` and CA certificates | the installer downloads Claude Code over https |
| `tmux` | every session runs in tmux |
| `git`, 2.28 or later | [`caboose sync`](sync.md) runs every git command in the container |
| glibc or musl, on x86_64 or aarch64 | Claude Code ships builds for exactly those |
| on musl: `libgcc`, `libstdc++`, `ripgrep` | what Claude Code's musl build needs at run time |
| `/usr/bin/env` (at that path), `readlink -f`, `mktemp -d`, `find -mindepth -delete`, `rm`, `rmdir`, `sleep`, `test` (a file, not only the builtin) | the entrypoint, and the launcher's readiness check |
| `uname`, `mkdir`, `chmod`, `cut`, `sed`, `tr`, `grep`, `head`, `sha256sum` | the installer |
| `chown` | the user setup, giving `/home/agent` to the `agent` user |
| `/etc/passwd` and `/etc/group`, regular files; no `/home/agent` that is a symlink or not a directory | the user setup edits the first two in place, and makes `/home/agent` and the mountpoints in it |
| optional: `tic` (ncurses) | compiling your terminal's terminfo; without it `TERM` falls back |

coreutils, findutils, grep and sed, or BusyBox, cover the last three tool
rows. CA certificates count when a bundle sits at one of the usual paths
(`/etc/ssl/certs/ca-certificates.crt` and the Red Hat and SUSE
equivalents), or when an https request from the image verifies. The libc is
detected exactly as Claude Code's installer detects it, so the two can't
disagree about which build (`linux-arm64-musl`, say) the image gets.

That is the minimum for caboose, not for work: add an ssh client (the
host's agent is forwarded; `caboose sync` pushes over it too), and whatever
your projects build with. Two
images that pass, Debian and Alpine:

```dockerfile
FROM debian:13.7-slim
# coreutils, findutils, grep, sed and tic (ncurses-bin) are already there.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash curl ca-certificates tmux git \
 && rm -rf /var/lib/apt/lists/*
```

```dockerfile
FROM alpine:3.24
# BusyBox covers the small tools. Add ncurses for the optional tic.
RUN apk add --no-cache bash curl ca-certificates-bundle tmux git libgcc libstdc++ ripgrep
```

These are the test bases `make test-byo` runs sessions on
(`tests/byo/*.Dockerfile`), where the comments say what each package is for.

The layer edits `/etc/passwd`, `/etc/group` and, where they exist,
`/etc/shadow` and `/etc/gshadow` directly, with plain `sh`: no `useradd` or
`adduser`. A user already holding your UID (ubuntu's `ubuntu`, node's
`node`) is taken over and renamed `agent`; a different user named `agent`
is dropped. That goes for a system account too (Debian's `_apt` is UID
100): the check says so, with its home and shell, and allows it, since the
`agent` user must have your UID. A group already holding your GID is reused
as it is — on a Mac that is GID 20, `dialout` on Debian — and an `agent`
group at another GID that nothing uses is then dropped; otherwise an
`agent` group is added. New entries go before a NIS `+`/`-` line, not after
it. Your UID or GID being 0 is refused: caboose must not run as root, and
the layer would take over the image's root user. It sets `HOME`, puts
`~/.local/bin` first on `PATH`, sets `LANG=C.UTF-8` unless the base sets
its own, and replaces the base's entrypoint and `CMD`. On musl the container is created with
`USE_BUILTIN_RIPGREP=0`, so Claude Code uses the image's `rg`.

## Checking an image

```sh
caboose check-image my/image:tag     # default: the base in use
```

It runs a throwaway container of the image (`docker run --rm`, as root, with
`/bin/sh` for its entrypoint), pulling it first if it names an image that
isn't local, and prints a checklist: each requirement `ok` or `missing`, the
libc and platform, which user and group in the image already hold your UID
and GID, and whether `https://claude.ai` answers from a container of the
image — a warning only, since a proxy may be set later, but Claude Code
installs from there on first run. Each row starts with a mark, as
[`caboose doctor`](commands.md#when-something-is-wrong)'s do (`✓` met, `✗` unmet, `!`
worth knowing), and a verdict ends the list; on a terminal a spinner says
the check is running. The unmet requirements are listed on stderr, each
with why it is needed. It exits

- **0** when every requirement is met,
- **1** when one isn't,
- **2** when it couldn't check at all: docker not answering, the pull
  failing, or, with no `IMAGE` and no `CABOOSE_BASE_IMAGE`, a default base
  that hasn't been built yet.

## What a build does

`caboose build` (or the launch that creates the container, when there
is no image or it was built on another base):

1. **Gets the base.** On the default, it builds the embedded `Dockerfile`
   and tags it `$CABOOSE_IMAGE-base`. On `CABOOSE_BASE_IMAGE`, it never
   builds anything: it pulls the image if it isn't local (or `--pull` asks),
   and otherwise uses it as it is.
2. **Checks it**, as `caboose check-image` does. A base that fails stops the
   build here, before the layer, with the list of what is missing.
3. **Builds the layer** on it, as `$CABOOSE_IMAGE`, for your UID and GID,
   labelled with the hashes of what it was built from, which kind of base
   and its name and ID, your UID and GID, and the platform the check found
   — which is how the launcher knows which `~/.local` to mount without
   starting a container first. Every label is set, empty where it does not
   apply, so labels a base inherited from a caboose image never read as the
   layer's.

A `^C` at any step stops the build there (exit 130).

## When your image changes

On your own base, the image caboose runs is current while it was built by
the same layer on the base's current image ID. A pull or rebuild of
`my/image:tag` since makes it stale: a launch warns, and `caboose
version` says `local : differs`, with the reason. So does an image
built for another host user (its UID or GID labels differ from yours). Base
names are compared as docker resolves them, so `node:22` and
`docker.io/library/node:22` are one base. The embedded `Dockerfile` plays
no part here, so a new launcher only makes your image stale when it changes
the layer. A running container is never moved off its image: the next
launch that creates the container (`caboose restart`, or any launch once
the container is gone) rebuilds a stale image first, and says so;
`caboose restart` builds before it removes the old container, so a build
that fails leaves it running. `caboose build` rebuilds it now, without
touching the container.

When `CABOOSE_BASE_IMAGE` names another base than the image was built on —
or is set or unset since — that is a change you asked for, and the launch
that creates the container rebuilds on the new base in the same way. With
`CABOOSE_NO_AUTO_BUILD` set, a launch builds nothing: it refuses to create
a container on another base than the configured one, naming both, and
otherwise uses the stale image with a warning, until you run `caboose
build`.

## Switching between images

Claude Code's builds only run on the libc and arch they were built for, so
the data dir keeps one install per platform, in `local/<platform>`
(`linux-x64`, `linux-arm64`, `linux-x64-musl`, `linux-arm64-musl`). A
container mounts the one its image's platform label names. The first
container on a new platform installs Claude Code into its dir; switching
back to an image you used before finds its dir as it was left, so it costs
nothing but the rebuild the `caboose restart` runs. Everything else in the
data dir — login, settings, memories, transcripts — is shared across
images.

Each platform in use keeps its own `CABOOSE_KEEP_VERSIONS` versions, at
~224MB each. `caboose status` shows the disk used per platform dir, and
`caboose prune` only prunes the mounted one and names the others. A platform
dir no image uses any more is never deleted for you; remove
the data dir's `local/<platform>` by hand once you're sure.
