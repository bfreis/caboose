# The image

The sandbox runs an image caboose builds on your machine, in two parts: a
**base**, which holds the OS and the tools, and a thin **layer** on top,
the same on every base. What the base is comes from the environment's
[image profile](configuration.md#image-profiles), one of three kinds:

- **`apko`**, the default: Wolfi packages, built into an image with
  [apko](https://github.com/chainguard-dev/apko). No Dockerfile: caboose's
  package groups and the packages you name. See [Packages](#packages-the-default).
- **`dockerfile`**, for experts: a Dockerfile of yours, in the
  environment's directory. See [A Dockerfile](#a-dockerfile).
- **`ref`**: an image of your own, used as it is. See
  [An image of your own](#an-image-of-your-own).

`caboose setup image` picks one, and writes it into `config.toml`.

**The layer** is all caboose adds to a base, whatever its kind
(`layer.Dockerfile`, `layer-user.sh`): the `agent` user with your UID and
GID and its home, the entrypoint, and `tmux.conf`. It installs nothing —
no apt, apk or dnf, ever — so a base caboose did not build, a Dockerfile's
or your own image, holds exactly what you put there. Every base, whatever
its kind, goes through the same [check](#checking-an-image) first.

## Packages (the default)

With no image profile in `config.toml`, the sandbox is `apko.default`:
caboose's packages. They come in groups, in
`internal/apkobuild/pkgset/packages.toml`:

| group | packages | |
|---|---|---|
| required | `wolfi-baselayout`, `ca-certificates-bundle`, `busybox`, `bash`, `curl`, `git`, `tmux`, `ncurses`, `tzdata` | always in: what the sandbox [requires](#requirements-for-your-own-image) |
| core | coreutils, findutils, grep, sed, diffutils, an ssh client and ssh-keygen (for signing commits), ripgrep, jq, less, vim, procps, unzip, zstd, a C toolchain (`build-base`), Python | default |
| node | Node.js, npm and corepack | default |
| bun | Bun | default |
| go | Go | default |
| gh | the GitHub CLI | default |
| docker | the Docker CLI, buildx and compose | default |
| dockerd | the Docker engine and `iptables` | default |
| sudo | sudo | default |
| rust | rustup | off |

Add your own, Wolfi package names, in the profile:

```toml
[apko.default]
packages = ["graphviz", "postgresql-17-client"]
```

`defaults = false` leaves out every group but the required one, so the
image is only what the sandbox requires and `packages`. `caboose setup
image`'s "choose package groups" writes exactly that: `defaults = false`
and the packages of the groups you tick, with any of your own kept. To
find a package's name, Wolfi's index
(`https://packages.wolfi.dev/os/<aarch64|x86_64>/APKINDEX.tar.gz`) lists
each as a `P:` line, and a `cmd:NAME` on its `p:` line says it provides
the command `NAME`. A session can [propose](proposals.md) packages too,
and the host checks the names as the proposal arrives.

An apko base is built on this machine, in caboose itself: no Docker build,
and no package's install scripts run, since apko unpacks packages and
runs none of them. It is built for the docker engine's architecture
(x86_64 or aarch64; under `vm`, the VM's), at a fixed timestamp, so the same lock gives the same
image, byte for byte; then `docker load`ed as `caboose-base:<env>`. Under
a [`vm` profile](configuration.md#the-vm-isolation) it is built the same
way, here, and streamed into the builder VM.

### The lock

The packages are resolved — each name and its dependencies, to exact
versions — into a lock, `apko-<name>.lock.json` in the environment's
directory, next to `config.toml`. A build builds the lock as it is, so a
rebuild gives the same base until something calls for resolving again:

- there is no lock yet, or it cannot be read;
- the profile's `packages` or `defaults` changed;
- caboose's groups changed with a new caboose;
- the lock is for another architecture than the engine's;
- `caboose build --pull`, which takes the newest packages the repository
  has. That is how a base gets updates: nothing does it by itself.

The lock is written only once the image it built is in: a resolve or a
build that fails leaves the last good lock, and the image it describes.
It also records what the profile asked for, so a change to the profile
that asks for the same packages (adding one the groups already have)
rewrites that record and nothing else.

### The package cache

Downloaded packages and indexes are kept in `~/.caboose/cache/apk`, shared
by every environment, so a rebuild downloads only what changed. `caboose
prune` removes what no environment's lock names, and every index but the
newest; see [Commands](commands.md).

## A Dockerfile

A `dockerfile` profile builds the base from a Dockerfile of yours:

```toml
image = "dockerfile.default"

[dockerfile.default]
```

It has no keys: its build context is always the environment's
`dockerfile/<name>/` (`~/.caboose/envs/default/dockerfile/default/`), so
files next to the Dockerfile can be `COPY`ed. That directory is on the
host and never mounted into the container, so nothing in the sandbox can
change what it is built from; its fixed place is why no key could point
elsewhere.

`caboose setup image`, choosing the Dockerfile you edit, writes the seed
there when it has no Dockerfile: the one caboose carries, Ubuntu with
Node, Bun, Go, gh, jj, the docker CLI and engine and sudo, each a section
between a `# caboose:section NAME TITLE` line and `# caboose:end` (Rust
is a section left commented out). From then on the file is yours: a newer
caboose never changes it, and setup run again leaves it as it is. The
sections are what a session's [proposal](proposals.md) adds to or
replaces; keep the markers you want proposals to find.

```sh
$EDITOR ~/.caboose/envs/default/dockerfile/default/Dockerfile
caboose build              # builds the base from the dir, checks it, adds the layer
caboose restart            # moves the container onto it; kills sessions, asks first
```

The image is labelled with a hash of everything in the directory, so an
edit is a change you made, and the next launch that creates the container
rebuilds for it by itself. A launch that finds the container running only
says so.

## An image of your own

To run on something else entirely — your own toolchains, another distro —
name it in a `ref` profile:

```toml
[ref.default]
image = "registry.example/team/image:tag"
```

```sh
caboose restart            # builds on it, then recreates the container; kills running sessions, asks first
```

It is pulled when it is not local, and with `caboose build --pull`;
otherwise it is used as it is. It has to be another image than
`caboose:<env>`, the one caboose builds on top of it — in any spelling
(`img`, `img:latest`, `docker.io/library/img`): a build and a launch refuse
the two being the same. An image built `FROM` a caboose one,
`caboose-base:<env>` included, is fine. Nothing can be proposed for it:
a session that needs a tool tells you what to add to your image.

## Requirements for your own image

Any image, glibc or musl based, that has all of this — exactly what
`caboose check-image` checks, on every base:

| requirement | why |
|---|---|
| `/bin/sh` | the check and the user setup run with plain `sh` |
| `bash` | the entrypoint and Claude Code's installer are bash scripts; the launcher runs its commands in the sandbox with bash |
| `curl` and CA certificates | the installer downloads Claude Code over https |
| `tmux` | every session runs in tmux |
| `git`, 2.28 or later | [`caboose sync`](sync.md) runs every git command in the sandbox |
| glibc or musl, on x86_64 or aarch64 | Claude Code ships builds for exactly those |
| on musl: `libgcc`, `libstdc++`, `ripgrep` | what Claude Code's musl build needs at run time |
| `/usr/bin/env` (at that path), `readlink -f`, `mktemp -d`, `find -mindepth -delete`, `rm`, `rmdir`, `sleep`, `test` (a file, not only the builtin) | the entrypoint, and the launcher's readiness check |
| `uname`, `mkdir`, `chmod`, `cut`, `sed`, `tr`, `grep`, `head`, `sha256sum` | the installer; the launcher also opens Docker Desktop's SSH agent socket with `chmod` |
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

**Docker inside the sandbox** is a fact the check reports, never a
requirement. Under a [`vm` profile](configuration.md#the-vm-isolation)
the sandbox starts the image's own `dockerd` when it has one, so `docker`
works inside it; that takes `dockerd`, `containerd`,
`containerd-shim-runc-v2`, `runc`, `iptables` (dockerd will not start
without it) and the `docker` CLI on `PATH`. Docker's static release
(`https://download.docker.com/linux/static/stable/`) has all of them but
`iptables`, which comes from the distribution; caboose's `dockerd` group,
and the seed's `dockerd` section, have both. An image without them is as
usable: its sandbox just has no docker. Under container and gvisor nothing
starts an image's dockerd, so the check says nothing about it there.

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
  failing, or, with no `IMAGE` under an `apko` or `dockerfile` profile, a
  base that hasn't been built yet.

Under a [`vm` profile](configuration.md#the-vm-isolation) there may be no
docker engine, so the check runs where `caboose build` builds: in the
builder VM, whose own docker keeps the bases builds made or pulled. The
base in use is the one the last `caboose build` made there; a named image
the builder lacks is pulled into it from its registry. An image that only a
docker engine on this Mac has cannot reach the builder, so its pull fails
(exit 2): push it to a registry first, or check it in that engine with
`caboose --env NAME check-image IMAGE`, in an environment whose `config.toml` has no `vm` profile. There the checklist
ends with a `docker inside` row: `available (dockerd VERSION)`, or what the
image lacks for it, marked `!` and never counted as unmet, with what to add
on stderr; `caboose build` notes it too.

## What a build does

`caboose build` (or the launch that creates the container, when there
is no image or it is out of date):

1. **Gets the base**, as the profile's kind says. `apko`: resolves the
   packages into the lock when [it must](#the-lock), builds the lock and
   loads it as `caboose-base:<env>`. `dockerfile`: builds the profile's
   directory, as `caboose-base:<env>`; a directory with no Dockerfile
   stops here, saying how to get one. `ref`: never builds anything; pulls
   the image if it isn't local (or `--pull` asks), and otherwise uses it
   as it is.
2. **Checks it**, as `caboose check-image` does. A base that fails stops the
   build here, before the layer, with the list of what is missing.
3. **Builds the layer** on it, as `caboose:<env>`, for your UID and GID,
   labelled with what it was built from: the hash of the layer, the kind
   of base and what identifies it (the lock's hash on `apko`, a hash of
   the directory on `dockerfile`), its name and ID, your UID and GID, and
   the platform the check found — which is how the launcher knows which
   `~/.local` to mount without starting a container first. Every label is
   set, empty where it does not apply, so labels a base inherited from a
   caboose image never read as the layer's.

A `^C` at any step stops the build there (exit 130).

## When the image is out of date

`caboose version` says whether the image is current, and if not, why; a
launch says so too. A running container is never moved off its image: the
next launch that creates the container (`caboose restart`, or any launch
once the container is gone) rebuilds an out-of-date image first, and says
so; `caboose restart` builds before it removes the old container, so a
build that fails leaves it running. `caboose build` rebuilds it now,
without touching the container.

What makes it out of date is one of two things. **A change you made**:
another image profile, or the one in use changed —

- `apko`: its `packages` or `defaults`, or its lock deleted or unreadable;
- `dockerfile`: anything in its directory;
- `ref`: another `image` (names compared as docker resolves them, so
  `node:22` and `docker.io/library/node:22` are one image).

**Or drift nobody asked for**:

- `apko`: a new caboose whose groups, or whose apko, change what the
  profile stands for, or a lock resolved again since the image was built
  from it;
- `ref`: a pull or rebuild of the same image since (a new image ID);
- any kind: a new caboose whose layer differs, or an image built for
  another host user (its UID or GID labels differ from yours).

With `auto_build = false` in `[build]`, a launch builds nothing: it
refuses to create a container on a base you changed, naming both, and
uses an image that merely drifted as it is, with a warning, until you run
`caboose build`.

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

Each platform in use keeps its own `keep_versions` versions, at
~224MB each. `caboose status` shows the disk used per platform dir, and
`caboose prune` only prunes the mounted one and names the others. A platform
dir no image uses any more is never deleted for you; remove
the data dir's `local/<platform>` by hand once you're sure.
