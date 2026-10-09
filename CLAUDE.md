# caboose

This repo defines the Docker sandbox that Claude Code sessions run inside — so
a session working on it is running in the very container it builds. What that
means day to day is in caboose's managed instructions, installed read-only
at `/etc/claude-code/CLAUDE.md` (with managed skills beside it), whose source is
`sandbox/CLAUDE.md` and `sandbox/skills/` here.

## Changes here need a rebuild, and it has to happen on the host

`layer.Dockerfile`, `layer-user.sh`, `entrypoint.sh`, `xdg-open.sh`,
`tmux.conf`, `shellrc.bash` and `caboose-agent` (built from
`cmd/caboose-agent`, `internal/agent` and `internal/agentproto` into
`agent-bin/` by `make agent`) are baked into the image, and caboose's
package groups (`internal/apkobuild/pkgset/packages.toml`) decide an apko
base's packages. The launcher is a
Go binary, `./caboose`, built from `cmd/` and `internal/` and gitignored, with those
files, the seed `Dockerfile`, `sandbox/CLAUDE.md`, `sandbox/skills` and `imagecheck.sh` embedded in it; the bind
mounts it sets up are fixed when the container is created. None of it is
read live, so an edit alone changes nothing:

    make build      # rebuild the dev environment's image (rebuilds ./caboose first if stale)
    make restart    # recreate the dev environment's container (prompts; kills its sessions)
    make test       # the test environment's image, then the integration suite in it
    make lint       # bash -n + shellcheck, gofmt + go vet + staticcheck (CI runs it)
    make go-test    # Go unit tests; no docker, safe to run in here
    make test-vm    # the integration suite's vm group alone (a Mac; slow)
    make test-byo   # the bring-your-own-image suite (host only; slow)

These act on environments of their own: `build`, `restart` and the rest on
`dev` (`DEV_ENV`), `test` on `test` (`TEST_ENV`, which a bare `tests/run.sh`
also defaults to), each created on defaults the first time. The sandbox a
session works in is the default environment, on a release, and moves only
when that updates itself; `DEV_ENV=default` aims the targets at it.

`make help` lists the rest. Prefer these over calling the launcher directly:
every target that runs it depends on `./caboose`, so make rebuilds it first
whenever a Go source or an embedded file changed. That matters because the
image is built from copies of those files embedded in the binary, not from
the checkout — a stale `./caboose` would build the old image.

