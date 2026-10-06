# Configuration

Each [environment](#environments) has a `config.toml` in its dir
(`~/.caboose/envs/default/config.toml` for the default one). `caboose setup`
writes one with every setting commented out, when there is none, and never
rewrites one that is there; `caboose status` names the file it read. Values
are TOML's own types (booleans, integers, arrays), and a key or table the
file does not know is an error naming the file and the key. What the sandbox
keeps of its home, and what of that syncs, is not here: it is the
[sandbox config](sandbox-config.md), the sandbox's own.

A setting that an older caboose read from a variable, or from another place
in the file, is refused where it is found, with a hint saying where it
lives now.

```toml
format = 1
isolation = "gvisor.default"       # which profile; needed only when several are defined

[roots]
projects = "~/dev/projects"        # /work/projects

[image]
auto_build = true

[session]
tz = "Europe/Lisbon"

[link]
open_urls = "ask"

[gvisor.default]
engine_socket = false
```

## Top level

| Key | Default | |
|---|---|---|
| `format` | `1` | the file's structure, which `caboose setup` writes; a file of a newer format than this caboose reads is refused, saying `caboose update` |
| `isolation` | the one profile defined, else `container` | the [isolation profile](#isolation-profiles) the sandbox runs under, `"<kind>.<name>"` |

## `[roots]`

The host directories the sandbox can reach; see [roots](#roots). Each entry
is `name = "host path"`, or a table with `host` and `path`.

## `[image]`

| Key | Default | |
|---|---|---|
| `base` | unset | [your own image](images.md) to build the sandbox on; unset, the environment's `image/Dockerfile` when it has one, else the embedded Dockerfile, built and tagged `caboose-base:<env>` |
| `auto_build` | `true` | build a missing image, or rebuild a stale one, when creating the container; `false` says to run `caboose build` |

## `[session]`

| Key | Default | |
|---|---|---|
| `tmux` | `true` | run sessions in tmux; `false` renders natively, but there is no detach or reattach |
| `tz` | host's zone | timezone inside the container |
| `hostname` | `caboose-<host>`, with `-<env>` after it in any environment but `default` | the sandbox's hostname: one lowercase DNS label (1 to 63 letters, digits and `-`, not starting or ending with `-`). The default's `<host>` is this machine's name, cut at the first `.`, lowercased, anything but letters, digits and `-` turned into `-`, and shortened to fit; on a Mac it is `scutil --get LocalHostName`, which unlike `hostname` does not change with the network. Fixed when the container is created, so `caboose restart` applies a change |
| `auto_sync` | `false` | [sync](sync.md) before a launch that finds nothing running in the container, once a sync remote is set |
| `keep_versions` | `2` | installed Claude Code versions to retain (~224MB each); at least 1 |
| `ready_timeout` | `600` | seconds to wait for first-run install; at least 1 |

## `[link]`

| Key | Default | |
|---|---|---|
| `forward_ports` | `["3000-3999", 5173, "8000-8999"]` | ports listening in the sandbox that are forwarded to the same port on this machine's localhost: integers from 1 to 65535 and `"a-b"` ranges; `[]` for none. See [the host link](host-link.md#ports) |
| `open_urls` | `"ask"` | whether the sandbox may open URLs in your browser: `"ask"`, `"allow"` or `"off"`; see [the host link](host-link.md#urls-and-notifications) |
| `ssh_agent` | the agent `ssh` here would use | the SSH agent socket the sandbox gets (`~` expanded), or `"none"`; for when `$SSH_AUTH_SOCK` is not your agent, as when a work login takes it over. Under `vm`, and `container` or `gvisor` on Linux; on a Mac, OrbStack and Docker Desktop forward the agent they were started with instead. See [SSH agent](ssh.md) |
| `host_exec` | `false` | let sessions run commands on this machine, as you, through the link (`caboose-agent host CMD`). **Sessions, and whatever steers them (a web page, a repo they work on), can then run anything on this machine as you**: meant for an environment whose point is a separate login or tools, not containment. See [the host link](host-link.md#host-commands) |

## Isolation profiles

`isolation` says what stands between the sandbox and this machine, as the
profile it picks. A profile is a table `[<kind>.<name>]`, where the kind is
one of:

- `container`, the default: the container runs under docker's own runtime,
  runc, and shares this machine's kernel (on a Mac, the engine's VM's).
- `gvisor`: the same container under [gVisor](https://gvisor.dev)'s
  `runsc`, a kernel of its own in user space, so a kernel bug the sandbox
  finds is gVisor's, not the host's.
- `vm`, on a Mac with Apple silicon: the sandbox is a VM of caboose's own,
  on Apple's Virtualization.framework, with no Docker at all; see
  [The vm isolation](#the-vm-isolation).

Each kind has its own keys, and a key another kind has is an error, so
`run_args` under a `vm` profile cannot be written:

| Kind | Key | Default | |
|---|---|---|---|
| `container`, `gvisor` | `run_args` | `[]` | more arguments for `docker run`, as the container is created; see [docker run arguments](#docker-run-arguments) |
| `container`, `gvisor` | `engine_socket` | `false` | mount this machine's engine socket (`DOCKER_HOST`'s unix socket, else `/var/run/docker.sock`): **removes the isolation**, see [caveats](caveats.md) |
| `vm` | `cpus` | half the CPUs | the VM's CPUs; at least 1 |
| `vm` | `memory` | half the memory, at most `8G` | the VM's memory, as `"8G"` or `"4096M"` |
| `vm` | `egress` | `true` | whether the sandbox's outbound connections are made from this machine, so a VPN's routes and DNS apply; `false` is the VM's own NAT. See [the host link](host-link.md#outbound-connections) |
| `vm` | `egress_ports` | `[22, 80, 443]` | the ports the sandbox may reach that way, written as `forward_ports` is; `[]` for none |
| `vm` | `egress_allow` | `[]` | private addresses it may reach anyway: names (`git.corp.example`), `*.suffix` patterns (`*.corp.example`, not `corp.example` itself), CIDRs or addresses. A name never lets through a loopback, unspecified or link-local address, nor one of this Mac's own: only an address or CIDR within that range does (`127.0.0.1`, `169.254.169.254`), and for the Mac's own, that very address |

A profile's name follows the rule for a root's: a lowercase label.
Profiles do not inherit from one another; define each one in full. Which
one runs:

- `isolation = "<kind>.<name>"` names it; naming one the file does not
  define is an error listing those it does.
- Without `isolation`, the only profile defined is used.
- With none defined, the sandbox is a `container` at its defaults.
- With several and no `isolation`, caboose refuses and lists them.
- A bare kind (`isolation = "gvisor"`) and `"docker"` are refused, with the
  spelling to use.

```toml
isolation = "vm.big"

[container.default]
run_args = ["--cap-add=NET_ADMIN"]

[vm.big]
cpus = 8
memory = "16G"
```

`caboose status` says which is in use and, when no profile is defined, that
it is the default.

## Other variables

Most of what a variable once set is in the file now. These remain:

| Variable | Default | |
|---|---|---|
| `CABOOSE_ENV` | `default` | the environment; `--env` wins over it |
| `CABOOSE_HOME` | `~/.caboose` | where an install lives: `versions/`, `vm/`, `runsc/`, `update.json` and the environments; moves the whole install |
| `CABOOSE_DATA_DIR` | `$CABOOSE_HOME/envs/<env>/data` | the data dir, named outright |
| `CABOOSE_SESSION` | unset | the name of the tmux session to attach to or create, in place of the search `<project>`, `<project>-2`, ... |
| `CABOOSE_NO_AUTO_UPDATE` | `false` | don't [update caboose](getting-started.md#updates) by itself; machine-wide, so no `config.toml` key |
| `CABOOSE_FORCE` | `false` | skip the question before a command ends running sessions (`restart`, `stop`, `prune`, `sync`); see [commands](commands.md) |

The two booleans take `true`, `false`, `1`, `0`, `yes`, `no`, `on` or `off`
in any case, and anything else is an error naming the variable. A variable
caboose no longer reads (`CABOOSE_ISOLATION`, `CABOOSE_DOCKER_RUN_ARGS`,
`CABOOSE_REPO_ROOT`, ...) is refused when set, with the place in
`config.toml` it moved to: unset it.

A container defaults to UTC, so the launcher detects the host's zone name
(from `$TZ`, else `/etc/localtime`) and applies it two ways: container-wide
at creation, and per-session via `tmux new-session -e` at attach. The second
is what makes a new session correct without a restart — a tmux session
inherits the *server's* environment, not the attaching client's, so a server
started before the zone was known would otherwise hand every new session
UTC. Sessions already running keep the zone they were created with.

The roots, the isolation profile and `keep_versions` are fixed when the
container is created (as bind mounts, labels and a `docker run -e`), so
changing any needs `caboose restart`. `caboose prune` always uses the
current `keep_versions`, and `caboose status` warns when the two disagree.

## docker run arguments

`run_args`, in a `container` or `gvisor` profile, adds arguments of your
own to the `docker run` that creates the container: a capability, a
device, a DNS server, a host entry, a memory limit.

```toml
[container.default]
run_args = ["--cap-add=NET_ADMIN", "--device=/dev/net/tun", "--dns=100.100.100.100"]
```

Like the roots, they are fixed when the container is created: `caboose restart` applies a change,
and until then a launch, `caboose status` and `caboose doctor` say the
container was created with others.

Each argument is one flag with its value, `--flag=value`. Only a few
boolean flags (`--privileged`, `--read-only`, ...) may stand alone, since
docker would take the next argument as another flag's value. Refused:

- short flags (`-e`): write the long one (`--env=...`);
- the flags caboose sets or depends on: `--name`, `--hostname` (use `hostname` in `[session]`),
  `--user`, `--runtime` (see [isolation profiles](#isolation-profiles)), `--entrypoint`,
  `--init`, `--restart`, `--rm`, `--detach`,
  `--interactive`, `--tty`, `--attach`, `--platform`, `--pull`,
  `--cidfile`, and `--env-file` and `--label-file`, whose contents caboose
  cannot check;
- `--env` for a variable caboose sets (`HOME`, `PATH`, `TZ`,
  `SSH_AUTH_SOCK`, `IS_SANDBOX`, anything `CABOOSE_` or `TINI_`), and `--label` for
  caboose's own (`dev.bfreis.caboose.*`);
- a `--volume`, `--mount` or `--tmpfs` at, inside or around one of
  caboose's mounts (the roots under `/work`, the kept parts of the home,
  Claude Code's install), or over the agent's home itself.

A refused argument stops the launch that would create the container, and
`caboose restart` before it removes the old one; `caboose doctor` names
it. Everything else goes to docker as written, and is yours to choose:
some arguments (`--privileged`, `--cap-add`, `--device`, `--volume`,
`--network=host`, ...) give the sandbox more of the host, which `caboose
doctor` notes. Only the host can set them: they are not in the sandbox's
own config, and a session cannot propose them.

## gVisor

A `gvisor` profile needs `runsc` registered with docker (in `docker info`'s
runtimes); a launch without it stops and says so, and `caboose doctor`
lists it as a problem. [`caboose setup isolation`](#setup) registers it
on OrbStack; elsewhere, install gVisor as [its guide](https://gvisor.dev/docs/user_guide/install/)
says (Docker Desktop is not supported yet), registering it with the flags
the sandbox needs, then reload docker:

```sh
sudo runsc install -- --host-uds=open --net-raw --allow-packet-socket-write
sudo systemctl reload docker
```

`--host-uds=open` lets the sandbox connect to the SSH agent caboose
forwards, a Unix socket of this machine's, which gVisor refuses
otherwise; the others let Docker run inside the sandbox. A runsc
registered without `--host-uds=open` still runs the sandbox, but
`caboose setup isolation` and `caboose doctor` say it cannot reach the
agent, and how to add the flag (to runsc's `runtimeArgs` in
`/etc/docker/daemon.json`; a running sandbox gets it at
`caboose restart`). Like the roots, the isolation is fixed when the container is
created: `caboose restart` applies a change, and until then a launch,
`caboose status` and `caboose doctor` say the container has another. A
container created under `gvisor` needs `runsc` for as long as it exists:
if docker loses it while the container is stopped, the next launch
recreates the container with the configured isolation (no session is
lost: it was stopped), or, when that cannot work either, says what to do. It is
set here only, never in the sandbox config: a session
cannot choose how it is isolated.

Under gVisor, the container runs as the agent user where that user can
write its mounts. Some engines (OrbStack, and probably Docker Desktop) show
mounted files as owned by whoever looks, which under gVisor is gVisor's own
process, so the agent could not write its own home. There the container
runs as root instead, a root that exists only inside gVisor's kernel; files
it writes on the mounts still belong to you on this machine. caboose finds
out which by trying, as it creates the container, and `caboose doctor`
says when it runs as root. Root's home is the agent's, `/home/agent`, there as
everywhere, so ssh and whatever else looks it up find what is kept.

OrbStack's file sharing answers a directory read again from the start,
through the same open handle, with what it said the first time, and
gVisor reads every directory that way. So caboose's `runsc` runs with
`--dcache=0`: gVisor then forgets a directory as soon as nothing uses it,
and a new directory listed while empty shows the files added to it later.
It costs a little on every path lookup (some 0.2s on an incremental
`go vet` of caboose itself). A directory some process keeps open, such as
a shell's working directory, can still list as it was: if a file seems
to be missing, `stat` it by name before taking it for gone.

gVisor turns none of this machine's edits under the roots into inotify
events inside, so a dev server or a watch-mode test in the sandbox would
not see a file you save in your editor. The [host link](host-link.md#file-changes)
makes up for it: under `gvisor` it watches the roots here and has the
agent raise an event inside for each file that changed.

### The vm isolation

Under a `vm` profile the sandbox is a Linux VM: a kernel of its own,
its image as a read-only disk, and a fresh scratch disk for what it writes,
so nothing it writes outside the mounts outlives `caboose restart`. It
needs no Docker engine, and runs these:

- `caboose-vmm`, next to `caboose`, which owns the VM. A release installs
  it there, signed with the virtualization entitlement; in a checkout,
  `make vmm` builds and signs it on the Mac (`make launcher` does too,
  there). `caboose setup isolation` and `caboose doctor` ask it
  (`caboose-vmm --check`) whether this Mac and its signature let it run
  a VM, and say what to do when they do not: macOS 13 or later, Apple
  silicon, and a caboose-vmm of the launcher's own version, signed.
- the guest's kernel (kernel.org's Linux 6.18 LTS, unpatched, built by
  caboose from `vm/kernel`: no modules, lockdown on) and the builder's disk,
  which `caboose build` builds the image in. A release build fetches its
  own version's the first time vm needs them (a few hundred MB, checked
  against the release's `checksums.txt`) into `~/.caboose/vm/<version>/`,
  and keeps only those of the versions installed. In a checkout,
  `make vm-kernel` and `make vm-builder` (with Docker, once each) put them
  in `vm-dist/`. Beside the kernel, `kernel-arm64.SOURCE` says where its
  source and config are.

The launcher looks for the last two in `~/.caboose/vm/<version>/arm64/`, then the checkout's `vm-dist/`. `caboose build` then builds the same image as
with a docker engine, in a builder VM of its own (the first build about a minute
and a half; one after a caboose update some fifteen seconds), and keeps
it in the data dir's `vm/images/` as a disk. A launch boots the sandbox in
about a second.

**Docker inside.** The VM is the sandbox, so it runs its own `dockerd`:
no socket of this machine's, no Docker-in-Docker. The default image has
it (its `dockerd` section, with buildx and compose, and a `sudo` section),
and the sandbox starts it when it boots; for [your own
image](images.md#requirements-for-your-own-image), `caboose check-image`
says whether it has one. Its images live on a disk of
their own, the data dir's `vm/volumes/docker.img`, which `caboose restart`
keeps, as a laptop's Docker keeps its images; to start afresh, `caboose
prune --docker` deletes it for an empty one, after saying what it frees
and asking (it stops a running VM first, listing its sessions). A start
also makes an empty one when the file is missing. Inner containers can bind-mount `/work` paths
as they are, and a port one publishes is forwarded to this machine like
any other port listening in the sandbox. `caboose status` says whether
dockerd is up, on its `dockerd` line (under container and gvisor its `docker`
line is about this machine's engine instead, whose socket the sandbox
may have); its log is `/var/log/caboose-dockerd.log` in the sandbox.

The sandbox runs as root inside the VM: its shares show every file as
root's, so the agent user could not tell its own. Root's home is
`/home/agent`, so ssh and the rest find what is kept there. Files it writes
on the mounts are yours on the Mac. The Mac's edits under the roots reach the
sandbox's watchers through the [host link](host-link.md#file-changes), and
so does your [SSH agent](ssh.md) (any command that starts the VM starts
the link too). The engine socket cannot be mounted into a VM, and `run_args` mean nothing there: a `vm` profile has no such key, and `cpus` and `memory` size the VM instead. `caboose logs`
shows the VM's console (`--tail N`). The data dir's path must be short
enough for the VM's socket (macOS allows 103 bytes); `caboose doctor`
says when it is not. Where caboose would say "container" -- `caboose
status`, `version`, `doctor`, `stop`, `restart` -- it says "VM".

## Roots

The sandbox mounts the host directories you name in `[roots]`, each at
`/work/<name>`. By default there is one, `dev = "~/dev"`, so `~/dev/you/project`
is `/work/dev/you/project` inside, and every project you run `caboose` in
has to be under a root; running it anywhere else is an error that says so.

`caboose setup roots` asks for them and writes them into `config.toml`;
the rest of this section is what it writes. Name each directory:

```toml
[roots]
projects = "~/dev/projects"   # /work/projects
work = "~/work"               # /work/work
```

A root that must sit at another container path (a script expects it
there) takes the long form, a table with `host` and `path`, or the same as
an inline table:

```toml
[roots.tools]
host = "~/src/tools"
path = "/opt/tools"

# or: tools = { host = "~/src/tools", path = "/opt/tools" }
```

The rules:

- A name is a lowercase label; `host` is absolute, or starts with `~`.
- Host directories cannot contain one another, and neither can container
  paths: no root's path is at, inside or around another's.
- `path = "/work"` puts a root at `/work` itself, and is allowed only for a
  sole root. Adding a second root moves it to `/work/<name>`: `caboose setup
  roots` says so, and asks first, since Claude Code keeps a project's
  memories under its path.
- A host path cannot hold `:` and a container path neither `:` nor `,`
  (docker's `-v SOURCE:TARGET` would misread them), and neither can hold a
  control character.
- A path is absolute and clean. It cannot be, or be inside or around, `/`,
  `/bin`, `/boot`, `/dev`, `/etc`, `/home` (the agent's home and caboose's
  own mounts), `/lib*`, `/proc`, `/root`, `/run`, `/sbin`, `/sys`, `/tmp`,
  `/usr` or `/var`. `/media`, `/mnt`, `/opt` and `/srv` themselves are
  refused too, but a path inside them is fine.

Use the same names and paths on every machine you [sync](sync.md), so a
project has the same path on each. The sync's default rules match only
projects under `/work`; for a root at a path outside it, add a rule to the
[sandbox config](sandbox-config.md).

Bind mounts are fixed when the container is created, so changing the roots
once a container exists needs `caboose restart`, which kills running
sessions (it asks first); until then a launch warns that they differ.

Everything under the roots is readable and **writable** from inside the
container. That is the point, but it is also the whole of what the sandbox
can reach on your disk, so choose them accordingly.

## Environments

An environment is a whole caboose of its own: its own data dir — and so
its own Claude login, memories and settings —, container, image and
config. Use one to keep a work subscription apart from a personal one, or
different roots, or a different image:

```sh
caboose -e work setup          # asks to create ~/.caboose/envs/work, then sets it up
caboose --env work             # or: CABOOSE_ENV=work caboose
caboose env                    # list them, the current one marked
```

The first launch in a new environment builds its image and creates its
container, `caboose-work`, and asks you to log in to Claude, as a first
run ever does. Sessions in different environments are in different
containers, so they do not see each other in the fleet roster. Any other
command in an environment that was never created is refused: a mistyped
`--env` would otherwise start a new, empty sandbox. The environment is
caboose's own flag, before the command: `caboose setup --env work` is
refused, with the spelling that works.

Without `--env` or `CABOOSE_ENV` you are in `default`. Names follow the
environment: the container is `caboose-<env>` (`caboose-default`), the
image `caboose:<env>`, and the base it is built on, when caboose builds
one, `caboose-base:<env>`.

## Setup

```sh
caboose setup                  # every section: roots, image, isolation, git, sync,
                               # and the container and the login along the way
caboose setup git              # only the git identity and signing
```

asks its questions on the terminal, each showing what is there now as
its default (Enter keeps it), and writes only what an answer changed, so
a re-run is safe and hand edits to `config.toml` and the sandbox's git
config survive. With no terminal it refuses rather than guess. A section
writes when its questions are done: stopping halfway (^C, ^D) leaves the
sections before it written and nothing of its own. A completed run leaves
a `config.toml` in the environment, which is what a launch and `caboose
doctor` take as set up.

Run for an environment that does not exist yet (`caboose -e work setup`),
it asks before creating it.

`config.toml` is edited line by line, as you would: only the keys setup
asks about change, a key's commented-out template line is uncommented with
the new value, in its table (`[session]`, `[vm.default]`, ...), and comments and other settings stay where they are. The
result is read back before it is written; if it would not say what you
answered (the file uses TOML the editor cannot follow), nothing is written
and setup shows the lines to change by hand.

- **roots** shows the [roots](#roots) and edits them: change
  one's directory, add another (each needs a name, and a path when
  it is not `/work/<name>`), or remove one. A change that moves projects to
  another path in the container — going from a sole root at `/work` to
  several moves `/work/x` to `/work/<name>/x` — is listed and asks first: Claude Code
  keeps each project's memories and history under its path, and will not
  find them under the old one (nothing is moved or deleted). Removing one
  of several keeps the other's name, and so its paths. A container that
  exists keeps the roots it was created with until `caboose restart`.
- **image** keeps what the sandbox is built on, or changes it: the
  Dockerfile embedded in caboose (the default when there is no `image/`),
  an [environment's own](images.md) `image/Dockerfile` written
  from caboose's preset or from the parts you choose, or, given up, the
  `base` in `[image]`. An `image/Dockerfile` that is there is
  kept by default; replacing it shows the difference and asks. After a
  change it offers to build; moving the container onto the new image is
  `caboose restart`, which it leaves to you.
- **isolation** asks for the [isolation](#isolation-profiles) and writes
  `isolation = "<kind>.<name>"`, the existing profile of that kind, else a
  new empty `[<kind>.default]`, even when it is the default: the strongest that works, `vm` when this Mac has
  [what it runs](#the-vm-isolation), else `gvisor` when docker has `runsc`
  and a container runs under it (tried with the image, when there is one),
  else `container`.
  Where docker has no `runsc` and the engine is OrbStack, it offers to
  download gVisor's latest release (`runsc` and what it runs beside it,
  some 150MB), checked against its published sha512, into
  `~/.caboose/runsc/<arch>/` (outside every mount, shared by every
  environment), then shows how `~/.orbstack/config/docker.json` would
  change and asks before writing it (the file as it was is kept as
  `docker.json.before-caboose`) and restarting OrbStack's Docker engine,
  which stops every running container. OrbStack's VM sees your home at
  the same path, so nothing is installed in it. Elsewhere it says how to
  get `runsc`, and when docker's lacks `--host-uds=open`, how to add it. When `docker.json` no longer lists the `runsc` the engine
  runs (so the engine's next restart would drop it), it says so and offers
  to put it right the same way. gVisor downloaded this way never updates by
  itself: each run checks it against the sha512 gVisor publishes for its
  latest release and, when that is newer (or the record of its release
  cannot be read), offers to download it in its
  place, checked the same way and swapped in whole. The engine needs no
  restart for it, but a sandbox already running under it goes on in the
  old release until `caboose restart`. `caboose doctor` notes when the
  release is over 60 days old.
- **git**: the identity and signing, [below](#git-identity-and-commit-signing).
- **the container**, in a whole run only, after the isolation: a running one
  is left alone, a stopped one started, and an absent one created when you
  say so (building the image first if there is none). git lists the
  forwarded agent's keys from it, and sync runs its git in it.
- **sync** asks for the [sync](sync.md) remote and, with
  one, for `auto_sync` in `[session]`. A new remote is set, and synced with right away,
  as `caboose sync --remote URL` does: that needs the container (it is
  started, never built) and no session running, and when it cannot run
  setup says why and what to run later, and goes on. A remote is not
  removed here; turning `auto_sync` off stops the automatic part.
- **the Claude login**, last in a whole run: whether the data dir has one
  (`.claude/.credentials.json`, only looked at, never read). Logging in is
  Claude Code's own first-run flow, which setup cannot drive from the
  host; with none yet, run from a project under a root, it offers to start
  a session there, which asks you to log in, and otherwise says how.

### Git identity and commit signing

`caboose setup git` asks for the sandbox's `user.name` and `user.email`
and how it signs commits, and writes them into its git config,
`<data>/home/.config/git/config` (`~/.config/git/config` inside). The
defaults are the sandbox's own values; where it has none, the host's
*default* identity is offered — an `[include]` is followed, an
`[includeIf]` is not, since the sandbox has one git config for every repo
and a per-repo override (a work address for work checkouts, say) must not
become it by accident. For signing it offers the key already set, the
host's SSH signing key, the keys the forwarded agent holds (listed when
the container is running), a pasted key or the path of a `.pub` file, or
none; then whether to sign every commit. See [SSH agent and commit signing](ssh.md)
for how signing works in the sandbox.

A launch never writes the identity, so a new environment never gets the
host's without a word. It says, in one line, while there is none, and
`caboose doctor` lists it as a problem. `git config --global` inside the
sandbox works as well as setup does.
