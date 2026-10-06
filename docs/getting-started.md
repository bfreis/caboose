# Getting started

## Install

You need a running Docker engine — Docker Desktop, OrbStack or colima — and,
only if you build from source, a Go toolchain (the version in `go.mod`).
Nothing else: the image is not published anywhere, because the launcher
carries the `Dockerfile` it is built from and builds it on your machine — or
builds on an image of your own ([Use your own image](images.md)).

```sh
curl -fsSL https://github.com/bfreis/caboose/releases/latest/download/install.sh | sh
```

`install.sh` downloads the latest release for your OS and architecture
(macOS or Linux, arm64 or x86_64; the native one under Rosetta), checks it
against the release's `checksums.txt`, and installs it without root:
`~/.caboose/versions/<version>/caboose`, with
`~/.local/bin/caboose` a link to it (the one thing outside `~/.caboose`;
`CABOOSE_HOME=DIR` installs under `DIR` instead, and caboose must be run
with the same variable). It says so when `~/.local/bin` is not
on your `PATH`, or when no Docker engine answers, and then runs [`caboose
setup`](configuration.md#setup) on your terminal. `CABOOSE_VERSION=v1.2.3` installs
that release instead; `CABOOSE_NO_SETUP=1` installs only. Running it again
installs the latest next to what is there.

An install made this way [keeps itself up to date](#updates). The binary is
not signed or notarized, and doesn't need to be: a file downloaded by `curl` has
no quarantine flag for Gatekeeper to check.

`go install` is not supported: it makes an install that does not update
itself.

**From a checkout**, which is what you want if you mean to change it — see
[Working on caboose](../CONTRIBUTING.md):

```sh
make launcher                   # builds ./caboose (gitignored)
ln -s "$PWD/caboose" ~/.local/bin/caboose
```

## First run

Run `caboose` from inside a project under one of the [roots](configuration.md#roots) (`~/dev` by default). The first
time, it:

1. **Builds the image**, when there is none. It says so first, then builds
   in two steps: the base (the `Dockerfile` embedded in the binary, tagged
   `caboose-base:<env>`, or your own `base` in `[image]`, pulled if need be),
   which it [checks](images.md), and on top of it a
   small layer with the `agent` user at your UID and GID, so files the
   container writes into your repos belong to you. It takes a few minutes,
   once. The build log goes to stderr, and it builds without a TTY too.
   `auto_build = false` in `[image]` turns this off: a missing image is then an
   error telling you to run `caboose build`.
2. **Creates the container**, with the roots and the data dir mounted.
3. **Installs Claude Code** into the persisted `~/.local` on its first boot
   (up to `ready_timeout` seconds, 600 by default) — once per data dir and
   [platform](images.md#switching-between-images), not once per image build.
4. **Attaches a tmux session** for the project and starts `claude` in it.

Nothing has to be set up first: a launch runs on defaults, and says in one
line that `caboose setup` has not been run, and while the sandbox has no
git identity, that commits in it will fail. `caboose setup` asks for what
the defaults do not know: the [roots](configuration.md#roots), [what it is
built on](images.md), the sandbox's
[git identity and signing](configuration.md#git-identity-and-commit-signing), and whether
and where to [sync](sync.md).

Log in to Claude Code once, in that first session. The account is the
sandbox's own, kept in the data dir, and every later session and container
uses it.

## Updates

An install made by `install.sh` updates itself, as Claude Code does. At
most once a day, a caboose command starts a check in the background and
goes on without waiting for it: when there is a newer release, it is
downloaded, checked against its checksums and installed next to the one
running, and the next caboose you run is the new one, which says so once.
The version before it stays, for going back (point `~/.local/bin/caboose`
at it). `caboose update` checks and installs now, and `caboose doctor`
says when the last check was and whether it failed.
`CABOOSE_NO_AUTO_UPDATE=1` (or `true`) turns the automatic part off (`caboose update`
still works). A build from a checkout never updates itself, and neither
does a binary anywhere else.

A new caboose may build the sandbox's image differently. The container
keeps running on the image it was created from, sessions and all, and the
next launch that creates the container — `caboose restart`, which asks
first since it ends sessions — rebuilds the image before it removes the
old container, so a build that fails leaves it running. Until then, a
launch says the image is out of date. `caboose version` prints the
launcher's version, commit and build date, how it was installed, a hash of
the build context embedded in it, the image name it expects, the base it
builds on, and whether that image exists and was built from the same
context on the same base — `caboose build` records the hashes, and the
base's name and ID, as labels on the image. On the default base, the
embedded `Dockerfile`'s hash is the base's identity; on your own base it is
the image ID ([When your image changes](images.md#when-your-image-changes)). `caboose build` rebuilds
the image now, without touching the container.

Rarely, a new caboose cannot work with a container an older one created at
all (the image carries a compatibility number for this, raised only then).
A launch then refuses, and says to run `caboose restart`. A
container that lacks what caboose always records when it creates one (its
isolation, hostname, run arguments, roots, ...) is reported the same way
by `doctor` and the launch warnings. Claude Code
itself is unaffected either way: it lives in the data dir and updates
itself.
