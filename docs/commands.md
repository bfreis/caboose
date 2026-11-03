# Commands

| | |
|---|---|
| `caboose` | start or attach the session for the current directory (run it *in* the repo): it starts in the directory [`[session.start]`](configuration.md#sessionstart) gives for that project, else in the path that mirrors it |
| `caboose claude [ARGS...]` | the same, passing ARGS to `claude` |
| `caboose setup [roots \| checkpoints \| image \| isolation \| git \| sync]...` | set up the environment, creating it first (it asks): the roots, which paths to take [checkpoints](checkpoints.md#choosing-the-paths) of, the image, the isolation, git and sync; see [Setup](configuration.md#setup) |
| `caboose apply` | review the changes sessions proposed (packages or a Dockerfile section for the image, a root, an alias, a writable part of a root, a few settings), and apply them; see [Proposals](proposals.md) |
| `caboose requests [--all]` | what the sandbox asked for and waits on: URLs to open, connections through the `vm` proxy, ports to forward, runs of an alias whose `policy` is `"ask"`; `requests show N`, `requests approve N [--for 8h \| --always]`, `requests deny N` answer them; see [Requests and grants](security.md#requests-and-grants) |
| `caboose ui [requests \| proposals \| ENV] [--print] [--new-token]` | open caboose's [web page](web-ui.md) in your browser, through the daemon; `--print` prints the address instead (it opens the page once, within 60 s), `--new-token` replaces the page's token first |
| `caboose app [--selftest]` | macOS: the [menu-bar app](menu-bar-app.md), which Caboose.app runs when opened (`install.sh` puts it in `~/Applications`, `make app` in `CABOOSE_HOME/app-dev`); `--selftest` checks the classes and selectors it uses and exits; it refuses to run from a place any sandbox reaches; elsewhere it says to use `caboose ui` |
| `caboose grants [revoke ID]` | the timed and once grants in force, with the time each has left; `revoke ID` ends one |
| `caboose permissions` | what this environment's sandbox may do now: trust, roots, aliases, host exec, URLs, outbound rules, the ports forwarded to your localhost, grants, how many Claude Code sessions run in it and how many lack caboose's mod, and, when the sandbox was created with other roots, writable parts or trust than `config.toml` now gives, that those take effect at `caboose -e NAME restart` |
| `caboose checkpoint list [PATH\|KEY] [--json]` | the [checkpoints](checkpoints.md) of the `[checkpoints]` paths: whether each path is copied now, and its checkpoints, newest first |
| `caboose checkpoint create [PATH]...` | a checkpoint now of each PATH, or of every path listed |
| `caboose checkpoint restore ID --to DIR` | copy a checkpoint into DIR, a new directory, made safe to open; see [Restoring](checkpoints.md#restoring) |
| `caboose checkpoint restore ID` | put a volume's checkpoint back as its disk, with the VM stopped; see [A volume's checkpoint](checkpoints.md#a-volumes-checkpoint) |
| `caboose checkpoint release ID` | unpin a checkpoint pinned after a sharp shrink; see [Releasing a pin](checkpoints.md#releasing-a-pin) |
| `caboose doctor [--offline]` | what, if anything, is wrong, and the command that fixes each problem; see [below](#when-something-is-wrong) |
| `caboose status` | the sandbox, version, the checkpoints (when paths are set: how many, how long since the newest, or what fails), platform, live sessions, disk use |
| `caboose version` | launcher version, and whether the image matches it |
| `caboose update` | update caboose to the latest release now; see [Updates](getting-started.md#updates) |
| `caboose build [--pull]` | build (or rebuild) the image: the base, checked, then the layer; see [What a build does](images.md#what-a-build-does) |
| `caboose check-image [IMAGE]` | whether an image can be the sandbox's base; see [Checking an image](images.md#checking-an-image) |
| `caboose restart` | recreate the sandbox, to pick up a rebuilt image or new mounts |
| `caboose stop` | stop the sandbox |
| `caboose detach` | detach every terminal from this project's sessions (only NAME's with `--session NAME`), leaving them running |
| `caboose link [--restart \| --foreground]` | the host's end of the link to the sandbox: forwards its ports, opens its URLs; the caboose daemon runs it, every launch asks it for one, `caboose link` asks now and `--restart` replaces it; `--foreground` runs the link in this terminal, to debug it (refused while the daemon serves the environment); see [The host link](host-link.md) |
| `caboose daemon [status\|stop\|restart]` | the host daemon that runs the links: how it is, stop it, or restart it; it starts on demand and exits when idle; see [The host link](host-link.md#the-daemon) |
| `caboose prune` | delete old Claude Code versions now, and cached packages no environment's lock names |
| `caboose prune --docker` | under a `vm` profile, delete the disk the sandbox's own `dockerd` keeps its images on, for an empty one (asks; `CABOOSE_FORCE=1` does not); see [The vm isolation](configuration.md#the-vm-isolation) |
| `caboose prune --builder-cache` | delete the builder guest's cache disk, every layer and download builds fetched; the next build makes an empty one (asks; `CABOOSE_FORCE=1` does not; refused while a build runs) |
| `caboose [-e ENV] disk [--json] [--budget DURATION]` | the disk space caboose takes up on this machine, per environment and per kind, and what frees each; see [Disk space](#disk-space) |
| `caboose volume create NAME PATH [--size SIZE] [--profile PROFILE]` | add a volume to a profile in `config.toml`; see [Volumes](configuration.md#adding-listing-and-removing-volumes) |
| `caboose volume ls [--json]` | every volume of the environment: the profiles declaring it, its disk's size and allocated space, whether the running VM has it; under `container` and `gvisor`, its docker volume and size |
| `caboose volume rm NAME [--profile PROFILE] [--delete-disk]` | remove a volume from a profile, keeping its disk (or docker volume) unless `--delete-disk` (asks) |
| `caboose volume resize NAME SIZE` | change a `vm` volume's size, while the VM runs too; see [Resizing a volume](configuration.md#resizing-a-volume) |
| `caboose sync [--remote URL]` | sync what the [sandbox config](sandbox-config.md) names (memories, settings, skills, ...) with your other machines; see [Syncing](sync.md) |
| `caboose sync status` | what a sync would send and take, changing nothing |
| `caboose sync add PATH`, `caboose sync rm PATH` | make a path of the sandbox's home sync, or stop it, in the sandbox config |
| `caboose logs` | the sandbox's supervisor log (under `vm`, the VM's console; `--tail N`) |
| `caboose shell` | a bash prompt inside the sandbox; `-c CMD` runs CMD, with a terminal only when caboose has one, so it works from a script |
| `caboose env [list]` | list the [environments](configuration.md#environments) |
| `caboose help [COMMAND]` | the commands and flags, or what one command does (also `--help`, `-h`) |

Arguments for claude go after `claude`, all of them, and nothing else
reaches it: `caboose claude -p "..."` runs a one-shot prompt in the
sandbox, `caboose claude --resume` picks a conversation to resume, and
`caboose claude --help` is claude's own help. They apply to a session that
starts then; attaching to one already running ignores them, with a note.
Anything caboose does not know — `caboose -p "..."`, `caboose statsu` — is
refused (exit 2) with what to type instead, rather than handed to claude.

caboose's own flags come first: `--env NAME` (`-e NAME`) picks the
environment, `--session NAME` the tmux session, `--help` (`-h`) shows the
commands, and `--version` the launcher's version. `caboose help COMMAND`,
or `caboose COMMAND --help`, says what one command does and takes; for
`shell`, `logs` and `build`, whose arguments go to bash or docker, only a
`--help` right after the command is caboose's.

`caboose build` passes extra args (`--no-cache`, `--progress=plain`, `-q`) to
every `docker build` it runs: a `dockerfile` profile's base, and the layer.
`--pull` is the exception, and means what fits the
[image profile](configuration.md#image-profiles): on `apko`, resolve the
packages again, to the newest the repository has, rewriting the lock; on
`dockerfile`, it goes to the base's build only; on `ref`, `docker pull` the
image first. `--platform` also goes to that pull and to the image check, so
the variant checked is the one built on; an `apko` base is built for the
engine's architecture, and refuses it. `-t`/`--tag` is refused: the images'
names (`caboose:<env>`, and the base caboose builds, `caboose-base:<env>`)
come from the environment. Only the layer's build writes to stdout, so
`caboose build -q` prints just the final image's ID.

`caboose prune` also clears the package cache `apko` builds share,
`~/.caboose/cache/apk`, on this machine: every cached package that no
environment's lock names goes, and every index but the newest of each
repository. Anything fetched in the last hour stays, since a build still
fetching has not written its lock yet, and while a build, or the host
link's check of a proposal, is using the cache, it leaves the cache
alone and says so ("a build is using the package cache; not pruned"),
pruning the Claude Code versions all the same. A lock it cannot read
could name anything, so then it removes nothing from the cache and says
which lock. It ends with one line: what it freed. Before the sandbox
starts, it deletes the scratch disks VMs that are not running left
behind (a VM makes its scratch disk at each start and deletes it at its
stop), skipping any VM starting, stopping or running. A volume's disk, or
under `container` and `gvisor` its docker volume, that no profile declares
is never pruned, since it holds what the sandbox wrote: `caboose prune`
names each one, with `caboose volume rm NAME --delete-disk`, which deletes
it.

`caboose restart` and `caboose stop` destroy every running session, so they list
the sessions they are about to kill and ask first. `CABOOSE_FORCE=1` (or `true`) skips the
prompt; without a tty they refuse rather than assume. `caboose prune
--docker` asks the same way, with or without sessions, since what it
deletes cannot come back, and lists those stopping a running VM ends;
`caboose prune --builder-cache` and `caboose volume rm --delete-disk` ask
the same way too. `--builder-cache` refuses while a build runs, since the
build holds the builder for as long as its guest runs (a second build
waits for the first), and stops a builder guest an earlier build left
running.
The daemon's stop and restart, for a client that asked you itself, ask
nothing more (see [the host link](host-link.md#the-daemon)).

Sessions are per project and per terminal: a second `caboose` in the same
repo while the first is attached gets `<project>-2`. `caboose --session
NAME` (or `CABOOSE_SESSION=NAME`) names one outright, attaching if it exists.

## Disk space

`caboose disk` lists what caboose keeps on this machine, for every
environment (or only the one `-e` or `CABOOSE_ENV` names, which must
exist) and for caboose's own files outside them, by kind: the VM's root
disks, the empty disk new disks are cloned from, scratch disks, volumes
(dockerd's disk among them), the builder's cache, checkpoints, the kept
home, the installed Claude Code versions, and the rest of each
directory; then caboose's versions, the package cache, the vm assets
(kernel and builder disk), runsc and Caboose.app. `--json` prints the
same as JSON. It reads no environment's `config.toml`, so a broken one
does not stop it. It only looks at each file's metadata (`fstatat`, and
`getattrlist` on macOS), never at what a file holds, and it changes
nothing. A directory a sandbox can write, such as the kept
home, is walked without following any symlink.

Each kind shows three sizes, which differ a great deal for what caboose
keeps:

- Apparent: the files' sizes, what `ls -l` shows. Every VM disk is a
  sparse file of its full size (a scratch disk is `scratch_size`, 64 GiB by default),
  most of it never written, so this is far more than the disk holds.
- Allocated: the blocks the files hold, what `du` counts. A sparse file's
  holes take none. On APFS, though, a clone (`cp -c`) shares every block
  with the file it was cloned from until either is written, and `du`
  counts those shared blocks once for each clone. Checkpoints are clones
  of each other and of the live files, and scratch disks and volumes are
  clones of the empty disk, so for them allocated is far more than
  deleting them would free.
- Unshared: the blocks no other file, clone or local snapshot holds,
  which deleting the files gives back. APFS on macOS reports it per file
  (its "private size", which leaves out blocks a clone or a snapshot,
  Time Machine's local ones included, still holds); elsewhere the column
  is `-`. Blocks that two of caboose's files share, say two checkpoints,
  but nothing else does are in neither's unshared size, yet deleting both
  frees them: the unshared total is what deleting everything would free
  at the least.

A file with several hard links counts once, and its unshared size only
once every one of its links has been met: APFS reports a hard-linked
file's whole size as its own, but deleting some of its links frees
nothing. One with a link outside what `caboose disk` looks at is counted
with no unshared size, and the report says how many there are.
Measuring the unshared size takes a call per file, tens of seconds on a
checkpoint store of a million files; past the budget, two minutes unless `--budget` says otherwise,
the remaining files are still counted but not measured, the report says
for how many files it measured, and a kind measured in part shows `≥`
before its unshared size. Checkpoints are measured last, so the budget
goes to the rest first.

`caboose doctor` has a `disk` row with the VM disks' allocated sizes,
from each disk file's metadata alone, and `caboose status` a `disk` line
with their total; neither walks the checkpoints. Doctor adds a line for
the builder's cache from 16 GiB, and for each leftover of 1 GiB or more:
the scratch disk of a VM that is not running, root disks no image or VM
uses, and a volume's disk no profile declares, each with the command
that deletes it.

What frees what:

| Kind | Freed by |
|---|---|
| root disks | the next `caboose build` deletes those no image record or VM uses |
| scratch disks | made at each start of a VM and deleted at its stop; `caboose prune` deletes those VMs not running left over |
| volumes | `caboose volume rm NAME --delete-disk` deletes a volume's disk; `caboose prune --docker` replaces dockerd's with an empty one |
| builder cache | only grows; `caboose prune --builder-cache` deletes it, and the next build makes it again, fetching everything anew |
| checkpoints | the [retention rules](checkpoints.md#what-is-kept-where-for-how-long); only the unshared size counts |
| Claude Code | `caboose prune`, beyond `keep_versions` |
| package cache | `caboose prune`, the packages no environment's lock names |
| caboose versions | an update keeps the newest two |

## When something is wrong

```sh
caboose doctor
```

goes through the whole environment and prints one row per check: the
configuration and roots, the data dir, the [sandbox config](sandbox-config.md)
(what it keeps, what of it was skipped, whether it is behind this caboose,
the roots it expects), the Docker engine, the image against this launcher
and its base, the container against the image, the roots and what it keeps,
the Claude login, Claude Code in it, live sessions, the SSH
agent, the daemon and the link,
the [checkpoints](checkpoints.md#when-something-is-wrong) (for an environment with `[checkpoints] paths`),
the sandbox's git identity and commit signing, and the sync. Each
row starts with a mark: `✓` when all is well, `!` for a note (worth
knowing; it clears up by itself, or is a choice), `✗` for a problem
(broken, or about to be, until you act), or `–` for not checked (and
why). Every problem ends up in a list at the bottom with the command
that fixes it, marked where that command ends running sessions. On a
terminal the report is in color and wrapped to its width, each row shows
up as its check finishes, and a spinner says what is being checked;
piped, the report comes whole at the end, each row one line for grep:
`MARK LABEL  TEXT`.

It changes nothing: it never builds, creates, starts or stops anything. A
container that is stopped or absent is a note, and what needs it running
is not checked. With a sync remote set it fetches, as a sync's first step
does, under the same prompt-free git and a 10s budget, to say whether the
remote has changes this machine has not taken; `--offline` skips that,
and what this machine has not sent is told without the network either
way. It exits 0 with no problems (notes allowed), 1 with any, and 2 when
it could not run.
