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
    make conformance ISOLATION=container|gvisor|vm [TRUST=hostile] [OUT=FILE]
                    # the security conformance suite (host only; destructive)
    make conformance-probe PROBE_DIR=DIR   # its static linux probe binaries

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

`sandbox/CLAUDE.md`, `sandbox/skills/` and `sandbox/mods/` are the exception: a launcher run
from a checkout expands them on every start into the data dir's
`claude-code/` (`CLAUDE.md`, `.claude/skills/` and `mods/`; the mods verbatim,
never expanded), which the sandbox has
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
| `entrypoint.sh` | container entrypoint; points root's home at `HOME`, and its shell at bash, when it runs as root (vm, some gVisor), bootstraps Claude Code, clears stale session state, prunes versions, runs the user's `start.d` in the background, idles under tini, and on a stop runs `claude daemon stop` (bounded, best effort) so background agents end "stopped", not "failed" |
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
| `internal/saferm` | the one way to remove a directory or file in code that walks paths (`RemoveAll`, `Remove`, `RemoveRoot`, and `RemoveTree`, which also clears the modes that would stop it, as an alias's fresh directory needs): refuses anything not under the root it is given once symlinks are resolved, an empty path, `/`, the home and top-level dirs; tests prove each refusal. A new `os.RemoveAll` of a directory goes through it |
| `internal/repocheck` | a check run by `go test ./...`: no `_test.go` names the sandbox's link socket (`/tmp/caboose-agent.sock`, `agent.SocketPath`), which in a sandbox is a live link to the host; `internal/agent`'s tests point its socket at a temp path (`TestMain`), and `agent.Main` refuses the real one in any test binary |
| `internal/nodocker` | keeps tests off the real docker: `Run`, called from `TestMain` in every package whose tests drive docker (`repocheck.DockerPackages`, which `go test ./...` holds them to), puts a docker first on `PATH` that records each call and fails it, clears `DOCKER_HOST` and `DOCKER_CONTEXT`, and fails the run if anything called it. Such tests fake docker themselves, on a `PATH` of their own or a `docker.CLI` pointed at a stub |
| `internal/nofollow` | file access under a directory the container can also write (the data dir's `home/`, the sync repo): never through a symlink or a hard link, in any component, every read and walk bounded; tested against swaps |
| `internal/statesync` | `caboose sync`: the sandbox config's rules applied to the sandbox's home and to what a remote sends, JSON merge by key, and the export/merge/apply cycle in the sync repo (the data dir's `sync/`, mounted at `~/.caboose-sync`), skipping keep entries the sandbox does not mount yet; it runs in the sandbox, files and git alike, and the host only holds the lock (`statesync.Lock`) and reads hints; tested against real git |
| `internal/syncagent` | the sync's run in the sandbox (`caboose-agent sync run\|status`: git's environment, the budget, askpass relaying git's and ssh's prompts, a lock of its own in the repo's `.git`, and stopping when the host's stdin closes) and its protocol with the host, newline-delimited JSON on the exec's stdin and stdout, every line bounded, and the prompts and conflicts put to the person (`MaxAsks`, `MaxConflicts`, none after an abort) (`Run` is the host's end, which `internal/launcher/sync.go` drives); tested with the test binary as the agent |
| `cmd/caboose-agent`, `internal/agent` | the sandbox's end of the host link, a static linux binary in the image: `caboose-agent link` (run by the host through `docker exec -i`) serves `open`, `notify`, `ports`, `host`, `run`, `aliases`, `checkpoint` (offered in the host's hello: `checkpoint.go`), `request` and `requests` (long polls ended by their client going away) to the sandbox on a Unix socket, counts the Claude Code sessions and their mods (`presence.go`), watches `/proc/net/tcp*` for listening ports, connects the host's streams to them, touches the paths the host says changed (`touch.go`), runs `caboose sync` in the sandbox (`sync`, `askpass`: `internal/syncagent`), and under vm serves the outbound proxy the host's hello offers, an HTTP proxy whose every connection the host dials (`egress.go`; `caboose-agent connect` for ssh, which a vm guest's agent points ssh at as it boots: `sshegress_linux.go`) |
| `internal/agentproto` | the link's protocol: streams multiplexed over one byte stream, with per-stream flow control, control messages on stream 0, and hard limits, since the host reads what the container writes |
| `internal/policy` | the pure decision: `Decide(policy, request, now)` for an alias, an outbound connection, a URL, host exec, a checkpoint asked for now (`checkpoint_requests` alone, either trust, never asks), deny rules first; `GrantKey`, `ExactKey` (what a once grant holds to: an alias's arguments, a connection's port, so "once" is the very request shown, while hours and always cover the whole alias or host), `Effect`, `AnswerEffect` (which says how far each answer reaches); the outbound proxy's address rules (`Denied`, `Allow`, `ParseAllow`), which `hostlink` aliases; no I/O, no clock |
| `internal/broker` | one environment's policy at work: builds the `policy.Policy` from `config.toml` and the grants, decides crossings, files what asks as requests (deduped by key, session and `ExactKey`, never merged into one that differs; bounded, expiring), turns answers into grants or `config.toml` edits; `grants.json`, `requests.json`, `decisions.log` and `broker.lock` in the env dir, through `nofollow`; serves only while its `Owner` holds link.lock (`LockOwner`). The link opens it as it starts (`internal/launcher/linkbroker.go`, with the URL dialog) and hands `hostlink` its `Link()` |
| `internal/uiport` | the host's web UI's fixed address, `127.0.0.1:7171` (a checkout's build: 7172), and `Is`, which every refusal that keeps a sandbox off those ports asks; imports nothing of caboose's |
| `internal/webui` | the web page as HTTP: the loopback-only listener (`Listen`, `CheckAddr`; at most 64 connections), the server's limits, the checks every request passes in order (Host, `Sec-Fetch-*`, then for `/api/`: POST, Origin, `Sec-Fetch-Site: same-origin`, JSON, the token header, the daemon's web allowlist, the body, the rate limits: authenticated calls, and proofs, which need no token), the headers every response carries (CSP, no framing, no CORS), `/api/proof`, and the static files from a fixed table (`static/`, embedded, in the `Makefile`'s `EMBEDDED`); it calls the daemon's method table through `Dispatcher` and imports nothing of the daemon's |
| `internal/menubar` | the menu-bar app's core, OS-independent: the `UI`, `Events` and `Daemon` interfaces between it and the glue, the menu tree and icon from the daemon's status, polling, the answer flow (every answer behind `UI.Confirm`), the held reflow, URL dialogs, the notification throttle and titles, the web page's routes, reconnecting, protocol skew and the spawn rules (`MayStart`, `SpawnExe`); tested with a fake daemon, UI and clock |
| `internal/macui` | the menu-bar app's AppKit glue through purego, no cgo and no `init`: it draws what the core says and posts back what the user did, deciding nothing. Every class and selector is a literal in one table (`selftest.go`), which `caboose app --selftest` checks on the Mac and a scan test holds to the code; what needs no AppKit (alert text, `approved`, the menu's flattening, the queues) is untagged and tested anywhere. The status item's images are PNGs in `icons/` (in the `Makefile`'s `EMBEDDED`), drawn from shapes in `icons_test.go`, which holds the files to the drawing (`-update` writes them) |
| `internal/appbundle`, `internal/launcher/app.go` | Caboose.app: its Info.plist (`Plist`, with `CabooseVersion`, the version whole; `ParseIdentifier`, `ParseString`, bounded), `Assemble` (built beside its place and renamed in, replacing only caboose's own bundle, removing through `saferm` within the parent), `IsAppLaunch` (the bundle's executable opened with no arguments runs `caboose app`) and `InCheckoutBundle` (a `make app` bundle is a checkout's build, `installed()`, whatever its stamp). The launcher wires the core to the glue as client kind `app` (`daemon.Config.NoReplace`, `appSpawnExe`, the relaunch), and has `__app-bundle`, which `make app` (a Mac only, signed ad hoc) runs: it assembles the bundle at its one fixed place, `appbundle.CheckoutPath` (`CABOOSE_HOME/app-dev/Caboose.app`), never in the checkout, and in no mount (`TestDaemonDirsAreInNoMount`); `__app-bundle --release` is the release job's, a release's bundle around a given binary. `caboose app` and turning on Open at login (`menubar.Options.LoginCheck`) refuse a place a sandbox reaches (`checkAppPlace`, `launcher/appplace.go`: `hostlink.AppPlaceOf`'s facts judged by `policy.CheckAppPlace` against every environment's roots, kept home, `run_args` mount sources and sandbox mounts and the daemon's writable set, by name and identity, nothing reached inside the bundle, no symlink on the way, an unknown reach refusing all) |
| `internal/hostlink` | the host's end: forwards what `forward_ports` allows to `127.0.0.1`, dials a vm guest's outbound connections as `egress_ports` and `egress_allow` say (`egress.go`), opens http(s) URLs as `open_urls` says, shows notifications, relays file changes under the roots into a gVisor container (`relay.go`), runs a session's `caboose-agent host` command here when `host_exec` says so, serving the exec port's protocol the other way round (`hostexec.go`), the aliases `config.toml` names (`alias.go`), the checkpoints the sandbox asks for, only of `[checkpoints] paths`, through a `Checkpointer` the daemon gives (its schedule's `TakeNow`, `launcher/daemoncheckpoints.go`) within the environment's `CheckpointLimits` (`checkpoint.go`), files and lists requests for the agent and waits on their decisions (`requests.go`), its decisions through one `Decider`, the environment's `broker.Decider` (`decide.go`; nil decides by the policy `config.toml`'s link settings make, and files nothing); the daemon runs one per environment as goroutines (`DaemonLinks`, `internal/launcher/daemonlinks.go`; the CLI side is `daemonclient.go`: `startLink` asks it, starting it on demand, and `caboose link`, `status` and `doctor` ask through `App.DaemonDial`'s seam), and a command run with `CABOOSE_DATA_DIR` set keeps a detached `caboose link --background` (`internal/launcher/link.go`), either way one per environment by a lock in the data dir |
| `internal/checkpoint`, `internal/launcher/checkpoint*.go`, `daemoncheckpoints.go`, `setup_checkpoints.go`, `doctor_checkpoints.go` | checkpoints of the sandbox's writable paths, kept on the host: the store (`EnvDir/checkpoints`, `Open`, keys and IDs, the lock, atomic publication by rename, leftovers), the walker (fd-relative, never following, linking unchanged files only from the previous checkpoint) and the macOS whole-tree clone, retention (`Retain`, `Prune`, the pins of `pins.go`), `Restore` (into a new directory, setuid bits cleared), and the `Scheduler` the daemon runs; the launcher resolves `[checkpoints] paths` to host sources (`checkpointSources`), guards the store by identity (`checkpointGuard`, `Store.CheckReach`), and has the CLI (`caboose checkpoint`), `setup checkpoints`, doctor's rows and status's line. The store is in no mount (`TestDaemonDirsAreInNoMount`) |
| `internal/conformance` (`checks/`, `fake/`, `matrix/`, `probe/`), `internal/launcher/conformance*.go`, `cmd/caboose/conformance_on.go`, `conformance_off.go` | the security conformance suite (`make conformance`, behind the `conformance` build tag, in no release): the envelope (`Guard`, `Runner`: a throwaway environment in its own `/tmp` base, audited and pinned before each check, destructive steps through the gate and last in the run, after every group's other steps, and none once one of those was an ERROR), the checks (`checks/`: PASS, FAIL, ERROR, GAP, SKIP, INFO), an in-memory `fake/` world with mutants that every check must fail, `matrix/` (renders `docs/security-matrix.json` into the table in `docs/security.md`: `conformance matrix`, checked by `verify-doc`), and `probe/`, the sandbox-side binary. The launcher side drives a real sandbox through `Backend`; `conformance leftovers` lists what an aborted run left |
| `internal/daemon` | the host daemon (`caboose daemon`, `internal/launcher/daemon.go`): one per daemon directory (`CABOOSE_HOME/daemon`, or `daemon-dev` for a checkout's build), its flock, a 0600 socket in a 0700 dir taking only its own user's connections (peer credentials), `daemon.json`, a capped `daemon.log` (`internal/cappedlog`, shared with `link.log`), the idle exit, and the client's start on demand under `start.lock` with a minimal environment; the protocol is `internal/daemonproto`; `Options.Brokers` serves the request broker's methods, and `Details`, `Proposals`, `Checkpoints`, `Writable` and `Jobs` (`sandbox_stop`, `sandbox_restart`) theirs. `methods.go` is the one table of methods, each with the client kinds that may call it, which the socket and the web page share. `Options.Web` turns on the web page (`web.go`, `webserve.go`): the token, made anew at every start and kept in memory only, never on disk (a `web` call with `rotate` replaces it, and starts the page when none could be had at start), served through `internal/webui` on its one address, tried again every 30 s while it cannot be had, never on another. Its directory is in no mount (`TestDaemonDirsAreInNoMount`) |
| `internal/launcher/daemonjobs.go`, `daemonjob_apply.go` | the daemon's jobs, which share `startJob` (one per environment at a time, `busy` otherwise, answered at once) and `endJob`. The stop and restart jobs (`StartJob`): the environment's own App running `Stop` or `Restart` with the session loss taken as confirmed (`lossConfirmed`) and every other question failing it (`noTerminal`: askYes and openTerminal), its notes in the env dir's `jobs.log` (`internal/cappedlog`), the link following the sandbox on the daemon's own links (`linkHook`, which `startLink` calls instead of dialling the daemon), the end through the daemon's Notifier, and `EnvStatus.job` for `daemonproto.JobKept` after; a job enters the links' `hostlink.ExecGate` as a host command does, so a `replace` waits for it and, once it holds or has closed the gate, starts none (`busy`). The apply job (`ProposalBuild`, `proposal_build`): a proposal with packages (`NeedsBuild`, alone or with a root) checked as `proposal_apply` checks one, under the proposal lock, before it starts; caboose apply's build (`buildPackages`, the lock deferred) before anything is written, then, under the lock, planned again on a fresh App and written in one `config.toml` edit only if its hash, the profile (`packagesChanged`) and the root's diff are as they were, the lock after; its output's last lines in `JobInfo.Lines` (`jobLines`), its notes in `JobInfo.Notes` |
| `internal/launcher/webui_cmd.go` | `caboose ui`: the page's address and a one-time code from the daemon (started if need be), opened only when the daemon serves it, else nothing opened and why; `--print`, `--new-token`; the `web` line of status and doctor's rows for the listener and the web ports |
| `internal/launcher/daemondetails.go`, `daemonproposals.go`, `apply_plan.go` | what the daemon's `Details` and `Proposals` are on the host: `link_log`, `env_details` (status's facts as data) and `doctor` (doctor's checks, run in the daemon's environment), the last two single-flight per environment; the proposals listed, deleted and applied by the hash of what was shown; `ProposalView`, a proposal planned as `caboose apply` would and said as data, with its hash (one part, or packages with a root, which `NeedsBuild` marks for `proposal_build`'s job, `daemonjob_apply.go`), and `ApplyPlanned`, which plans it again and writes only if that view still has the hash the page confirmed (and a root or an alias, its typed name), refusing what builds |
| `internal/launcher/brokerservice.go`, `requests.go`, `configwriter.go` | the broker on the host: `DaemonLinks` serves the daemon's `requests`, `request`, `answer`, `grants`, `revoke` and `permissions` from the running link's broker (`not_owner` when this daemon runs none for the environment), `caboose requests\|grants\|permissions` ask the daemon for them, and the config writer is the broker's `ConfigWriter` for an "always" answer: `config.EditFile` and `CheckEdit`, the diff shown, written only for the diff's hash |
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
| `.goreleaser.yaml` | release build: static darwin/linux × amd64/arm64 archives (darwin/arm64's with `caboose-vmm`; both darwin binaries signed by `macsign.sh` with `rcodesign`, which `release.yml` installs: with a Developer ID and notarized when the repository has the signing secrets, else `caboose-vmm` ad-hoc, CONTRIBUTING.md), `checksums.txt`, `install.sh` and the app's zips (made by `release.yml`'s `app` job on a Mac, only checked and attached here) on the GitHub release |
| `macsign.sh` | POSIX sh: signs a darwin binary with `rcodesign`, with a Developer ID (hardened runtime) and notarized when `MACSIGN_*` name the files (one submission, polled with retries until a verdict or `MACSIGN_NOTARY_TIMEOUT`; `macsign_test.go` stubs `rcodesign`), else ad-hoc with `--adhoc`; goreleaser's post-build hooks and `make vmm` run it |
| `install.sh` | the one-line install, POSIX sh: the latest release (or `CABOOSE_VERSION`), checked against `checksums.txt`, into the layout `internal/selfupdate` keeps, on a Mac the menu-bar app in `~/Applications` (`CABOOSE_NO_APP` skips it), then `caboose setup`; tested by `install_test.go` and `install_app_test.go` against a fake release host, a Mac's tools stubbed |
| `internal/selfupdate` | updating an install made by `install.sh`: the latest tag from the `releases/latest` redirect, download and checksum, `CABOOSE_HOME/versions/<tag>` and the `~/.local/bin/caboose` link, `update.json` and its lock; on a Mac `App` (`app.go`), the menu-bar app in `~/Applications`, replaced when older than the release (by its own `CabooseVersion`, so one left behind catches up at the next check); those two are the only things of an install outside `CABOOSE_HOME`; `releasetest` serves fake releases, with a fake ditto and codesign |
| `.github/workflows` | `ci.yml` (gofmt, vet, test, shellcheck); `release.yml` (goreleaser on a `v*` tag, after the vm assets and the app, built on a Mac) |
| `sandbox/CLAUDE.md`, `sandbox/skills/` | caboose's instructions to every session, a template, and its managed skills (`caboose-propose`, `caboose-persist`, `caboose-requests`, `caboose-troubleshoot`), expanded per launch (`internal/datadir/instructions.go`) and installed read-only under `/etc/claude-code` |
| `sandbox/mods/caboose` | caboose's Claude Code mod, a plugin folder (`.claude-plugin/plugin.json`, `hooks/hooks.json`, `hooks/register.ts`): the tools `caboose_request` and `caboose_requests` (they run `caboose-agent request` and `requests`) and the announcement of the host's decisions to the session that asked. Embedded with `all:` (the manifest sits in a dot directory, which `//go:embed` skips), read from a checkout through `nofollow` within the skills' limits, installed verbatim (no placeholders; a test refuses `@@`) into the managed dir's `mods/` and pruned from it like skills; Claude Code loads it from `/etc/claude-code/mods/caboose` through `CLAUDE_CODE_PLUGIN_DIRS`. `make go-test` never compiles `register.ts`: keep it small, and run it once under a real Claude Code after a change |
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
- **The mod is code the sandbox runs in Claude Code, and only informs.**
  `CLAUDE_CODE_PLUGIN_DIRS` (`datadir.PluginDirs`) is one entry in
  `backend.Spec.Env` under every isolation, labelled
  (`assets.LabelPluginDirs`, set even when empty; a container without it is
  told to `caboose restart`), and in `ownedEnv` of `run_args`'s refusals. A
  settings `env` that replaces it drops the mod, so nothing may depend on the
  mod: every decision is the host's, the mod only tells a session. Files
  under `mods/` are never expanded or rewritten; add one to `sandbox/mods`
  and `Makefile`'s `EMBEDDED` lists it by `find`. The link counts the
  sandbox's `claude` processes and those with a waiting mod (`/proc`,
  `internal/agent/presence.go`) and sends `TypeMods`; the count is the
  sandbox's word and feeds one line of `caboose permissions`, nothing else.
- **The conformance suite is host-only and destructive, and tests never
  run it for real.** It creates a sandbox and attacks it (writes, links,
  signals, mounts, removal as root), so `make conformance` refuses in the
  sandbox, and it exists only under the `conformance` tag: `./caboose`
  from `make launcher` has no `conformance` command, `./caboose-conformance`
  does. Everything runs through the `Guard`/`Runner` envelope, which
  verifies the base, the marker, the pins and the audited mounts before
  each check, and only a destructive command through `GatedSandbox`; a
  check never touches the person's home, roots, `CABOOSE_HOME` or a
  real engine. `internal/conformance` holds no removal and no secret
  string but where `detect.go` is allowed them (`scan_test.go`), and every removal goes through `saferm` with
  the base as root, under `t.TempDir()` in tests. Where a sandbox can
  write the place (a restore's copy in a mounted canary), the removal is
  `saferm.RemoveTreeAt` on a descriptor held below the base, every
  component opened `O_NOFOLLOW` and the last checked by device and inode
  (`Guard.DiscardRestore`): that counts as with the base as root, a
  path-based removal does not. A new check gets a row
  in `docs/security-matrix.json` (`conformance matrix` renders the table,
  `verify-doc` fails on one out of step or uncited) and a `fake/` mutant
  or fault its tests fail on; ids are cited in `docs/security.md`.
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
  What only some backends can tell is an optional interface beside it
  (`backend.Linker`, `backend.Starter` for env_details' start time, which
  the vm backend does not keep), asked by type assertion.
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
- **Roots are read-only but for their writable parts.** `rootMounts`
  (`internal/launcher/roots.go`) is the whole plan: each root read-only
  unless its `writable` is `["."]`, each part a writable mount at its own
  path under the root's, parents first (the vm guest binds in the order
  given, and `planShares` refuses a mount listed before one that holds
  it). It takes the roots and nothing else: no grant, answer or anything
  of the sandbox's may reach it. `planShares` refuses a read-only share
  that is or is inside a writable one, and allows one that holds a
  writable one; never loosen the first. The parts are checked for links
  and missing components at `ResolveRoots` and again just before
  `Create` (`config.CheckWritableParts`), when a sandbox being replaced is
  gone. The container is labelled with them (`assets.LabelWritable`, set
  even when empty, every root in it); `mountedRoots` reads it back, and a
  container without it, or a root it does not name, counts as wholly
  writable, failing closed. A change, or a missing label, is a restart
  (`writableDrift`), never `assets.Compat`.
  The gVisor relay watches only the writable parts: the agent's touch
  fails on a read-only mount. A root inside `CABOOSE_HOME` is refused at
  `ResolveRoots` by identity too, and so are roots that overlap by
  identity. `ResolveRoots` keeps the symlinks it met (`Root.Via`,
  `physicalVia`) and refuses a root reached through one in another root's
  writable part (`LinkInWritable`); just before `Create`,
  `checkRootReach` refuses a root that lies strictly inside, or is
  reached through a symlink that lies in, what another environment's
  sandbox can write (`otherWritable`: its configured writable parts,
  sandbox or not, and its sandbox's mounts when it has one), since that
  sandbox could swap it for a link to ~/.ssh before the mount is made. A
  root that is the very directory another environment mounts writable is
  shared and fine (`strictlyHolds`): a sandbox cannot replace its own
  mount point. An environment whose engine is down counts by its
  config.toml alone; one whose config.toml does not load refuses the
  launch, named. A proposed root is
  checked against the same set, and against the other roots by identity
  both ways. A root that holds `CABOOSE_HOME` is refused
  at `ResolveRoots`, in `setup roots` (`badRootPath`) and in a proposal
  (`config.CabooseHomeRootProblem`, by identity through `config.HoldsDir`:
  a `CABOOSE_HOME` not made yet by its nearest existing ancestor), naming
  a narrower root.
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
  session may propose is an allowlist -- a Dockerfile section, one root,
  one alias alone, one writable part alone (`internal/proposal`) -- and
  anything more is refused whole, never shown as a question. Never add a proposable part that
  widens what the sandbox reaches without a refusal list of its own, as a
  root has (`checkProposedRoot`: home, hidden dirs of it and `~/Library`,
  caboose's own state, overlaps; the home and caboose's state judged by
  identity through `config.HoldsDir` and `homeEntryOf`, and by name
  ignoring case; written as the physical path), and an alias
  has (`checkProposedAlias`, `launcher/apply_alias.go`: no shell or other
  program that runs its arguments, no command in a root, in what any
  sandbox can write, in the home's hidden dirs or caboose's state, judged
  as written, resolved and at each symlink on the way), and a writable
  part has (`checkProposedWritable`, `launcher/apply_writable.go`: the
  sandbox's path mapped through the roots it mounts, never its word for
  the host; a real directory under a configured root, no symlink in any
  component and physical as mapped; not the home or holding it, not in
  its hidden dirs or `~/Library`; not in, at or holding caboose's state
  by path and by identity; not writable already; confirmed by the root's
  name, written as the root's long form, in effect at the next
  restart). Everything a
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
  apply writes only blobs of mode 100644/100755. Every read there is
  bounded, since the sandbox decides a file's size (a sparse file costs it
  nothing): `nofollow` has only `ReadFileMax`, which refuses a file over its
  limit before reading it (`ErrTooLarge`), and `ReadDir` and `Walk` stop
  past `MaxEntries` entries or `MaxDepth` levels (`ErrTooMany`). Give a new
  read the smallest limit its file needs. The sync in the sandbox
  (`statesync.Syncer.InSandbox`) reads its own files whole: a bound there
  protects no host and would only stop the sync; `MaxFile` is the host's.
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
- **Who may call a daemon method is its method table's, and who answered
  is the caller's kind.** `internal/daemon/methods.go` is the one table
  the socket and the web page share: each method names the client kinds
  that may call it, and one it does not name is refused (`not_allowed`),
  so a new method reaches the page only once `TestMethodTableAllowlist`
  lists it. The kind `web` exists only inside the daemon (`callWeb`); the
  socket refuses a hello naming `web` or `daemon` before any request.
  `by` is derived from the kind (`broker.ByCLI`, `ByWeb`, `ByApp`), and
  `AnswerParams` has no field for it: keep it so. `app_attach` and
  `notifications` are the `app` kind's alone, and `notifications` only
  the attached connection's (one app at a time, `busy` for another); what
  an attachment records of the app (its PID and executable,
  `peercred_*.go`) is read off the socket as the connection is taken,
  never from params, and the page's `status` has no executable. The
  daemon's `Notifier` (`internal/daemon/notifier.go`) queues for an app
  holding `notifications` under a title from a fixed table
  (`daemonproto.HostTitles`, `SandboxTitle`: a sandbox's own title only
  ever in the body) and a route picked by the source, never the text;
  without one, or after 30 s without a poll, it shows what it was given
  as it always did. `status.attention` (`internal/daemon/attention.go`)
  is a cache the daemon's poll loop fills from `Options.Attention`'s
  sources (`launcher/daemonattention.go`): `status` never calls one, the
  engine-asking kinds (`image_stale`, `update`) run at most every 10
  minutes, and an item's text is host-written (anything the sandbox
  influenced is cut and made printable first). `dialogs` is the attached
  app's too, once it holds `dialogs`: the links' one URL dialog queue
  (`launcher/dialogqueue.go`, `daemon.Dialogs`) then shows nothing itself
  and hands the app the requests in the order filed, one in flight at a
  time, which the app answers with `answer` like anyone (`by` from its
  kind, the URL opened the stored one). Whoever shows a URL dialog, the
  app or the host's own, marks the request in flight in `requests.json`
  first (`broker.MarkInFlight`); one in flight when they go away is
  denied, "ask again", never asked another way (`DenyInFlight` when the
  attachment ends, `broker.Open` for a mark a broker that went away
  left). `request` carries the
  host's `offers` (once, hours, always) with their effects; an answer whose
  scope is not offered is refused. The "always" offer carries the literal
  diff of `config.toml` and its hash, and an "always" answer requires that
  hash (`ConfirmDiff`, `bad_request` without it) and writes only if the
  file would still change exactly so (`stale_diff` otherwise); the CLI
  shows the offer and asks a typed yes on a terminal before it answers.
  The writer edits line by line (`configwriter.go`), never a
  marshalled file. The daemon's broker calls are served by the link that
  holds the environment's `link.lock`; any other daemon, and a
  `CABOOSE_DATA_DIR` helper's environment, answers `not_owner`. The broker's
  files in the env dir (`grants.json`, `requests.json`, `decisions.log`,
  `broker.lock`) and the jobs' `jobs.log` beside them are in no mount
  (`TestDaemonDirsAreInNoMount`). `sandbox_stop` and `sandbox_restart`
  end every session, so they are the `cli` and `app` kinds' only, never
  the page's; their caller asked the user, and the job asks nothing
  (`launcher/daemonjobs.go`). `proposal_build`, the third job, ends no
  session and does what `caboose apply` does, so every kind may call it,
  as `proposal_apply`. Nothing locks a sandbox's lifecycle but
  the one job per environment: a terminal's restart beside a job races
  as two terminals' do. An App in the daemon never dials the daemon's own
  socket for its link (`linkHook`).
- **The web page's token is the daemon's, and its assets are inert.** The
  token, made anew at every start and held in the daemon's memory alone
  (no file: a token read from a page while the daemon was down must be
  dead when it is back), leaves the daemon only in `/api/redeem`'s answer
  to the page, for a one-time code: never in a URL, a log, an error, a status, any other response or
  the web allowlist. The socket's `web` result, which `caboose ui` and the
  app ask for, carries a code (`WebParams.Code`; `code_url`), never the
  token: 60 s, once, in the daemon's memory only (`webState.codes`, at
  most 16), all cleared by a rotation, every refusal one same answer
  (`bad_code`), and `web` is not a method the page may call. `caboose ui`
  opens an address only when the daemon says it serves it, since whatever
  else holds the port would be handed the code; it warns when an
  environment runs on the host's network. The page's files
  (`internal/webui/static/`, in the `Makefile`'s `EMBEDDED`) take text
  only as text nodes (no HTML sinks, no `innerHTML`), carry no inline
  script or style and name no external URL, and keep the token in the
  tab's `sessionStorage`, never `localStorage`, which whatever squats the
  port can read back; the CSP and the asset tests hold that, so keep them
  in step with any new file.
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
  root, and `caboose-vmm` beside it in darwin/arm64's), the app's zips
  (`caboose-app_<version>_darwin_<arch>.zip`, `Caboose.app` at the root,
  `release.yml`'s `app` job), vm's asset
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
  checks it (`VM_ASSETS_PREBUILT=1`). A repository may need arm64
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
- **Checkpoints stay on the host, out of the sandbox's reach.** The store
  is `EnvDir/checkpoints`, beside `data/` and never in a mount or a vm
  share (`TestDaemonDirsAreInNoMount` holds it out under every isolation);
  a root or a mount that holds it makes every take, prune and restore
  refuse, decided by device and inode, never by comparing path strings.
  A checkpoint never hard-links to a source (only to the previous
  checkpoint, after checking size, mtime, ctime and mode), so editing the
  live tree never changes one. A restore never links out of the store,
  writes only into a new directory (renamed into place without replacing
  anything), and clears the setuid, setgid and sticky bits in the copy. It
  refuses a target inside `CABOOSE_HOME` or any environment's writable set
  (`restoreTargetPolicy`), always: the daemon's `writable` answer plus
  what every environment's configured roots would mount
  (`configuredPaths`, sandbox or not), or the conservative set without the
  daemon; never turn this into a warning.
  Every removal under the store goes through `internal/saferm` with the
  store's physical path as its root. Retention (`DefaultRules`) always
  keeps a key's newest checkpoint and counts ages from it (from now when
  it is stamped later), so a stop never ages anything out; never measure
  from the clock alone. A sharp shrink pins the checkpoint before it in
  the store's `pins.json` (`Store.Pins`: written whole and renamed into
  place, read through `openRegular`, bounded), which every `Prune` honours
  and a pins file it cannot read makes it drop nothing; a pin goes only by
  `caboose checkpoint release` or a later checkpoint as large again in
  entries and bytes, and a take whose pin failed prunes nothing. Below the
  soft disk floor the schedule skips its takes (`SkipBelowSoft`,
  `ErrLowSpace`) and prunes nothing extra: space never shortens
  retention. `[checkpoints]` is `config.toml`'s,
  never the sandbox config's or a proposal's, and `setup checkpoints`
  edits it with `config.EditFile`, never rewriting the file.
- **The host link trusts nothing the agent sends.** `caboose-agent` runs
  in the sandbox, so everything `internal/hostlink` reads is the
  container's: which ports forward is `forward_ports` in `[link]` of `config.toml`,
  never the agent's list; forwards listen on `127.0.0.1` only; a URL is
  `hostlink.CheckURL`'s (http(s), no credentials, printable), and it, an
  outbound connection and a host command are decided by the broker's
  policy (`Decider`), with the facts the host resolved, never the agent's;
  an ask is never waited on (pending, with the request's number) and
  nothing the agent sends answers one or grants anything: the `Decider`
  has no such method, keep it so; text shown goes through
  `proposal.Printable` and is cut short; AppleScript gets it as `argv`,
  never spliced into the script. A new request type needs the same:
  decided by the host's config, bounded, rate-limited -- what it logs
  too, since the agent decides how often (`egressLogf`). The outbound
  proxy dials only addresses it resolved and checked, and no name
  egress_allow allows reaches loopback, link-local or this machine's own
  addresses (`policy.Decide`); a grant made at run time reaches a private
  address only if it holds that very address. The host's web UI ports
  (`internal/uiport`, 7171 and 7172) are out of the sandbox's reach on
  every path caboose offers, each checked where it is decided and none
  relying on another (a container or gVisor sandbox can still connect to
  the host's loopback through the engine, where the page's token is the
  boundary): `config.toml` refuses a `forward_ports` holding one (at
  load and in `CheckEdit`), the link never forwards one whatever its
  set, `CheckURL` refuses a URL to one whatever its host,
  `policy.Decide` denies one on any local address before every allow
  (`deny:ui_port`), and `checkRunArgs` refuses a `--publish` onto one.
  A new way out of the sandbox asks `uiport.Is` too. A wait for decisions holds
  up nothing else: its own goroutine, at most 16 a link. The link is a
  `docker exec`, never a socket of the host's mounted in, and a launch
  has the daemon start it (`startLink`: best effort, one note on failure,
  never a silent fall back to a helper) because the attach `exec`s docker
  and leaves no process to hold it. `status` and `doctor` only ask the
  daemon (`Connect`, never `Ensure`) and create nothing. The daemon's
  `replace` is frozen across protocols: an update asks the release daemon
  to exit once no host command runs (`ExecGate`, drained atomically, so
  none is admitted in between), and it starts the new version itself when
  a link ran; only a client of a newer protocol may force it (`Ensure`). The agent's protocol has a `Version`: an
  image from another launcher is refused, not half-served. An addition
  an older peer can ignore (a request offered in the hello, a message
  type, a field omitted when unset, a refusal's new `Reason`) keeps it;
  anything else raises it, and with it `assets.Compat`, since every
  running sandbox's link would be refused.
- **Host exec is the host's to offer, and runs only what it was asked.**
  `host_exec` is `config.toml`'s, never the sandbox config's: the host's
  hello offers it, and a host that does not refuses the request whatever
  the agent says; every request is decided by the policy first, which
  refuses it under `trust = "hostile"`, where the hello offers none
  either (`linkSettings.check`). What runs is the request's argv, through no shell, with
  no terminal, user or variables of the sandbox's, in the host directory
  of a container path under a root the container mounts
  (`config.HostPath`, with `mountedRoots`); the session that carries it
  takes three streams and the least window (`agentproto.Limits`), since
  the sandbox opens them. A command outlives neither its client nor the
  link: its process group is killed when the stream ends, when its link
  stops (each link tracks its own, `hostlink.Execs`, so stopping one of
  the daemon's links leaves another's commands be), and at the process's
  signal (`KillHostExecs`, every link's). It runs with exactly `PATH`,
  `HOME` and `LANG` (`hostlink.HostExecEnv`), never the rest of the host
  process's environment: the daemon spawns with a minimal environment and
  takes `PATH`, the SSH agent and docker's variables once from the user's
  login shell (`launcher/loginenv.go`), so a link behaves the same
  whichever terminal started the daemon. The daemon's links share one
  `hostlink.ExecGate`: a `replace` closes it at once and exits when the
  commands it found have ended, so new ones cannot hold an update off.
- **An alias runs what `config.toml` says, decided at every run.** The
  sandbox sends a name and `KEY=VALUE` values (`agentproto.ParseAliasArgv`),
  never a command; the host resolves `command[0]`, `cwd` and absolute
  arguments itself, component by component (`hostlink.ResolvePath`), with
  every symlink met and the device and inode of every path and directory
  above it (`policy.FileID`), and `Decide` refuses a command within or
  through what a sandbox can write by name and by identity (a Mac firmlink
  or another normalization of a name is the same directory). What runs is
  the resolved path decided, with the alias's argv, through host exec's
  machinery (`openExec`, `runCmd`: the same bound, environment, kill with
  the client), plus its timeout and the kill of what it leaves in its
  group (`killRemains`); its fresh directory is made under the daemon
  dir's `alias-tmp` and removed only through `saferm.RemoveTree` with that
  as the root. The writable set is the broker's (`Options.Writable`),
  read off every environment's sandbox mounts and labels
  (`launcher/writable.go`, `writableOf`), never off `config.toml`, again
  before an alias is decided once it is two seconds old; one it cannot read
  is everything. An alias is offered in the hello (`AliasesOn`), so it needs
  no new `agentproto.Version`.
- **Deny wins, and nothing the sandbox sends changes policy.**
  `policy.Decide` is the one decision for every crossing:
  its deny lines come before every allow, and no grant, allow entry,
  "always" answer or trust level turns one into an allow (its property
  tests hold that: keep them). It fails closed on what it does not know:
  a `trust` other than exactly `fallible` is decided as hostile, an
  unknown kind, an unresolved or unclean path, an empty address list, an
  odd `open_urls` or alias `policy` are denied. In the broker only
  `Answer` and expiry change a request's state, add a grant or edit
  `config.toml`; `Decide`, `File`, `List` and `Wait`, all the sandbox
  reaches, only file and dedupe, and a test drives them with random,
  hostile input to hold it. A writable set that cannot be read is `/`
  (every alias refused), and a broker that cannot be reached refuses.
  Everything the guard and the broker log, notify or show is bounded and
  rate-limited, since the sandbox decides how often it happens.
- **`trust` is `config.toml`'s, and `hostile` holds only in a VM with no
  NAT.** `trust` is a top-level key of the host's file, never the sandbox
  config's. Under `hostile` a launch refuses any isolation but `vm`, the
  VM is created with `Network: "none"` and the outbound proxy on
  (`vmNetwork`; an explicit `egress = false` is refused at load), and a
  launch refuses (and doctor says so) while `egress_allow` does not name every host
  in `claudeHosts` (`launcher/trust.go`, one list: never allow them
  implicitly, never add a broader pattern). The container is labelled
  `assets.LabelTrust` and `LabelNetwork`, set even when empty; an
  existing sandbox whose labels are missing or say otherwise is refused
  at attach with `caboose restart` (`checkTrustExisting`), never `Compat`.
  Under `hostile` with `ssh_agent` unset no SSH agent is forwarded
  (`agentFrom`, `agentFromHostile`): never fall back to an
  `IdentityAgent` or `$SSH_AUTH_SOCK` there.
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
- **The menu-bar app decides nothing in its glue, and never replaces a
  daemon.** Every decision is `internal/menubar`'s; `internal/macui` maps
  calls to AppKit and back (no `if` on the daemon's data beyond nil
  checks), every selector and class it sends is a literal in the
  self-check's table, AppKit runs on the main thread only (`main_darwin.go`
  locks main to it, for every command), and sandbox text is never a menu
  item's or a notification's title. The app-only methods are refused to
  every other client kind (the method table). The app reaches the daemon
  with `NoReplace`: it asks an older daemon to make way without force, and
  starts one from the installed CLI when that is not older than the app,
  and never after seeing a newer daemon (`MayStart`). The host runs the
  app, at every login once it is a login item, so it never runs from what
  a sandbox can write: `make app` writes it to a fixed place under
  `CABOOSE_HOME`, never the checkout, held out of every mount by
  `TestDaemonDirsAreInNoMount`, and the app refuses its own place at every
  start and before Open at login when a sandbox reaches it
  (`checkAppPlace`), failing closed. `make app`
  signs ad hoc, on a Mac only. A release's bundle has an identifier of
  its own (`appbundle.ReleaseID`, `dev.bfreis.caboose`: changing it loses
  the notification authorization and login item given to the old one),
  is built and signed by `release.yml`'s `app` job on a Mac (Apple's
  codesign, notarytool and stapler, never rcodesign), and lives at one
  fixed place, `appbundle.InstallPath` (`~/Applications/Caboose.app`),
  which `install.sh` fills and `selfupdate.App` replaces: only caboose's
  own release bundle, only one whose signature verifies, for that
  identifier, and (an update) by the installed one's team. A running app
  relaunches into a bundle renamed into its place (`Options.Replaced`,
  `Relaunch`: the executable it started from is no longer the file
  there), never while a question waits on the user, and only once its
  own process has ended (`relaunchScript`), or the daemon would turn the
  new one away as a second app.
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
