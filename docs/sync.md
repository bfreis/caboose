# Syncing between machines

`caboose sync` keeps the part of the data dir worth carrying around —
memories, settings, skills, start-up scripts, whatever else you name — in
step across machines, through any git remote the host can push to. Set the
remote once, on each machine:

    caboose sync --remote git@github.com:you/caboose-state.git

(or answer `caboose setup sync`, which sets it the same way and asks
about `auto_sync`), and from then on `caboose sync` syncs: it sends this machine's changes
since the last sync, takes the other machines', and writes back only the
files that changed. Use a **private** repo: memories are notes about your
projects.

What syncs is what the [sandbox config](sandbox-config.md)'s rules say,
and nothing else. By default:

| in the sandbox | merged |
|---|---|
| `~/.claude/projects/{roots}/memory/`: the memory of each project under the roots | as text; lines each side added to a `MEMORY.md` index are both kept |
| `~/.claude/settings.json` | key by key |
| `~/.claude/skills/`, `agents/`, `commands/` | as text, file by file — not `skills/synced/`, Claude Code's own cache of the skills your account provides |
| the `mcpServers` key of `~/.claude.json` | key by key; the rest of that file never leaves the machine |
| `~/.config/caboose/` | as text: the sandbox config itself, and [`start.d` and `shell.d`](startup.md) |

To sync more, or less, change the rules: `caboose sync add ~/.cargo/config.toml`
and `caboose sync rm ~/.claude/commands` edit the sandbox config for you, and
it syncs too, so a rule added on one machine reaches the others with the
files it names. `caboose sync status` shows what a sync would send and
take, and changes nothing.

A keep entry added since the container was created — by hand, by `caboose
sync add` of a path no entry keeps yet, or by a sync bringing another
machine's sandbox config — is not mounted until `caboose restart`, so
nothing of it syncs before then: what were written there would be lost with
the container. The sync says so (`which the container does not mount yet:
they sync after 'caboose restart'`), and after the restart the first sync
takes what the remote has of it, as for any path just started syncing.

Everything else stays put: the credential, transcripts, prompt history, the
Claude Code binaries, gh's token, and anything new that appears in
`~/.claude`. `~/.claude/.credentials.json` never syncs whatever a rule
says, and an export that looks like it holds a token (an Anthropic, GitHub
or AWS key, a private key, an OAuth token field) is refused before anything
is committed. A file the remote has that no rule here names is never
written, and stays in the sync repo for the machines that sync it; one this
machine has only just started syncing is taken from the repo, not read as
deleted.

**Links are never followed.** Anything in the sandbox can write `.claude`,
so the sync takes nothing there at its name: a symlink or a hard link, or a directory on the way that is one, is
neither read nor written through, and the sync names it (`not synced,
being symlinks or hard links`). Whatever the remote has of such a path is
kept, not read as deleted. The sync repo gets no symlinks either, whatever
a remote committed.

**Project memory follows the checkout.** Claude Code keeps a project's
memory under a key made from its path, and the sandbox's paths are the same
on every machine (`/work/...`, [How it works](how-it-works.md)), so `you/project`
under the root `dev` is `-work-dev-you-project` on each, and the sync stores it
as it is. With `[roots]`, name them, and give them paths, alike everywhere. The
sandbox config's `roots` lists their container paths, and the memory rules
follow them: `{roots}` in a rule is the directory of any project under one of
those roots, so a root at `/opt/tools` syncs `-opt-tools` and `-opt-tools-*`,
and never `-opt-toolsmith`. The list is the file's, which syncs, so every
machine reads a rule the same way. `caboose doctor` says when a machine
lacks one of those roots, and, once a remote is set, when it has a root the
list does not name: add it there, and its projects' memory syncs from the
next sync on, what the remote already has of it included. Projects under no
listed root (a session started elsewhere) do not sync, unless a rule of the
sandbox config says so.

