# caboose

This repo defines the Docker sandbox that Claude Code sessions run inside — so
a session working on it is running in the very container it builds. What that
means day to day is in the installed `~/.claude/CLAUDE.md`, whose source is
`sandbox/CLAUDE.md` here.

## Changes here need a rebuild, and it has to happen on the host

`Dockerfile`, `layer.Dockerfile`, `layer-user.sh`, `entrypoint.sh` and
`tmux.conf` are baked into the image. The launcher is a Go binary,
`./caboose`, built from `cmd/` and `internal/` and gitignored, with those
files, `sandbox/CLAUDE.md` and `imagecheck.sh` embedded in it; the bind
mounts it sets up are fixed when the container is created. None of it is
read live, so an edit alone changes nothing:

    make build      # rebuild the image (rebuilds ./caboose first if stale)
    make restart    # recreate the container (prompts; kills sessions)
    make test       # both, then the integration suite
    make lint       # bash -n + shellcheck, gofmt + go vet (+ staticcheck)
    make go-test    # Go unit tests; no docker, safe to run in here
    make test-byo   # the bring-your-own-image suite (host only; slow)

`make help` lists the rest. Prefer these over calling the launcher directly:
every target that runs it depends on `./caboose`, so make rebuilds it first
whenever a Go source or an embedded file changed. That matters because the
image is built from copies of those files embedded in the binary, not from
the checkout — a stale `./caboose` would build the old image.

A running container cannot rebuild the image it runs from, and recreating the
container kills the session doing the asking — so whenever you touch
`Dockerfile`, `layer.Dockerfile`, `layer-user.sh`, `entrypoint.sh`,
`tmux.conf` or the launcher's Go code, **say that a host terminal has to
run `make build && make restart`**, or the change looks applied and isn't
(`imagecheck.sh` is in no image: it needs only the launcher rebuilt). Never
build `./caboose` in here: the checkout is shared with the host, so a linux
binary there replaces the host's own, and `make launcher` refuses inside the
sandbox for that reason. `make go-test` and
`make lint` (go vet) compile every package without writing it; a real build
can go elsewhere (`make launcher LAUNCHER=/tmp/caboose`). Never run the
launcher either: it drives the host's docker, which from in here is either
absent or the container you are in. The Makefile records the platform
`./caboose` was built for in `caboose.platform` and rebuilds on a mismatch,
and stamps the version (`git describe`, commit, date) into
`internal/version` through `caboose.version`, rewritten only when it changes.

Users install release binaries with `install.sh` (goreleaser publishes the
GitHub release it downloads from), have no checkout, and are kept current by
the binary itself (`internal/selfupdate`); building from a checkout still
needs a Go toolchain on the host (the version in `go.mod`). `caboose build` labels the
image with the launcher version and a sha256 of the embedded build context
(`internal/assets`), and a launch compares that hash to its own to spot a
stale image — so any change to an embedded image file is a new hash, which
is the point. There are two contexts, hashed apart: the base (`Dockerfile`,
the default base image) and the layer (the rest), built FROM whichever base
is in use, after the image check passes on it. On `CABOOSE_BASE_IMAGE` the
`Dockerfile`'s hash plays no part; the base's image ID does. The layer is
also labelled with the kind of base, its name and ID, the host UID/GID and
the platform the check found (`linux-arm64-musl`, ...), which picks the
`dot_local/<platform>` a new container mounts; `USE_BUILTIN_RIPGREP=0` is
set at `docker run` on musl. Labels are inherited through `FROM`, and a
user's base may be built from a caboose image, so the layer sets every
label in `assets.LayerLabels`, `""` where one does not apply, and the
launcher reads empty as unset: a new label goes in that list.

`sandbox/CLAUDE.md` is the exception: a launcher run from a checkout copies
it into the data dir's `.claude/CLAUDE.md` on every start, so the next session
picks it up with no rebuild and no restart. An installed binary uses the copy
embedded in it instead.

## Layout

