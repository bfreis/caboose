# Commands

| | |
|---|---|
| `caboose` | start or attach the session for the current directory (run it *in* the repo) |
| `caboose claude [ARGS...]` | the same, passing ARGS to `claude` |
| `caboose setup [roots \| image \| isolation \| git \| sync]...` | set up the environment, creating it first (it asks); see [Setup](configuration.md#setup) |
| `caboose apply` | review the changes sessions proposed (a tool in the image, a directory to keep, a root), and apply them; see [Proposals](proposals.md) |
| `caboose doctor [--offline]` | what, if anything, is wrong, and the command that fixes each problem; see [below](#when-something-is-wrong) |
| `caboose status` | the sandbox, version, platform, live sessions, disk use |
| `caboose version` | launcher version, and whether the image matches it |
| `caboose update` | update caboose to the latest release now; see [Updates](getting-started.md#updates) |
| `caboose build` | build (or rebuild) the image: the base, checked, then the layer |
| `caboose check-image [IMAGE]` | whether an image can be the sandbox's base; see [Checking an image](images.md#checking-an-image) |
| `caboose restart` | recreate the sandbox, to pick up a rebuilt image or new mounts |
| `caboose stop` | stop the sandbox |
| `caboose detach` | detach this project's session, leaving it running |
| `caboose link [--restart]` | the host's end of the link to the sandbox: forwards its ports, opens its URLs; every launch starts one in the background, and `--restart` replaces it; see [The host link](host-link.md) |
| `caboose prune` | delete old Claude Code versions now |
| `caboose prune --docker` | under `vm`, delete the disk the sandbox's own `dockerd` keeps its images on, for an empty one (asks; `FORCE=1` does not); see [The vm isolation](configuration.md#the-vm-isolation) |
| `caboose sync [--remote URL]` | sync what the [sandbox config](sandbox-config.md) names (memories, settings, skills, ...) with your other machines; see [Syncing](sync.md) |
| `caboose sync status` | what a sync would send and take, changing nothing |
| `caboose sync add PATH`, `caboose sync rm PATH` | make a path of the sandbox's home sync, or stop it, in the sandbox config |
| `caboose sandbox-config update` | bring the sandbox config up to this caboose: its format, and the defaults added since it was written |
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
both `docker build`s, the base's and the layer's, with one exception:
`--pull` goes to the default base's build only, and on `CABOOSE_BASE_IMAGE`
means `docker pull` it first. `--platform` also goes to that pull and to the
image check, so the variant checked is the one built on. `-t`/`--tag` is
refused: the images' names come from `CABOOSE_IMAGE`. Only the layer's
build writes to stdout, so `caboose build -q` prints just the final
image's ID.

`caboose restart` and `caboose stop` destroy every running session, so they list
the sessions they are about to kill and ask first. `FORCE=1` skips the
prompt; without a tty they refuse rather than assume. `caboose prune
--docker` asks the same way, with or without sessions, since what it
deletes cannot come back, and lists those stopping a running VM ends.

Sessions are per project and per terminal: a second `caboose` in the same
repo while the first is attached gets `<project>-2`. `caboose --session
NAME` (or `CABOOSE_SESSION=NAME`) names one outright, attaching if it exists.

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
agent, the sandbox's git identity and commit signing, and the sync. Each
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