A running container cannot rebuild the image it runs from, and the launcher
cannot run in here -- so whenever you touch
`layer.Dockerfile`, `layer-user.sh`, `entrypoint.sh`, `xdg-open.sh`,
`tmux.conf`, `shellrc.bash`, `packages.toml`, the agent's or the launcher's Go code, **say that a host
terminal has to run `make build && make restart`** to try it (in the `dev`
environment; the sandbox you are in moves only with a release), or `make
test` to test it, or the change looks applied and isn't
(`imagecheck.sh` and the seed `Dockerfile` are in no image: they need
only the launcher rebuilt, and the seed reaches only a dockerfile
profile's dir that setup or a proposal seeds after that). Never
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
image with the launcher version and a sha256 of the layer's embedded build
context (`internal/assets`), and a launch compares that hash to its own to
spot a stale image — so any change to an embedded layer file is a new
hash, which is the point. The layer is built FROM whichever base the image
profile (`image = "<kind>.<name>"` in `config.toml`) gives, after the image
check passes on it, and is labelled with the kind of base
(`LabelBaseKind`: `BaseKindApko`, `BaseKindDockerfile`, `BaseKindRef`) and
what identifies it (`LabelBaseHash`): an apko lock's `Hash`, which mixes in
`ApkoVersion` and the input caboose's groups made; a dockerfile profile's
dir's `assets.DirHash`; `""` on a ref, whose image ID (`LabelBaseID`) is
compared instead. A change the user made (another profile, its packages,
its dir edited, another ref) is a switched base, rebuilt at creation even
past a warning; caboose's groups or apko changing, or a ref's new ID, is
drift (`classifyImage`, `classifyLock`). The layer is
also labelled with the base's name and ID, the host UID/GID and
the platform the check found (`linux-arm64-musl`, ...), which picks the
`local/<platform>` a new container mounts; `USE_BUILTIN_RIPGREP=0` is
set at `docker run` on musl. Labels are inherited through `FROM`, and a
user's base may be built from a caboose image, so the layer sets every
label in `assets.LayerLabels`, `""` where one does not apply, and the
launcher reads empty as unset: a new label goes in that list.

`caboose-vmm` (`cmd/caboose-vmm`, `internal/vm/...`) is in no image
either: it is a Mac binary beside the launcher, which only `make vmm` on
a Mac builds and signs (the Makefile refuses anywhere else), so a change
to it needs that, and a VM restarted with it.

`sandbox/CLAUDE.md` and `sandbox/skills/` are the exception: a launcher run
from a checkout expands them on every start into the data dir's
`claude-code/` (`CLAUDE.md` and `.claude/skills/`), which the sandbox has
read-only as Claude Code's managed `/etc/claude-code`, so the next session
picks them up with no rebuild and no restart. An installed binary uses the
copies embedded in it instead. A checkout's set that the launcher
cannot use is refused as a whole and the embedded one is installed, with a
note: a file that is a link, not plain or over the limits (fix it in the
checkout), or a placeholder or `@@IF` key the launcher does not know (rebuild
`./caboose`). The managed files are read through `internal/nofollow`, bounded.

## Layout

| | |
|---|---|
| `Dockerfile` | the seed: what `caboose setup image` (or the first section a proposal applies) writes into a dockerfile image profile's dir that has no Dockerfile (`assets.Seed`, `internal/assets/seed.go`, under a header saying it is the user's now). OS packages and toolchains, nothing agent-specific, its optional parts in marked sections; in no image of its own, and no build reads it |
| `internal/apkobuild` | apko bases, apko as a library: `Resolve` a profile's spec (`Spec`: its packages, and whether the default groups come) into a `Lock` (apko's lock format, the spec and the list it stood for kept in its config name), `Build` a lock into an image tarball at a fixed time (one lock, one digest), `Check` a spec against the signed indexes alone (no package downloaded: the link's proposal check), and `PruneCache` (`cache.go`: apko's cache layout, matched to the locks' URLs); `network_test.go` runs against Wolfi, the rest against a fixture repository served from memory |
| `internal/apkobuild/pkgset` | caboose's package groups, `packages.toml` (Wolfi names: one required group, default groups, off ones), and the checks on package names, apart from apko so that `internal/config` and the agent need not link it |
| `internal/launcher/apko.go`, `linkcheck.go`, `apply_packages.go` | an apko base in the launcher: when the lock is resolved again, building it into `docker load` or the vm builder, writing the lock after; the package cache (`CABOOSE_HOME/cache/apk`) and `caboose prune`'s pruning of it; the link's check of `[packages]` proposals as they arrive (`NAME.check`); `caboose apply` building a `[packages]` proposal before writing it |
| `layer.Dockerfile` | the layer built on every base: the agent user, its home, the entrypoint, `tmux.conf`, `HOME`/`PATH`/`LANG` |
| `layer-user.sh` | the layer's user setup, POSIX sh editing /etc/passwd, group, shadow directly; tested by `layeruser_test.go` |
| `entrypoint.sh` | container entrypoint; points root's home at `HOME`, and its shell at bash, when it runs as root (vm, some gVisor), bootstraps Claude Code, clears stale session state, prunes versions, runs the user's `start.d` in the background, idles under tini |
| `xdg-open.sh` | the sandbox's `xdg-open`, at `/usr/local/bin/xdg-open` (with `BROWSER` set to it): runs `caboose-agent xdg-open`, which refuses anything but an http(s) URL and hands that to `open`. It wins over a base's `/usr/bin/xdg-open` on purpose, and exists only in the image: nothing of the host's or the agent's Go code runs or reads it |
| `shellrc.bash` | the `shell.d` loader every interactive bash sources, through the line `layer-user.sh` adds to `~/.bashrc`; tested by `shellrc_test.go` |
| `tmux.conf` | tmux configuration baked into the image, set up to own no keys |
| `imagecheck.sh` | the image probe, POSIX sh, run by `caboose check-image` and before every layer build; reports facts only, embedded but in no image |
| `internal/imagecheck` | the requirements table the probe's output is judged against (the requirements table in `docs/images.md` mirrors it), and the checklist |
| `cmd/caboose`, `internal/` | the host launcher, in Go: subcommands and their help, from one table (`cmd/caboose/args.go`; nothing reaches claude but through `caboose claude`), environments and `config.toml` (`internal/config`), container lifecycle, bind mounts, SSH agent forwarding, roots at fixed container paths, tmux attach, `caboose build` |
| `internal/backend` | the sandbox as the launcher sees it: `Backend` (state, create from one `Spec`, start, stop, remove, exec, labels, mounts, logs) and its docker implementation, which serves the docker and gvisor isolations; images, builds and what the engine says of itself stay `internal/docker`'s. `backendtest` is an in-memory one, which the launcher's tests of the sandbox's lifecycle run under every isolation (`launcher/box_test.go`), failing any that asks docker for the sandbox |
| `internal/shelltest` | the POSIX shells the tests run the shell scripts with: `/bin/sh` and whichever others the machine has, but never a BusyBox that runs its own applets before `PATH` (Debian's and Ubuntu's), which would bypass the tests' stub tools |
| `internal/proposal` | what a session proposes for `caboose apply` (`internal/launcher/apply.go`): the TOML format, its checks (an allowlist of parts, no control or bidi characters), reading through `nofollow` from the data dir's `proposals/` (mounted at `~/.caboose-proposals`), and `current/`, what the host tells sessions a proposal is made against |
| `internal/sandboxcfg` | the sandbox config, `~/.config/caboose/sandbox.toml`: `[[keep]]` entries (what of the home is kept, and mounted) and their sync rules (globs, first match wins, excludes, merge drivers), checked entry by entry; its defaults, format and line-based edits (`caboose sync add/rm`) |
| `internal/nofollow` | file access under a directory the container can also write (the data dir's `home/`, the sync repo): never through a symlink or a hard link, in any component; tested against swaps |
| `internal/statesync` | `caboose sync`: the sandbox config's rules applied to the sandbox's home and to what a remote sends, JSON merge by key, and the export/merge/apply cycle in the sync repo (the data dir's `sync/`, mounted at `~/.caboose-sync`), skipping keep entries the sandbox does not mount yet; it runs in the sandbox, files and git alike, and the host only holds the lock (`statesync.Lock`) and reads hints; tested against real git |
| `internal/syncagent` | the sync's run in the sandbox (`caboose-agent sync run\|status`: git's environment, the budget, askpass relaying git's and ssh's prompts, a lock of its own in the repo's `.git`, and stopping when the host's stdin closes) and its protocol with the host, newline-delimited JSON on the exec's stdin and stdout, every line bounded (`Run` is the host's end, which `internal/launcher/sync.go` drives); tested with the test binary as the agent |
| `cmd/caboose-agent`, `internal/agent` | the sandbox's end of the host link, a static linux binary in the image: `caboose-agent link` (run by the host through `docker exec -i`) serves `open`, `notify`, `ports` and `host` to the sandbox on a Unix socket, watches `/proc/net/tcp*` for listening ports, connects the host's streams to them, touches the paths the host says changed (`touch.go`), runs `caboose sync` in the sandbox (`sync`, `askpass`: `internal/syncagent`), and under vm serves the outbound proxy the host's hello offers, an HTTP proxy whose every connection the host dials (`egress.go`; `caboose-agent connect` for ssh, which a vm guest's agent points ssh at as it boots: `sshegress_linux.go`) |
| `internal/agentproto` | the link's protocol: streams multiplexed over one byte stream, with per-stream flow control, control messages on stream 0, and hard limits, since the host reads what the container writes |
| `internal/hostlink` | the host's end: forwards what `forward_ports` allows to `127.0.0.1`, dials a vm guest's outbound connections as `egress_ports` and `egress_allow` say (`egress.go`), opens http(s) URLs as `open_urls` says, shows notifications, relays file changes under the roots into a gVisor container (`relay.go`), runs a session's `caboose-agent host` command here when `host_exec` says so, serving the exec port's protocol the other way round (`hostexec.go`); `caboose link` (`internal/launcher/link.go`) runs it detached, one per environment by a lock in the data dir |
| `internal/fswatch` | the host's watcher for that relay: FSEvents through purego on a Mac (the launcher has no cgo), inotify on Linux; only paths, never what happened |
| `internal/launcher/vm*.go` | the vm isolation in the launcher: finding `caboose-vmm`, the kernel and the builder disk (`~/.caboose/vm/<tag>/<arch>/`, else the checkout's `vm-dist/`), the VM's size, the initramfs from the embedded agent, the `VMHost`, `caboose build` through the builder, and the image store of root disks (`vm/images/`), behind the same `imageStore` as docker's |
| `internal/backend`'s `VM`, `internal/vm` | the vm isolation as the launcher sees it: a VM's dir (`vm/<name>/`: `vm.sock`, `state.json`, the launcher's record, never shared; `machine.json`, what vmm boots), the control port's client, the clock keeper (a guest's clock stops while the Mac sleeps), the exec helper `Command` runs (the launcher as a hidden `__vm-exec`), the initramfs and the scratch disk's clone; tested against a fake vmm |
| `internal/hvsock` | Cloud Hypervisor's hybrid vsock, which `vm.sock` speaks: `CONNECT <port>`, `OK`, then the guest's bytes; both halves |
| `cmd/caboose-vmm`, `internal/vm/vmm`, `internal/vm/vz` | `caboose-vmm`, a binary of its own beside the launcher, since it alone carries the virtualization entitlement: the detached process that owns one VM (`vmm`: the lock, `vmm.pid`, serving `vm.sock`, the clock, shutdown on SIGTERM) on a `Runner`, which on a Mac is Virtualization.framework through purego (`vz`). `make vmm` builds and signs it on a Mac; `internal/vm/e2e` boots a real VM with it, run on a Mac by hand |
| `vm/builder/`, `internal/vm/builder` | the vm isolation's builder guest: `vm/builder` is its disk's recipe (Alpine with dockerd, e2fsprogs and GNU tar, written by `mkfs.ext4 -d`; `make vm-builder`, docker on the host, writes `vm-dist/builder-<arch>.img`) and `caboose-builder`, its side of a build; `internal/vm/builder` boots it (overlay on tmpfs, a kept cache disk, the output disk, the scratch template) and runs caboose build's steps in it over the exec port: the contexts as tars, the image check through `imagecheck.Args`, the layer as a root disk, the image's config |
| `vm/kernel/` | the vm isolation's guest kernel: kernel.org's 6.18 LTS, unpatched, pinned in the Makefile (`LINUX_VERSION`, `LINUX_SHA256`); `config-common` and `config-<arch>` merged onto allnoconfig, `check-config` (awk) failing the build on any line Kconfig did not take, and the `Dockerfile` that builds it reproducibly for each build machine's architecture (Debian by digest and snapshot date; `make vm-kernel`, docker on the host, writes `vm-dist/kernel-<arch>` and its `.config`); `smoke/`, `make vm-kernel-smoke`: a test init booted under QEMU in a container. `internal/vm/kernelconfig_test.go` holds the fragments to the decisions |
| `agent-bin/` | `caboose-agent` for amd64 and arm64, from `make agent`, gitignored but for its README; embedded in the launcher, and the layer COPYs the one for its `TARGETARCH` |
| `embed.go` | the `//go:embed` list: the files the launcher carries with it and builds the image from |
| `Makefile` | the maintenance entry points; also builds `./caboose` (gitignored) |
| `internal/version` | the launcher's version, commit and date: ldflags when stamped (make, goreleaser), else what the Go toolchain recorded, else `dev` |
| `.goreleaser.yaml` | release build: static darwin/linux × amd64/arm64 archives (the darwin ones with `caboose-vmm`; both darwin binaries signed by `macsign.sh` with `rcodesign`, which `release.yml` installs: with a Developer ID and notarized when the repository has the signing secrets, else `caboose-vmm` ad-hoc, CONTRIBUTING.md), `checksums.txt` and `install.sh` on the GitHub release |
| `macsign.sh` | POSIX sh: signs a darwin binary with `rcodesign`, with a Developer ID (hardened runtime) and notarized when `MACSIGN_*` name the files (one submission, polled with retries until a verdict or `MACSIGN_NOTARY_TIMEOUT`; `macsign_test.go` stubs `rcodesign`), else ad-hoc with `--adhoc`; goreleaser's post-build hooks and `make vmm` run it |
| `install.sh` | the one-line install, POSIX sh: the latest release (or `CABOOSE_VERSION`), checked against `checksums.txt`, into the layout `internal/selfupdate` keeps, then `caboose setup`; tested by `install_test.go` against a fake release host |
| `internal/selfupdate` | updating an install made by `install.sh`: the latest tag from the `releases/latest` redirect, download and checksum, `CABOOSE_HOME/versions/<tag>` and the `~/.local/bin/caboose` link (the only thing of an install outside `CABOOSE_HOME`), `update.json` and its lock; `releasetest` serves fake releases |
| `.github/workflows` | `ci.yml` (gofmt, vet, test, shellcheck); `release.yml` (goreleaser on a `v*` tag) |
| `sandbox/CLAUDE.md`, `sandbox/skills/` | caboose's instructions to every session, a template, and its managed skills (`caboose-propose`, `caboose-persist`, `caboose-troubleshoot`), expanded per launch (`internal/datadir/instructions.go`) and installed read-only under `/etc/claude-code` |
| `README.md`, `docs/`, `CONTRIBUTING.md` | a short README (logo in `docs/assets/`, badges, links); the user docs, one file per topic; building and testing caboose |
| `tests/run.sh` | integration suite; drives the real launcher against a real container |
| `tests/byo/` | bring-your-own-image suite (`make test-byo`): Debian and Alpine test bases as ref profiles, one shared throwaway data dir, stock images the check must refuse, and a dockerfile profile's dir (`envs/<env>/dockerfile/default`) built from the seed with the off section (`tests/byo/seed` prints it) |

Nothing in this checkout is state. The live OAuth credential, transcripts and
the ~224MB version binaries live in `$CABOOSE_DATA_DIR` (default
`~/.caboose`, each environment's in `envs/<name>/data`), on the host,
outside the repo.

## Things that are easy to get wrong

- **Claude Code is not in the image.** It installs into `~/.local`
  (`bin` and `share/claude`, plus `~/.cache/claude`, bind-mounted from
  the data dir's `local/<platform>/{bin,share/claude,cache/claude}`) so it
  updates itself in place. Do not reintroduce `npm install -g
  @anthropic-ai/claude-code` — a root-owned install would force
  `DISABLE_AUTOUPDATER=1`. The split by platform is because a build only
  runs on its own libc and arch (`internal/datadir/platform.go`).
- **caboose never installs packages into a base.** No apt, apk or dnf in
  `layer.Dockerfile`, `layer-user.sh` or anywhere the launcher runs against
  a base; a ref image and a dockerfile profile's are the user's, and hold
  what they put there. An apko base is not installed into either: it is
  caboose's own build of the list the profile gives (caboose's groups and
  the user's packages), unpacked by apko, which runs no package's scripts,
  so nothing of a package runs on the host. Every base, apko's too, then
  goes through the same image check and the same layer. What the sandbox
  needs is documented as a requirement and checked (and is the required
  group in `packages.toml`), and the layer only adds the user, the
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
- **The launcher touches the sandbox only through `Backend`.** Running a
  command in it, creating, starting, stopping or removing it, and reading
  its labels or mounts go through `App.box()` (`internal/backend`), never
  `a.Docker` with the container's name: vm will be a second `Backend`, with
  no docker between the launcher and the guest. What `createContainer`
  decides goes in the one `backend.Spec`, which each backend realises its
  own way; `a.Docker` is for the engine (images, builds, `docker info`, the
  isolation probe) and for `caboose logs`, which passes docker's own flags.
- **Container paths are fixed, not mirrored.** Each `[roots]` entry is
  mounted at `/work/<name>`, or at the `path` of its long form (a sole root
  may take `/work` itself), so a project's container path -- and the
  project key Claude Code derives from it, which `caboose sync` carries as
  it is -- is the same on every machine. Container paths never nest and
  never reach the home, caboose's mounts or a system directory
  (`config.CheckContainerPath`), as host paths never overlap. Never build
  one as `"/work" + hostPath`: go through `config.ContainerPath`, with the
  roots the container actually mounts (`mountedRoots`, read from the
  container's roots label), which differ from the configured ones until a
  restart. The sync and the sandbox config compare container paths, not
  root names.
- **This repo is not mounted at a path of its own.** It is visible only
  because it sits under a root like any other checkout, at its place
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
  are kept at their XDG paths (`~/.config/git`, ...), each a directory
  `[[keep]]` entry. Keep `~/.gitconfig` from ever existing in the image,
  too: git writes `~/.config/git/config` only while it is absent.
  `.claude.json` is a file entry caboose itself needs; a user's file entry
  is allowed, and doctor notes the risk.
- **The layer's build context is an allowlist.** `caboose build` writes the
  layer's embedded files to an empty temp dir and builds there, never from
  the checkout, so nothing unlisted can drift into it. A base's context is
  never caboose's: apko builds from the lock alone, a dockerfile profile's
  is its own dir, a ref has none. A new `COPY` in `layer.Dockerfile` needs
  its file added to the `//go:embed` line in `embed.go`, to `LayerContext`
  in `internal/assets`, and to `EMBEDDED` in the `Makefile` so edits to it
  rebuild the launcher. Forgetting the first two fails loudly — in `go
  test` and at build time — which is the point. The seed `Dockerfile`
  (`BaseContext`) COPYs nothing, and a test keeps it that way: it is
  written into a dir that holds nothing else.
- **What `caboose sync` syncs is the sandbox config's rules, and a remote
  is untrusted.** Syncing a new path is a rule in the sandbox config, never
  code; what code decides is `sandboxcfg.Denied` (the login, whatever a
  rule says), the merge drivers, and the secret scan. A file no rule on
  this machine names is never exported, and a path a remote sends that
  `LiveTarget` refuses is never written — nor deleted from the repo, since
  another machine's rules, or a newer caboose, may sync it. A path this
  machine has only just started syncing (named by its rules, not by the
  last sync's, kept in the repo's `.git/caboose-rules.toml`) is adopted from
  the repo, never read as deleted. So a rule is read from the file alone:
  `{roots}` (project memory, by default) is the file's own `roots`, never
  this machine's mounts, which would make a rule mean something different
  on each machine and change it unrecorded. The sync's own git never reads the
  sandbox's git config (`GIT_CONFIG_GLOBAL=/dev/null`): it can arrive by
  sync, and must not redirect the push. It still sits in the sync repo, where git obeys a `.gitattributes`
  and runs no hooks only because every git call passes
  `core.hooksPath=/dev/null`: the first line of `.git/info/attributes`
  unsets every content-changing or program-running attribute for all
  paths, so keep it first, and set any new attribute after it. Never make the data dir, or anything mounted from it, a git work
  tree: the sync repo is the data dir's `sync/`, and results are applied file
  by file.
- **A proposal is the sandbox asking; only the host decides.** What a
  session may propose is an allowlist -- a Dockerfile section, one root
  (`internal/proposal`) -- and anything more is refused
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
  (`home/`, `sync/`, `proposals/`) goes through
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
  VM and so cannot be checked for from the host: `sshAgentSource` mounts it
  when `docker info` names either engine. Setup writes signing without
  `gpg.ssh.program` (`datadir.SigningChanges`) for the same reason: the
  host's is a Mac binary.
- **The sandbox config keeps sandbox state, never host paths, and is the
  sandbox's.** Each `[[keep]]` entry is a path under the container's home,
  kept at the same path under the data dir's `home/` and mounted back at
  container creation; never let one name a host path -- that is what roots
  are for, and it would hand the sandbox host files -- and never give the
  sandbox config a field that reaches the host (a root's path, the image):
  a session writes it, and it syncs from other machines. `sandboxcfg`
  refuses an entry in, holding, or inside one of caboose's own mounts
  (`sandboxcfg.Reserved`, which a test holds to the mounts
  `createContainer` makes: add a new home mount there too) and one whose
  parent is not in `sandboxcfg.Parents` -- the directories `layer-user.sh`
  creates owned by the agent, since docker would create a deeper one's
  parents as root. A bad entry is skipped, never the whole file; a file
  that cannot be used is replaced by the last good copy
  (`datadir.LoadSandboxConfig`), and nothing syncs until it is fixed.
  `~/.ssh` holds `known_hosts` and the sandbox's own ssh config, never
  keys: doctor notes one it finds (`datadir.PrivateKeysIn`, through
  `nofollow`).
- **`run_args` is the host's, and leaves caboose's flags alone.**
  The user's own `docker run` arguments are `run_args` in a `container` or
  `gvisor` profile of `config.toml`, never in the sandbox config or a proposal:
  they can hand the sandbox the host. They go in after caboose's, where
  docker takes the last of a single-value flag, so `checkRunArgs`
  (`internal/launcher/runargs.go`) refuses the flags caboose sets or
  depends on, its variables and labels, and mounts over its own (read off
  the `-v`/`-e` it built, plus `/work` and `datadir.ManagedTarget`,
  refused even with none built). A new flag caboose passes at `docker run` goes
  in `ownedFlags` there. The container is labelled with them
  (`assets.LabelRunArgs`, set even when empty) to tell when a restart is
  due.
- **The isolation is `config.toml`'s, and it picks the user.**
  `isolation` (a profile `<kind>.<name>`, the kind `container`, `gvisor` or
  `vm`) is the host's key, like `run_args`, never the sandbox config's. `--runtime` is in
  `ownedFlags` because the runtime decides who can write the mounts:
  `createContainer` probes it (`agentCanWrite`, `launcher/isolation.go`)
  and runs as `0:0` with `IS_SANDBOX=1` only where the agent cannot, and
  only under gVisor, whose root is inside its own kernel. Never run as root
  under runc, and never pick the user by engine name. The container is
  labelled with the kind (`assets.LabelIsolation`), the profile
  (`LabelProfile`, `<kind>.<name>`, or the bare kind when config.toml
  defines none: a switch between two profiles of a kind is drift too) and
  `LabelUser`, set even when empty. Under gVisor a closed terminal's tmux client lingers, so a
  launch records each attach by host PID in the data dir's `attached/`
  (`launcher/attach.go`) and takes back a session whose clients all
  belong to ended launches; that dir stays out of every mount, or the
  sandbox could plant records. The `runsc` that `caboose setup isolation`
  downloads for OrbStack (`launcher/runsc.go`) is run by the engine as
  root in its VM, so it lives in `CABOOSE_HOME/runsc/`, outside every
  data dir and mount; the engine's daemon config is the user's, edited
  only after showing the difference and asking, keys kept in order. A
  container keeps its runtime for life, so a stopped one whose runtime
  docker has lost can never start: a launch recreates it with the
  configured isolation (`runtimeGone`), and any failed `docker start` says
  what to do (`startFailed`). Never leave the user at a bare docker error.
- **A launch never writes the sandbox's git identity.** `caboose setup
  git` is its only writer (`datadir.WriteSandboxGit`); a launch and doctor
  only say when there is none. Never seed it from the host: that would
  give every new environment the host's identity without a word.
- **The seed's sections are what proposals find.** Everything after the
  OS packages in the seed `Dockerfile` sits between `# caboose:section
  NAME [off] TITLE` and `# caboose:end`, and carries its own `ARG`s and
  `ENV`s: a section must build without any other. A `[section]` proposal
  is one more of them, added to the dockerfile profile's Dockerfile or
  replacing one of its name (`assets.SetSection`), which is why the seed
  keeps them marked in the user's copy. What the image check requires
  stays outside every section. An `off` section is commented out line by
  line (`# `), so the Dockerfile builds without it, and `assets.Seed`
  uncomments one it is asked for. `internal/assets` tests the markers;
  `make test-byo` builds the seed with the off section for real.
- **A dockerfile profile's dir is the user's.** It is always
  `<env>/dockerfile/<name>/` (`config.DockerfileDir`): a fixed place,
  which is why `[dockerfile.NAME]` has no dir key -- no setting, a
  proposal included, can point a build at another host path. A base built
  from it is labelled kind `dockerfile` with `assets.DirHash` of the whole
  dir, hashed before the build. Nothing of caboose's ever writes into it
  but the seed, into a dir with no Dockerfile (`caboose setup image`, or
  the first `[section]` applied), and `caboose apply`'s sections, after
  showing the diff and asking. It is never mounted into the container.
- **Setup and apply edit `config.toml`, they never rewrite it.** Changes go
  through `config.EditFile` (line by line: only the keys asked about, by
  dotted name, in their table, comments kept, a top-level key always above
  the first table; `Tables` makes an empty profile table, as
  `[dockerfile.default]`, from the template's commented header when there
  is one) and are read back with `config.CheckEdit` before `writeConfig`
  writes them; a result that does not say what was answered is not
  written. Never marshal the whole file: that would drop the user's
  comments and hand edits.
- **An apko lock is the image's record.** `<env>/apko-<name>.lock.json`
  (`config.LockPath`) is written only once the image it built is in
  (`apkoBase`, after the load or the vm builder took it): a resolve or a
  build that fails leaves the last good lock, and the image it describes,
  and a `[packages]` proposal writes the profile's packages only after its
  build. The profile's spec is recorded in the lock (its config name,
  beside the list it stood for), which is how a user's change (another
  spec: switched, rebuilt at creation) is told from caboose's (the same
  spec standing for another list: the groups changed with the launcher,
  drift) and from a change that asks for the same packages (`WithSpec`
  rewrites the record, no resolve). `ApkoVersion` is mixed into every
  lock's hash: bump it with apko in `go.mod`, or a new apko's images look
  current. `caboose prune` keeps every cached package any environment's
  lock names, and none it cannot be sure of: an unreadable lock prunes
  nothing. The link's check of a `[packages]` proposal fetches the indexes
  only, never a package (`apkobuild.Check`), each check bounded in time
  and the checks budgeted per hour, since the sandbox decides how often
  it writes one; past the budget, `apply` checks.
- **An environment is a whole caboose.** Everything that can differ between
  two -- data dir, container, image, config -- is derived from the
  environment in `internal/config`, never from a fixed name: the default
  one is `caboose-default` through `config.ContainerFor` (`ImageFor`,
  `BaseImageFor`: `caboose:<env>`, `caboose-base:<env>`). A setting belongs
  in `config.toml`, in its table, and nowhere else: `CABOOSE_` variables
  name where things live (`CABOOSE_ENV`, `CABOOSE_HOME`,
  `CABOOSE_DATA_DIR`, `CABOOSE_SESSION`) or are two strict booleans
  (`CABOOSE_FORCE`, `CABOOSE_NO_AUTO_UPDATE`). An old layout leaves no
  trace: no table of moved keys or variables, no hint for an old key,
  which is an unknown key like any other. The file is read as tables: a
  key or table it does not know is an error naming it, and each isolation kind
  (`[container.NAME]`, `[gvisor.NAME]`, `[vm.NAME]`) and image kind
  (`[apko.NAME]`, `[dockerfile.NAME]`, `[ref.NAME]`) has its own keys
  (`profileKeys`, `imageProfileKeys`), so a key a kind lacks cannot be
  written; there is no inheritance between profiles.
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
  root, and `caboose-vmm` beside it in darwin's), vm's asset
  (`caboose-vm_<version>_<arch>.tar.gz`, `make vm-assets`, which a release
  build fetches: `internal/launcher/vmassets.go`), `checksums.txt`, the `releases/latest` redirect and the layout
  (`CABOOSE_HOME/versions/<tag>/`, the one `~/.local/bin/caboose` link, and
  `CABOOSE_HOME` itself) are read by both installers; change them together, or
  every existing install stops updating. Prereleases are never
  `releases/latest`, so neither installer takes one unasked.
- **The guest kernel is a pin, and its source is a release of its own.**
  `vm/kernel` builds kernel.org's tarball unmodified: bumping it is
  `LINUX_VERSION` and `LINUX_SHA256` together, the sha256 from
  `sha256sums.asc` with its signature checked (kernel.org's autosigner
  key, `632D3A06589DA6B1`, from its pgpkeys repository), on every 6.18.y
  security release and at least monthly; the `Dockerfile`'s Debian digest
  and snapshot date move only on purpose, since the compiler is part of
  the Image. Every option a fragment names is checked by `check-config`,
  so an option Kconfig refuses stops the build; add a new one to a
  fragment, never by editing a `.config`, and mind that allnoconfig with
  `EXPERT` on turns off whatever the fragments do not name. Modules,
  io_uring and anything that writes the running kernel stay off
  (`kernelconfig_test.go`). The GPL is met by the release `kernel-<version>`
  that `release.yml` makes in the releasing repository (the tarball once,
  each release's config beside it) and that `kernel-<arch>.SOURCE` points
  to: never delete one while a release built from it is installable, and
  keep it out of `releases/latest`: a prerelease (`--prerelease
  --latest=false`, set again on every run), since `--latest=false` alone
  still leaves it the latest on a repository with no other full release.
  Both installers refuse a latest tag that is no caboose version anyway.
  The Image is reproducible per build machine's architecture only, so a
  release builds vm's asset on an arm64 runner (`release.yml`'s
  `vm-assets` job, `ubuntu-24.04-arm`, as ci.yml's kernel job), the same
  Image a Mac's `make vm-assets` gives, and goreleaser's amd64 job only
  checks it (`VM_ASSETS_PREBUILT=1`). A private repository may need arm64
  runners enabled: without one that job sits queued, and nothing releases.
- **`start.d` and `shell.d` are the user's, and the sandbox's.** Both sit
  in the data dir's `home/.config/caboose`, mounted at `~/.config/caboose`,
  and a session may write them: they run as the agent, in the container,
  and widen nothing, which is why they are not a proposal. The host never
  runs them and reads them only through `nofollow`
  (`datadir.StartScripts`, for doctor). `start.d` runs one script at a time
  in byte order, never through a pipe (a daemon would hold it open), and
  after the ready file, so it never holds up a launch; tini signals the
  entrypoint's whole process group (`TINI_KILL_PROCESS_GROUP`) so what the
  scripts left running hears the stop. `shellrc.bash` sources at the top
  level, not in a function, where a file's `declare`s would turn local, so
  every name of its own is `__caboose_`-prefixed and unset after.
- **The host link trusts nothing the agent sends.** `caboose-agent` runs
  in the sandbox, so everything `internal/hostlink` reads is the
  container's: which ports forward is `forward_ports` in `[link]` of `config.toml`,
  never the agent's list; forwards listen on `127.0.0.1` only; a URL is
  `hostlink.CheckURL`'s (http(s), no credentials, printable) and asks
  first unless `open_urls` says otherwise; text shown goes through
  `proposal.Printable` and is cut short; AppleScript gets it as `argv`,
  never spliced into the script. A new request type needs the same:
  decided by the host's config, bounded, rate-limited -- what it logs
  too, since the agent decides how often (`egressLogf`). The outbound
  proxy dials only addresses it resolved and checked, and no name
  egress_allow allows reaches loopback, link-local or this machine's own
  addresses (`Egress.denied`). The link is a
  `docker exec`, never a socket of the host's mounted in, and a launch
  starts it detached (`startLink`) because the attach `exec`s docker and
  leaves no process to hold it. The agent's protocol has a `Version`: an
  image from another launcher is refused, not half-served.
- **Host exec is the host's to offer, and runs only what it was asked.**
  `host_exec` is `config.toml`'s, never the sandbox config's: the host's
  hello offers it, and a host that does not refuses the request whatever
  the agent says. What runs is the request's argv, through no shell, with
  no terminal, user or variables of the sandbox's, in the host directory
  of a container path under a root the container mounts
  (`config.HostPath`, with `mountedRoots`); the session that carries it
  takes three streams and the least window (`agentproto.Limits`), since
  the sandbox opens them. A command outlives neither its client nor the
  link: its process group is killed when the stream ends, and at the
  link's signal (`KillHostExecs`).
- **The file-change relay must not change what it touches.** Under
  gVisor the link relays the host's changes under the roots
  (`hostlink/relay.go`), and the agent raises an event for each by setting
  its mode to what it is (`agent/touch.go`). That touch reaches the host
  as a change too; the relay drops it only because type, mode, size and
  mtime are what it last sent the path with. A touch that moves any of
  them (a utimes to "now", a write) loops forever. Setting the mtime to
  its own value raises the better event, `IN_MODIFY`, but races a host
  edit landing between the stat and the set, and leaves new contents with
  an old mtime.
- **`sandbox/CLAUDE.md` is sandbox-wide.** It is read by every session in
  every project, so project-specific instructions — including everything in
  this file — do not belong there. Keep it to what is true in every project
  and short, since it loads into every session; a procedure goes in a skill
  (`sandbox/skills/<name>/SKILL.md`). It ships to every user, most with an
  installed release and no source: write for them, name no one's paths, and
  give no rebuild path unless `@@IF source=checkout@@`. The launcher
  expands placeholders (`@@CABOOSE_*@@`) and `@@IF`/`@@ELSE`/`@@END@@`
  blocks from the running sandbox's facts (`datadir.Facts`, built by
  `instructionFacts`): `internal/datadir/instructions.go` lists them. Maintainer
  notes belong in this file, not there.
- **caboose's instructions are Claude Code's managed CLAUDE.md, and
  `~/.claude/CLAUDE.md` is the user's.** A launch writes the expanded
  `sandbox/CLAUDE.md` and skills (managed skills, at
  `.claude/skills/<name>/SKILL.md`; stale ones pruned) to the data dir's `claude-code/`
  (`datadir.ManagedDir`), beside `home/` and in no other mount, which the
  sandbox has at `/etc/claude-code` (`datadir.ManagedTarget`), the one
  mount of the data dir that is read-only (`backend.Mount.ReadOnly`) under
  every isolation: docker's `:ro` under runc and gVisor, and under vm a
  read-only directory of the virtio-fs share (`vm.Share.ReadOnly`, so the
  host refuses the writes and the guest's root cannot remount it
  writable), bound read-only in the guest too. That protects the host's
  copy, and under docker and gVisor the mount itself; under vm the guest's
  root (CAP_SYS_ADMIN) can still unmount or cover the mount for the
  running VM, until the next boot -- never claim more. The Mac refuses only
  through a share no writable one overlaps (`planShares` refuses an
  overlap) and a `caboose-vmm` that knows `ReadOnly`: an older one ignores
  it, so starting a VM checks the vmm's version and notes a mismatch
  (`noteVMMVersion`), without refusing to boot. Never write anything of
  caboose's into `~/.claude/CLAUDE.md`. The image check refuses a base with
  anything at `/etc/claude-code`, which the mount would hide; a container
  without the mount is told to `caboose restart` (`instructionsDrift`).
