# Configuration

Each [environment](#environments) has a `config.toml` in its dir
(`~/.caboose/envs/default/config.toml` for the default one). `caboose setup`
writes one with every setting commented out, when there is none, and never
rewrites one that is there; `caboose status` names the file it read. Values
are TOML's own types (booleans, integers, arrays), and a key or table the
file does not know is an error naming the file and the key. What the sandbox
keeps of its home, and what of that syncs, is not here: it is the
[sandbox config](sandbox-config.md), the sandbox's own.

```toml
format = 1
isolation = "gvisor.default"       # which profile; needed only when several are defined
image = "apko.default"             # likewise, for the image

[roots]
projects = "~/dev/projects"        # /work/projects

[build]
auto_build = true

[session]
tz = "Europe/Lisbon"

[link]
open_urls = "ask"

[gvisor.default]
engine_socket = false

[apko.default]
packages = ["graphviz", "postgresql-17-client"]
```

## Top level

| Key | Default | |
|---|---|---|
| `format` | `1` | the file's structure, which `caboose setup` writes; a file saying any other format is refused (a newer one saying `caboose update`) |
| `trust` | `"fallible"` | how far the sandbox is trusted: `"fallible"` (an agent that may make mistakes) or `"hostile"` (assume something else steers it); any other value is an error. Under `"hostile"` the sandbox needs a `vm` isolation profile and has no network but the proxy, `host_exec` is ignored, every URL asks and every outbound connection `egress_allow` does not name asks. See [Trust](#trust) |
| `isolation` | the one profile defined, else `container` | the [isolation profile](#isolation-profiles) the sandbox runs under, `"<kind>.<name>"` |
| `image` | the one profile defined, else `apko.default` | the [image profile](#image-profiles) the sandbox is built from, `"<kind>.<name>"` |

## `[roots]`

The host directories the sandbox can reach; see [roots](#roots). Each entry
is `name = "host path"`, or a table with `host` and optionally `path` and
`writable`. Every root is read-only to the sandbox but for the parts its
`writable` lists; see [Read-only roots and writable parts](#read-only-roots-and-writable-parts).

## `[build]`

| Key | Default | |
|---|---|---|
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
| `forward_ports` | `["3000-3999", 5173, "8000-8999", "32768-60999"]` | ports listening in the sandbox that are forwarded to the same port on this machine's localhost, while they listen (the last range is where a login's loopback callback listens): integers from 1 to 65535 and `"a-b"` ranges; `[]` for none. See [the host link](host-link.md#ports) |
| `open_urls` | `"ask"` | whether the sandbox may open URLs in your browser: `"ask"`, `"allow"` or `"off"`; see [the host link](host-link.md#urls-and-notifications) |
| `ssh_agent` | the agent `ssh` here would use; none under `trust = "hostile"` | the SSH agent socket the sandbox gets (`~` expanded), or `"none"`; for when `$SSH_AUTH_SOCK` is not your agent, as when a work login takes it over, and the only way a `hostile` sandbox gets one. Under `vm`, and `container` or `gvisor` on Linux; on a Mac, OrbStack and Docker Desktop forward the agent they were started with instead of a socket named here, but `"none"` still gives none. See [SSH agent](ssh.md) |
| `open_urls_allow` | `[]` | origins the sandbox may open in your browser without asking when `open_urls` is `"ask"` (`"off"` still refuses every URL): `"https://host"` or `"http://host:port"`, lowercase, with nothing after the host or port (no path, not even a `/`). Ignored under `trust = "hostile"`, where every URL asks. See [Trust](#trust) |
| `checkpoint_requests` | `"allow"` | whether a session may ask for a checkpoint now of paths [`[checkpoints]`](#checkpoints) lists (`caboose-agent checkpoint`): `"allow"` or `"off"`; under either `trust`, since it copies nothing the schedule does not. At most one per path a minute and 10 requests an hour. See [Checkpoints](checkpoints.md#asked-for-by-the-sandbox) |
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
| `vm` | `egress` | `true` | whether the sandbox's outbound connections through the link's proxy (HTTP(S) through the proxy settings, ssh through its `ProxyCommand`) are made from this machine, so a VPN's routes and DNS apply. At `trust = "fallible"` the VM keeps its own NAT beside the proxy, and a program that ignores the proxy settings leaves through it, unrestricted; under `"hostile"` there is no NAT. `false` is the VM's own NAT alone. See [the host link](host-link.md#outbound-connections) |
| `vm` | `egress_ports` | `[22, 80, 443]` | the ports the sandbox may reach that way, written as `forward_ports` is; `[]` for none |
| `vm` | `egress_allow` | `[]` | private addresses it may reach anyway: names (`git.corp.example`), `*.suffix` patterns (`*.corp.example`, not `corp.example` itself), CIDRs or addresses. A name never lets through a loopback, unspecified or link-local address, nor one of this Mac's own: only an address or CIDR within that range does (`127.0.0.1`, `169.254.169.254`), and for the Mac's own, that very address |
| `vm` | `egress_deny` | `[]` | names, `*.suffix` patterns, CIDRs or addresses the sandbox may never reach, written as `egress_allow` is; a deny entry beats every allow entry, and every grant |

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

## Image profiles

`image` says what the sandbox is [built from](images.md), as the profile
it picks, the way `isolation` does. A profile is a table
`[<kind>.<name>]`, where the kind is one of:

- `apko`, the default: a base built from Wolfi packages with apko, no
  Dockerfile: caboose's package groups and your own packages. See
  [Packages](images.md#packages-the-default).
- `dockerfile`: a base built from a Dockerfile of yours, in the
  environment's `dockerfile/<name>/`. See
  [A Dockerfile](images.md#a-dockerfile).
- `ref`: an image of your own, used as it is. See
  [An image of your own](images.md#an-image-of-your-own).

Each kind has its own keys, and a key another kind has is an error:

| Kind | Key | Default | |
|---|---|---|---|
| `apko` | `packages` | `[]` | packages of your own, on top of caboose's: Wolfi package names |
| `apko` | `defaults` | `true` | whether caboose's default package groups come with them; `false` leaves only what the sandbox requires, and `packages` |
| `ref` | `image` | none, required | the image to build the sandbox on, as `docker pull` takes it; pulled when it is not local |

A `dockerfile` profile has no keys: its build context is always the
environment's `dockerfile/<name>/` (`~/.caboose/envs/default/dockerfile/default/`
for `[dockerfile.default]`), Dockerfile and all. Profile names and which
one is used follow the rules for isolation profiles, except that with
none defined it is `apko.default`, caboose's packages, and naming
`apko.default` needs no table.

```toml
image = "dockerfile.default"

[apko.default]
packages = ["jq", "graphviz"]

[dockerfile.default]

[ref.team]
image = "registry.example/team/image:tag"
```

An apko profile's packages are pinned in the environment's
`apko-<name>.lock.json`, next to `config.toml`; see
[The lock](images.md#the-lock).

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
in any case, and anything else is an error naming the variable.

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
  `SSH_AUTH_SOCK`, `IS_SANDBOX`, `CLAUDE_CODE_PLUGIN_DIRS`, anything `CABOOSE_` or
  `TINI_`), and `--label` for
  caboose's own (`dev.bfreis.caboose.*`);
- a `--volume`, `--mount` or `--tmpfs` at, inside or around one of
  caboose's mounts (the roots under `/work`, the kept parts of the home,
  Claude Code's install, caboose's instructions at `/etc/claude-code`), or
  over the agent's home itself.

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
`caboose status` and `caboose doctor` say the container has another. That
holds for a switch between two profiles of one kind too (say `vm.default`
to `vm.big`): the container records the profile it was created with. A
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
It costs a little on every path lookup. A directory some process keeps open, such as
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
with a docker engine, in a builder VM of its own (an `apko` base is built
here, on the Mac, and streamed into it; the first build takes a minute or
two, a later one much less), and keeps it in the data dir's `vm/images/`
as a disk. A launch boots the sandbox in seconds.

**Docker inside.** The VM is the sandbox, so it runs its own `dockerd`:
no socket of this machine's, no Docker-in-Docker. caboose's packages have
it (the `dockerd` group, with the CLI, buildx and compose, and `sudo`),
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

### Large repos: the Mac's file limit and slow git

This is about `vm`, with the roots shared over virtiofs.

**The file limit.** The Mac's Virtualization process holds one open file, so one pinned vnode, for every file and directory the guest kernel has cached from the shares, and releases it only when the guest drops it from its cache (`echo 2 > /proc/sys/vm/drop_caches`, in the sandbox, drops nearly all of them; walking the tree again pins them again). macOS's default `kern.maxvnodes` is a few hundred thousand, so a VM whose sessions walk a repo of several hundred thousand files, worktrees included, can hold most of it, and a second VM can run the Mac out: `Too many open files in system` for every process on it, so git and new processes fail.

The fix is a larger limit. `sudo sysctl kern.maxvnodes=1048576` applies at once and resets at reboot. `caboose setup isolation` offers to make it stick when `vm` is chosen and the limit is lower: it shows a LaunchDaemon, `/Library/LaunchDaemons/dev.caboose.maxvnodes.plist`, that runs that command at boot, and the `sudo` commands that install and load it, and runs them only after you say yes (sudo asks for your password). `caboose doctor` reports a lower limit under `vm`. Under `vm`, the [host link](host-link.md) checks about once a minute how many files the VM's Virtualization process holds open; past half of `kern.maxvnodes` it shows a notification, once per crossing, and again past 80%, and logs it in `link.log`. Meanwhile, `sync; echo 2 > /proc/sys/vm/drop_caches` in the sandbox lets the Mac release files at once, at the price of the guest re-reading what it needs again. Whether Docker Desktop's or OrbStack's file sharing pins the Mac's vnodes the same way is not covered here.

**Worktrees.** A worktree placed inside the repo's directory adds its whole tree to what a walk of the repo caches. Keep worktrees outside it (and outside the roots, if the sandbox need not see them).

**git on both sides.** The Mac and the VM see different inode, device, uid and gid numbers for the same file: only size and times match. With git's default `core.checkStat`, each side finds every index entry changed after the other wrote the index, re-reads and re-hashes every file (in the VM, over virtiofs, minutes on several hundred thousand files), and writes the index back with its own numbers. `git config core.checkStat minimal` in the repo (set it on the Mac; the repo's config applies on both sides) stops that: a run on one side after a write on the other no longer re-hashes anything, and the Mac keeps its fsmonitor speed. The cost: git no longer compares inode, device or owner, so an edit that keeps a file's size and lands in the same second as the previous change can be missed until something else touches the file.

**fsmonitor.** `core.fsmonitor` cannot work in the VM: git says `remote repository ... is incompatible with fsmonitor` and checks every file, which is slow when cold over virtiofs. Whatever the Mac's fsmonitor saves, the VM does not get.

**Claude Code's start.** It runs `git status` when a session starts and waits for it. On a large repo that can be minutes in the VM. `"includeGitInstructions": false` in the sandbox's `~/.claude/settings.json` (or `CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS=1`) skips it, at the price of the git instructions and git snapshot in Claude Code's system prompt. caboose does not set it.

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

A root that holds `CABOOSE_HOME` (`~/.caboose`), such as `~` itself, is
refused, as the roots are read, in `caboose setup roots` and in a proposed
root (`caboose apply`): a sandbox
could change caboose's configuration, data dirs and checkpoints through
it. The refusal names a narrower root, a directory under it that does not
hold `CABOOSE_HOME` (`~/dev` instead of `~`). It is judged by the
directories' identity on disk, so another name for the same directory is
refused too, and a `CABOOSE_HOME` not made yet by its nearest existing
parent.

A root that must sit at another container path (a script expects it
there), or that has writable parts (below), takes the long form: a table
with `host` (required) and optionally `path` and `writable`, or the same as
an inline table:

```toml
[roots.tools]
host = "~/src/tools"
path = "/opt/tools"

# or: tools = { host = "~/src/tools", path = "/opt/tools" }
```

### Read-only roots and writable parts

A root is **read-only** to the sandbox unless its long form lists the
parts it may write, as paths relative to the root. They are mounted
read-write over the read-only root at the same container path, so every
file keeps the path it has otherwise. A root with no `writable`, a short
form included, is read-only throughout: nothing in the sandbox can change
it. So is the default root, `dev = "~/dev"`, when `config.toml` has no
`[roots]`: `caboose setup roots` asks, for each root, which directories
under it the sandbox may write, and a launch says when it can write none.

```toml
[roots.dev]
host = "~/dev"
writable = ["you/project", "you/other"]   # /work/dev/you/project, ...
# path = "/work/dev"                          # as before: optional
```

- `writable = ["."]` makes the whole root writable; `"."` is then the only
  entry. `writable = []` is the same as leaving it out.
- Each entry is relative and clean (`a/b`, not `./a`, `a/`, `a//b`), with
  no `..`, no leading `/`, no control or invisible character, and no `:`,
  `,` or `\`; at most 64 per root; none twice, and none inside another
  (it would mean nothing). These are checked when the file is read.
- At every launch, and again just before the sandbox is created, every
  component of an entry, from the root down, must exist and be a real
  directory: a symbolic link anywhere on the way, a missing entry, or an
  entry that is not a directory is refused, naming the entry. The
  components above the last stay in the read-only part of the root, so
  the sandbox cannot swap one for a link afterwards.
- Writable parts are the host's setting: the sandbox cannot widen them,
  and no proposal can. Changing them takes `caboose restart`, like any
  change to the roots; until then a launch, and `caboose doctor`, say that
  the sandbox may write other parts than the configuration says. A
  sandbox created by a caboose from before read-only roots writes every
  root whole until that restart.
- Under gVisor and vm, the host's edits reach file watchers in the
  sandbox only in the writable parts (as attribute changes); in the
  read-only rest, a watcher needs its polling mode.

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
project has the same path on each. The sync's default rules match the
projects under the roots the [sandbox config](sandbox-config.md) lists,
wherever their paths are; a root added after it was written is added to its
`roots` too, and `caboose doctor` says when one is missing.

Bind mounts are fixed when the container is created, so changing the roots
once a container exists needs `caboose restart`, which kills running
sessions (it asks first); until then a launch warns that they differ.

Everything under the roots is readable from inside the container, and
the writable parts are writable too. That is the whole of what the sandbox
can reach on your disk, so choose them accordingly.

## Trust

`trust` says what the sandbox is assumed able to do:

- `"fallible"`, the default: an agent that may make mistakes.
- `"hostile"`: assume something else steers it.

It is the host's setting, in this file only. Under `"hostile"`:

- **a `vm` isolation profile is required.** A launch that would create the
  sandbox under `container` or `gvisor` refuses ("trust = hostile needs a
  vm isolation profile: ..."), and so does a launch that finds a sandbox
  created under one (set `isolation` to a `vm.*` profile, or `trust =
  "fallible"`, then `caboose restart`). An existing sandbox is trusted as
  hostile only if it records a `vm` isolation and a VM created with no
  network; one made before that, or at `fallible`, is refused until
  `caboose restart` recreates it;
- **the VM has no network of its own.** It is created with its NAT off, so
  the only way out is the outbound proxy: HTTP(S) through the proxy
  settings and ssh through its `ProxyCommand`. A program that ignores
  those reaches nothing, and the containers the VM's `dockerd` runs reach
  nothing, since they do not use the proxy. Changing `trust` changes how
  the VM is created, so it takes `caboose restart`, which a launch says;
- **`egress` cannot be off**: `egress = false` in the `vm` profile in use
  is an error, and the proxy is on whatever else the profile says;
- **every outbound connection `egress_allow` does not name asks**: it is
  refused, as pending, until a person allows it (`caboose requests`);
- **Claude Code's own hosts must be in `egress_allow`.** They are not
  allowed implicitly; a launch refuses, naming the ones missing and the
  line to write, until each is named by `egress_allow` (a name, or a
  `*.` pattern that covers it) and none by `egress_deny`. Only this file
  counts, not a grant given at run time. `egress_ports` must allow 443,
  which they use. The hosts: `api.anthropic.com` (the API), `claude.ai`
  (the installer and the login), `platform.claude.com` (the login's token
  exchange) and `downloads.claude.ai` (the release binaries). The list
  covers starting and logging in, and may be incomplete for optional
  features (telemetry, an MCP proxy): add what a feature needs to
  `egress_allow`, or approve its request when it asks. For example:

      [vm.default]
      egress_allow = ["api.anthropic.com", "claude.ai", "platform.claude.com", "downloads.claude.ai"]

- **`host_exec` is ignored**: the link does not offer host commands and
  refuses a request for one, whatever `host_exec` says; `caboose doctor`
  notes it;
- **every URL asks**: `open_urls = "allow"` and `open_urls_allow` are
  ignored, and `"off"` still refuses;
- **an alias whose `cwd`, command or an absolute argument is in a writable
  path is refused** (at `fallible`, only the command and its path are).

The SSH agent: `trust = "hostile"` forwards no SSH agent unless `[link]
ssh_agent` names one. The one `ssh` on this machine would use by default
(an `IdentityAgent` in `~/.ssh/config`, or `$SSH_AUTH_SOCK`) is never
carried, and `caboose doctor` says so. A socket `ssh_agent` names is
carried into the VM as at `fallible`, and the sandbox can sign and log in
wherever its keys reach, for as long as it runs; its ssh connections
still go through the proxy, and so are decided by `egress_allow` and
`egress_ports`.

`caboose doctor` and `caboose permissions` show the level first.

## `[checkpoints]`

The paths whose files caboose's daemon copies, on this machine, into
`~/.caboose/envs/<env>/checkpoints/`, which no sandbox mounts:

- while the sandbox runs, a checkpoint of each path every 10 minutes, the
  first as soon as the daemon's link to the sandbox starts, and one more
  when the sandbox stops;
- they are kept every 10 minutes for 2 hours, then the newest of each hour
  for 48 hours, then the newest of each day for 7 days, and pruned once an
  hour while the daemon runs (it stays while a sandbox runs, and leaves a
  few minutes after the last stops), and again when it starts. Ages count
  from the path's newest checkpoint, which is always kept, not from the
  clock: an environment left stopped for a month keeps what it kept when
  it stopped. A key whose path is no longer listed is pruned by the same
  rule;
- when a checkpoint holds half or less of the entries, or of the bytes,
  of the one before it, and that one held more than 1,000 entries or
  100 MiB, the one before it is pinned: kept whatever its age, until a
  later checkpoint holds as many entries and bytes again, or `caboose
  checkpoint release ID`;
- one checkpoint is taken at a time, across all environments; one that
  fails is tried again 10 minutes later;
- with less free than the larger of 10 GiB and 5% of the disk holding
  the checkpoints, the daemon skips them (`caboose checkpoint create`
  still takes one), and prunes none early for it; with less than the
  larger of 1 GiB and 1%, none is taken at all;
- you get a notification when a checkpoint fails, when a path stops
  standing for a directory, when the disk holding the checkpoints runs
  low, when a sandbox could reach the store (then none is taken), and
  when a checkpoint shrinks sharply as above (it names the earlier one,
  pinned, to restore from). Each at most once an hour.

They are taken by the daemon, only for an environment at its default data
dir: a command run with `CABOOSE_DATA_DIR` set gets none.
`caboose setup checkpoints` chooses the paths, `caboose checkpoint list`
shows the checkpoints, `caboose checkpoint create` takes one now, `caboose checkpoint release ID` unpins one, and `caboose checkpoint restore ID --to DIR` copies one into a new
directory, refusing one inside `~/.caboose` or one any environment's
sandbox can write, or would at its next start ([Checkpoints](checkpoints.md)). `caboose status` and `caboose
doctor` report on them ([when something is
wrong](checkpoints.md#when-something-is-wrong)); deleting `~/.caboose/envs/<env>/`
deletes them.

```toml
[checkpoints]
paths = ["/work/dev/myproject", "~/.claude"]
```

| Key | Default | |
|---|---|---|
| `paths` | none | container paths, at most 32, none at or inside another |

Each path is written as the sandbox sees it, absolute and clean, or under
the sandbox's home as `~/...` (`/home/agent/...` is read as the same). The
whole home, `~` alone, is refused: name the kept paths in it. A path that
is not of this form is an error naming it.

Which host directory a path stands for is worked out each time, from the
roots, the running sandbox and the sandbox config of the time; a path
that stands for none is skipped, with the reason, and never stops the
others:

- a path under a root is copied when it is, or is inside, a writable part
  of the root, either as `config.toml` says or as the running sandbox has
  it mounted (a sandbox created by a caboose that recorded no writable
  parts counts as writing all of its roots). So a part you have made
  read-only is still copied until the restart that mounts it so. The copy
  is made from the root's directory on this machine;
- a path under the home, `~/REL`, is copied when `REL` is, or is inside, a
  path the sandbox keeps (a `[[keep]]` entry of its
  `~/.config/caboose/sandbox.toml`, or one caboose itself keeps, such as
  `~/.claude`), or one the running sandbox has mounted. The copy is always
  made from the environment's data dir, `data/home/REL`: the sandbox's
  config only says which of those paths qualify, never a path on this
  machine. Claude Code's login, `~/.claude/.credentials.json`, is never
  copied;
- a path that is missing, or runs through a symbolic link, is skipped:
  nothing is followed;
- a path that is, holds or is inside a checkpoint store or the daemon's
  directory, of any environment, is refused, whatever root reaches it.

## `[alias.NAME]`

An alias is a command on this machine that a session may run with
`caboose-agent run NAME [KEY=VALUE...]`: a fixed argv plus enum
parameters, never a shell and never free-form arguments. There are none by
default, and only this file defines them (a session can propose one,
which `caboose apply` shows you: [proposals](proposals.md)). `NAME` is a
lowercase label. The link reads a change within seconds, with no restart.

```toml
[alias.caffeinate]
command = ["/usr/bin/caffeinate", "-i"]
description = "keep the Mac awake while a turn runs"

[alias.test]
command = ["/usr/local/bin/runner", "--suite", "{suite}"]
timeout = "10m"
policy = "ask"

[alias.test.params]
suite = ["unit", "all"]
```

| Key | Default | |
|---|---|---|
| `command` | required | the argv: `command[0]` an absolute, clean path to the program, then its arguments. An element that is exactly `{KEY}` for a key of `params` is replaced by the value given for it (the first of its values when none is); there is no other substitution |
| `description` | `""` | what the sandbox is told of it (never the command); at most 200 bytes of printable text |
| `timeout` | none | a Go duration (`"30s"`, `"10m"`); `"0"` is none. At its end the command is killed |
| `cwd` | a fresh empty directory | the host directory it runs in, absolute or starting with `~/` |
| `policy` | `"allow"` | `"allow"` runs it when asked; `"ask"` refuses it as pending until you allow the request it files (`caboose requests`); `"deny"` refuses it |
| `params` | none | a sub-table `[alias.NAME.params]` of parameters, each an array of 1 to 32 distinct values; a parameter's name is a lowercase label and a value is 1 to 64 of letters, digits, `.`, `_` and `-` |

In the sandbox, `caboose-agent aliases` lists the names, descriptions,
parameters and whether each asks first, never a command or a directory;
`caboose-agent run test suite=unit` runs one. A key the alias does not
declare, or a value not in its list, is refused. Its stdin, stdout and
stderr are the sandbox command's, and its exit status is
`caboose-agent run`'s; when it did not run, 75 means it waits for your
answer (`pending #N`, which `caboose requests approve N` answers), 77 that
it was refused, 127 that there is no such alias, and 126 that it could not
run here (its command missing, say).

What runs, every time:

- The command is looked up, and every symlink in its path followed, at
  each run; it is refused when it, or any symlink on the way, is within
  anything a sandbox of any environment can write (its writable parts, its
  home, ...), whatever `trust` says: the sandbox could change what it
  runs. Paths are compared by their device and inode too, so another
  spelling of a writable directory (a Mac firmlink, another Unicode
  normalization of a name) is the same directory. Under
  `trust = "hostile"` an alias whose `cwd` or an absolute argument is
  within a writable path is refused as well.
- The program runs as you, with exactly `PATH`, `HOME` and `LANG` (as host
  commands get them: [host link](host-link.md#host-commands)), no terminal,
  and no shell: `*`, `|` and `$(...)` in an element are passed as they are.
- With no `cwd`, it runs in a fresh empty directory under caboose's daemon
  directory (`~/.caboose/daemon/alias-tmp/`, or `daemon-dev/` for a build
  from a checkout), removed once it is done.
- It lives only while `caboose-agent run` does: interrupting that, or
  anything that ends it, ends the command and whatever it started (its
  process group, `SIGTERM`, then `SIGKILL` three seconds later), as does
  its `timeout` and the link's own end. What it leaves running when it
  exits is ended too; a process that leaves its group (a daemon that calls
  `setsid`) escapes that. Aliases and host commands together run at most 16
  at once.

Some examples, per host:

```toml
# macOS: keep the Mac awake while the alias runs
[alias.awake]
command = ["/usr/bin/caffeinate", "-i"]
description = "keep the Mac awake until interrupted"

# Linux (systemd): keep the machine from idling
[alias.awake]
command = ["/usr/bin/systemd-inhibit", "--what=idle", "/bin/sleep", "infinity"]
description = "keep the machine from idling until interrupted"
```

`caboose-agent run awake &` in a session holds the machine awake until that
job ends. A command that runs what a writable project says (`make` in a
`cwd` the sandbox can write) gives the sandbox a way to run code on your
machine: allowed under `fallible`, as your choice, and refused under
`hostile`.

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
caboose setup                  # every section: roots, checkpoints, image, isolation, git, sync,
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
  of several keeps the other's name, and so its paths. Then, for each
  root, it asks which directories under it the sandbox may write: paths
  relative to the root, separated by spaces, `.` for all of it, empty for
  none (when the root has some already, Enter keeps them and `-` is none),
  asked again until each is a real directory under the root. A path with a
  space in it is written into `config.toml` by hand. A container that
  exists keeps the roots it was created with, and what of them it may
  write, until `caboose restart`.
- **image** keeps what the sandbox is built from, or changes it, as an
  [image profile](#image-profiles): caboose's packages (the default),
  package groups you choose one by one (an `apko` profile with
  `defaults = false` and the groups' packages listed), a Dockerfile you
  edit, or an image of your own, which it asks for. The profile in use
  keeps its name when you pick its kind again; another kind is written as
  `<kind>.default`. Your own packages in an `apko` profile stay through
  either apko choice. Choosing the Dockerfile writes caboose's seed into
  the profile's `dockerfile/<name>/` when there is no Dockerfile there; one
  that is there is yours, and stays as it is. After a change it offers to
  build; moving the container onto the new image is `caboose restart`,
  which it leaves to you.
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