| | |
|---|---|
| `Dockerfile` | the default base image: OS packages and toolchains, nothing agent-specific; its optional parts are marked sections, which `caboose setup image` cuts it down to (`internal/assets/preset.go`) |
| `layer.Dockerfile` | the layer built on every base: the agent user, its home, the entrypoint, `tmux.conf`, `HOME`/`PATH`/`LANG` |
| `layer-user.sh` | the layer's user setup, POSIX sh editing /etc/passwd, group, shadow directly; tested by `layeruser_test.go` |
| `entrypoint.sh` | container entrypoint; bootstraps Claude Code, clears stale session state, prunes versions, idles as PID 1 |
| `tmux.conf` | tmux configuration baked into the image, set up to own no keys |
| `imagecheck.sh` | the image probe, POSIX sh, run by `caboose check-image` and before every layer build; reports facts only, embedded but in no image |
| `internal/imagecheck` | the requirements table the probe's output is judged against (the requirements table in `docs/images.md` mirrors it), and the checklist |
| `cmd/caboose`, `internal/` | the host launcher, in Go: subcommands and their help, from one table (`cmd/caboose/args.go`; nothing reaches claude but through `caboose claude`), environments and `config.toml` (`internal/config`), container lifecycle, bind mounts, SSH agent forwarding, repo roots at fixed `/work` paths, tmux attach, `caboose build` |
| `internal/shelltest` | the POSIX shells the tests run the shell scripts with: `/bin/sh` and whichever others the machine has, but never a BusyBox that runs its own applets before `PATH` (Debian's and Ubuntu's), which would bypass the tests' stub tools |
| `internal/proposal` | what a session proposes for `caboose apply` (`internal/launcher/apply.go`): the TOML format, its checks (an allowlist of parts, no control or bidi characters), reading through `nofollow` from the data dir's `proposals/` (mounted at `~/.caboose-proposals`), and `current/`, what the host tells sessions a proposal is made against |
| `internal/nofollow` | file access under a directory the container can also write (the data dir's `.claude`, the sync repo): never through a symlink or a hard link, in any component; tested against swaps |
| `internal/statesync` | `caboose sync`: the allowlist, which project keys sync (those under `/work`), JSON merge by key, and the export/merge/apply cycle in the data dir's `sync/`; files are handled on the host, git runs in the container (`docker exec`, repo mounted at `~/.caboose-sync`); tested against real git |
| `embed.go` | the `//go:embed` list: the files the launcher carries with it and builds the image from |
| `Makefile` | the maintenance entry points; also builds `./caboose` (gitignored) |
| `internal/version` | the launcher's version, commit and date: ldflags when stamped (make, goreleaser), else what the Go toolchain recorded, else `dev` |
| `.goreleaser.yaml` | release build: static darwin/linux × amd64/arm64 archives, `checksums.txt` and `install.sh` on the GitHub release |
| `install.sh` | the one-line install, POSIX sh: the latest release (or `CABOOSE_VERSION`), checked against `checksums.txt`, into the layout `internal/selfupdate` keeps, then `caboose setup`; tested by `install_test.go` against a fake release host |
| `internal/selfupdate` | updating an install made by `install.sh`: the latest tag from the `releases/latest` redirect, download and checksum, `versions/<tag>` and the `~/.local/bin/caboose` link, `update.json` and its lock; `releasetest` serves fake releases |
| `.github/workflows` | `ci.yml` (gofmt, vet, test, shellcheck); `release.yml` (goreleaser on a `v*` tag) |
| `sandbox/CLAUDE.md` | the global CLAUDE.md installed into the sandbox |
| `README.md`, `docs/`, `CONTRIBUTING.md` | a short README (logo in `docs/assets/`, badges, links); the user docs, one file per topic; building and testing caboose |
| `tests/run.sh` | integration suite; drives the real launcher against a real container |
| `tests/byo/` | bring-your-own-image suite (`make test-byo`): Debian and Alpine test bases, one shared throwaway data dir, stock images the check must refuse, and a preset built from an environment's `image/` dir (`tests/byo/preset` prints it) |

Nothing in this checkout is state. The live OAuth credential, transcripts and
the ~224MB version binaries live in `$CABOOSE_DATA_DIR` (default
`~/.caboose`, each environment's in `envs/<name>/data`), on the host,
outside the repo.

## Things that are easy to get wrong

- **Claude Code is not in the image.** It installs into `~/.local`
  (`bin` and `share/claude`, plus `~/.cache/claude`, bind-mounted from
  the data dir's `dot_local/<platform>/{bin,share/claude,cache/claude}`) so it
  updates itself in place. Do not reintroduce `npm install -g
  @anthropic-ai/claude-code` — a root-owned install would force
  `DISABLE_AUTOUPDATER=1`. The split by platform is because a build only
  runs on its own libc and arch (`internal/datadir/platform.go`).
- **caboose never installs packages into a base.** No apt, apk or dnf in
  `layer.Dockerfile`, `layer-user.sh` or anywhere the launcher runs against
  a user's image; the user owns it. What the sandbox needs is documented as
  a requirement and checked, and the layer only adds the user, the
  entrypoint and `tmux.conf`. Setting env at `docker run` (as
  `USE_BUILTIN_RIPGREP=0` on musl) is configuration, and fine.
- **Anything the layer's scripts call must be a probe requirement.** Every
  tool `entrypoint.sh`, `layer-user.sh` or the launcher's `docker exec`s use
  — beyond sh builtins — has to be checked by `imagecheck.sh`, listed in
  `requirements` in `internal/imagecheck`, and in the table in `docs/images.md`, or a
  base that passes the check can still fail to build or boot. Prefer a
  builtin over a new requirement (the entrypoint sorts versions in bash
  rather than rely on `sort -V`, which BusyBox may lack).
- **Some state is deliberately ephemeral.** `~/.local/state/claude` (lock
  files) and `~/.claude/daemon.lock` / `~/.claude/sessions/*` (PID-keyed, and
  meaningless once the processes are gone) must stay out of the data dir; the
  entrypoint clears them at start.
- **Container paths are fixed, not mirrored.** A single repo root is
  mounted at `/work`, each of several (`[roots]`) at `/work/<name>`, so a
  project's container path -- and the project key Claude Code derives from
  it, which `caboose sync` carries as it is -- is the same on every
  machine. Never build one as `"/work" + hostPath`: go through
  `config.ContainerPath`, with the roots the container actually mounts
  (`mountedRoots`), which differ from the configured ones until a restart.
- **This repo is not mounted at a path of its own.** It is visible only
  because it sits under the repo root like any other checkout, at its place
  under `/work`. Don't add a second mount for it.
- **Persistent state is not in this repo.** It lives in `$CABOOSE_DATA_DIR`
  (default `~/.caboose/envs/<env>/data`). Never put it under the checkout,
  even gitignored: `.gitignore` is tracked, so any history rewrite that drops it
  un-ignores the state, and the VCS then snapshots and deletes it.
- **Config a tool rewrites is mounted as a directory, never a file.** git,
  jj and gh save by writing a temp file and renaming it over the original,
  and a rename onto a single-file bind mount fails with `EBUSY` ("could
  not write config file /home/agent/.gitconfig: Device or resource busy"
  from `gh auth login`, and `git config --global` broken with it). They
  live under `dot_config/` at their XDG paths, and `~/.ssh` is the
  whole of `dot_ssh`. Keep `~/.gitconfig` from ever existing
  in the image, too: git writes `~/.config/git/config` only while it is
  absent. `.claude.json` is the one remaining file mount.
- **The image's build contexts are an allowlist.** `caboose build` writes the
  embedded files to empty temp dirs (one per context) and builds there,
  never from the checkout, so nothing unlisted can drift into a context. A
  new `COPY` in `layer.Dockerfile` needs its file added to the `//go:embed`
  line in `embed.go`, to `LayerContext` in `internal/assets` (the base
  `Dockerfile` COPYs nothing, and a test keeps it that way), and to
  `EMBEDDED` in the `Makefile` so edits to it rebuild the launcher.
  Forgetting the first two fails loudly — in `go test` and at build time —
  which is the point.
- **What `caboose sync` syncs is an allowlist, and a remote is untrusted.**
  Adding a synced path means `LiveTarget` and `ExportLive` in
  `internal/statesync` (and the table in `docs/sync.md`); anything not there is
  never exported, and a path a remote sends that `LiveTarget` refuses is
  never written — nor deleted from the repo, since a newer caboose may sync
  it. It still sits in the sync repo, where git obeys a `.gitattributes`
  and runs no hooks only because every git call passes
  `core.hooksPath=/dev/null`: the first line of `.git/info/attributes`
  unsets every content-changing or program-running attribute for all
  paths, so keep it first, and set any new attribute after it. Never make the data dir, or anything mounted from it, a git work
  tree: the sync repo is the data dir's `sync/`, and results are applied file
  by file.
- **A proposal is the sandbox asking; only the host decides.** What a
  session may propose is an allowlist -- a Dockerfile section, `[persist]`
  entries, one root (`internal/proposal`) -- and anything more is refused
  whole, never shown as a question. Never add a proposable part that
  widens what the sandbox reaches without a refusal list of its own, as a
  root has (`checkProposedRoot`: home, hidden dirs of it, caboose's own
  state, overlaps; judged and written as the physical path). Everything a
  proposal holds is shown on the host's terminal, so its text is refused
  with any control or invisible format character, and whatever else of the
  sandbox's is said (a file's name, an error quoting one) goes through
  `proposal.Printable`.
- **The host never follows a path the container can write.** Anything
  the launcher reads or writes inside a mounted dir of the data dir
  (`.claude`, `dot_config/*`, `sync/`, ...) goes through
  `internal/nofollow`, not `os`: the container can put a symlink or a hard
  link anywhere there, and `os.Root` alone follows one that stays inside
  the root -- the data dir holds `.credentials.json` and gh's token. The
  mount points themselves, and `.claude.json` (a file mount, which the
  container cannot replace), are the host's. Never point a host tool at
  such a file either: the host's `git config --file` writes through a
  symlink, so the sandbox's git config is read and written on a host-side
  copy (`datadir.editConfig`). The sync repo sets `core.symlinks=false`, and
  apply writes only blobs of mode 100644/100755.
- **On a Mac, never bind-mount the host's SSH agent socket.** It arrives in
  the engine's VM as an empty directory unless it is macOS's own launchd
  agent, so 1Password's never worked that way. OrbStack and Docker Desktop
  provide `/run/host-services/ssh-auth.sock`, which exists only in their
  VM and so cannot be checked for from the host: `sshAgentArgs` mounts it
  when `docker info` names either engine. Setup writes signing without
  `gpg.ssh.program` (`datadir.SigningChanges`) for the same reason: the
  host's is a Mac binary.
- **`[persist]` keeps sandbox state, never host paths.** Each entry is a
  directory under the container's home, kept in the data dir's
  `persist/<name>` (`datadir.PersistDir`) and mounted back at container
  creation; never let one name a host path -- that is what roots are for,
  and it would hand the sandbox host files. `config.ParsePersist` refuses
  anything in, holding, or inside one of caboose's own mounts
  (`config.ReservedHome`, which a test holds to the mounts
  `createContainer` makes: add a new home mount there too) and anything
  whose parent is not in `config.PersistParents` -- the directories
  `layer-user.sh` creates owned by the agent, since docker would create a
  deeper one's parents as root. `~/.ssh` (`dot_ssh`, `0700`) holds
  `known_hosts` and the sandbox's own ssh config, never keys: doctor notes
  one it finds (`datadir.PrivateKeysIn`, through `nofollow`).
- **A launch never writes the sandbox's git identity.** `caboose setup
  git` is its only writer (`datadir.WriteSandboxGit`); a launch and doctor
  only say when there is none. Never seed it from the host: that would
  give every new environment the host's identity without a word.
- **The Dockerfile's sections are setup's menu.** Everything after the OS
  packages sits between `# caboose:section NAME [off] TITLE` and
  `# caboose:end`, and carries its own `ARG`s and `ENV`s: a section must
  build without any other, since setup can leave any of them out. What the
  image check requires stays outside every section. An `off` section is
  commented out line by line (`# `), so the Dockerfile builds without it.
  `internal/assets` tests the markers; `make test-byo` builds the core
  with the off section for real.
- **An environment's `image/` dir is the user's.** A base built from it is
  labelled kind `env` with `assets.DirHash` of the whole dir, hashed before
  the build; nothing of caboose's ever rewrites it but `caboose setup
  image`, after showing the difference and asking. It is never mounted
  into the container, and it excludes `CABOOSE_BASE_IMAGE`
  (`config.ErrTwoBases`).
- **Setup edits `config.toml`, it never rewrites it.** Changes go through
  `config.EditFile` (line by line: only the keys asked about, comments
  kept, a top-level key always above the first table) and are read back
  with `config.CheckEdit` before `writeConfig` writes them; a result that
  does not say what was answered is not written. Never marshal the whole
  file: that would drop the user's comments and hand edits.
- **An environment is a whole caboose.** Everything that can differ between
  two -- data dir, container, image, config -- is derived from the
  environment in `internal/config`, never from a fixed name: the default
  one is `caboose` only through `config.ContainerFor`. A setting belongs in
  `config.toml` (`fileKeys`) and as a `CABOOSE_` variable, the variable
  winning; `[roots]` and `[persist]`, tables, are the ones without a
  variable (`CABOOSE_REPO_ROOT` overrides `[roots]` with a single root).
- **caboose updates itself; the container does not move with it.** An
  install made by `install.sh` (`selfupdate.Layout.Managed`) checks at
  most daily in a detached `caboose update --background`, and a checkout's
  build never does. So a new launcher routinely meets a container an older
  one created, with sessions in it, and has to keep working with it:
  entrypoint protocol, paths, mounts, what it execs. When a change cannot,
  raise `assets.Compat`: the layer is labelled with it, and a launch that
  finds a container of another compat refuses and says `caboose restart`.
  Never raise it for a mere change to the image's files: the hashes mark
  the image stale, and the launch that creates the container rebuilds it.
- **Releases, `install.sh` and `internal/selfupdate` are one contract.**
  The archive names (`caboose_<version>_<os>_<arch>.tar.gz`, caboose at the
  root), `checksums.txt`, the `releases/latest` redirect and the layout
  under `~/.local` are read by both installers; change them together, or
  every existing install stops updating. Prereleases are never
  `releases/latest`, so neither installer takes one unasked.
- **`sandbox/CLAUDE.md` is sandbox-wide.** It is read by every session in
  every project, so project-specific instructions — including everything in
  this file — do not belong there. It also ships to every user, so it names
  no one's paths: the launcher fills in `@@CABOOSE_DIR@@` and
  `@@CABOOSE_ROOTS@@` at install time (`internal/datadir`).