**Conflicts.** Settings merge key by key, and text files with git's merge,
so edits to different keys or different lines just combine. When both
machines changed the same key or the same lines, a sync run from a terminal
asks: keep this machine's, take the remote's (for JSON, only the conflicting
keys), edit the file with conflict markers in `$VISUAL`/`$EDITOR`, or abort.
With no terminal, or on abort, it stops before changing anything in
the data dir; your changes are committed locally and go out with the next
sync.

**On launch, if you ask for it.** With `auto_sync = true` in `[session]` of `config.toml`, a launch that finds nothing running in the
container syncs before it attaches — the one moment nothing is writing
what a sync merges — so each machine picks up the others' changes as it
starts and sends its own on the next launch. Only a launch on a terminal:
a script's `caboose claude -p` never syncs, and neither does `caboose shell`. It
never gets in the way: it says nothing when already in sync, one line
when it did something, and one line ending in `run 'caboose sync'` when it
could not — offline, a conflict (nothing in the data dir changes then),
anything else. It never prompts, and a remote that does not answer costs
at most 20 seconds, once. An SSH remote's host key has to have been
accepted by a `caboose sync` from a terminal first. A launch that finds a
sync running (another terminal's) waits for it, whether `auto_sync` is set
or not. `caboose doctor` says what is waiting on either side without
syncing: this machine's changes not sent, and (after a fetch) the
remote's not taken, or why the remote cannot be reached.

**Only between sessions.** Running sessions write the very files a sync
merges, and Claude Code rewrites `.claude.json` whole from memory, so
`caboose sync` refuses while any session is up and names them (`CABOOSE_FORCE=1`
overrides; restart the sessions after). That is every Claude Code process
in the container, not only tmux sessions: a `tmux = false` session, a
`caboose claude -p` run or background agents count too (`caboose status` lists
those as "outside tmux"). It starts the container if it is
stopped, as a launch would.

**The sync runs in the sandbox, not on your host.** The launcher starts it
there (`caboose-agent sync`, under every isolation), and it reads and writes
the files and runs git on the home and the sync repo as the sandbox sees
them, so git never meets a file the host changed a moment ago that the
sandbox's view still has otherwise (a VM's shared folders cache what they
show). The host needs no git, and neither your host's git config, hooks and
credential helpers nor anything a remote sends come near it; the launcher
only holds the lock, says what happened, and asks at your terminal what
the sync needs asked: how to settle a conflict, and git's or ssh's own
questions (a host key to accept, a username, a password or a passphrase,
the last two without echo). Pushes use what the sandbox has: the forwarded
SSH agent, and `gh`'s token when gh is in the image. The sandbox's own git
config is not read at all: it can arrive by sync, and a `url.*.insteadOf`
or a credential helper in it must not redirect the sync's own push. That is
why the image must have git (2.28 or later). A container made from an
older caboose's image, whose `caboose-agent` cannot run the sync, is
refused with what fixes it: `caboose restart`. Interrupt a sync (^C, a closed terminal) and it
stops in the sandbox too, finishing any files it was writing back; the
next sync takes it from there, and one started while it stops is told to
try again in a moment.

Over SSH, the sync's ssh keeps the remote's host key in
`<data>/sync/.git/known_hosts`, so the sandbox asks about it once, on the
first `caboose sync` from a terminal, and not again when the container is
recreated. That file is the sync's own, apart from the sandbox's
`~/.ssh/known_hosts`. A `GIT_SSH_COMMAND` the container's environment sets
is left alone, known hosts and all.

The sync keeps its own git repo in `<data>/sync/`, mounted in the
container at `~/.caboose-sync`. The data dir itself is never a work tree,
so no checkout, reset or stray ignore rule can ever touch the credential
or transcripts in it, and git never checks anything out over it: a sync
exports what the rules name into that repo (each file at `home/<its path
under ~>`), commits, fetches, merges there, and applies the result file by
file. Delete `sync/` to start
over; the next sync merges with the remote as a new machine would, and
nothing in the data dir is lost.
