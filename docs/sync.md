# Syncing between machines

`caboose sync` keeps the part of the data dir worth carrying around —
memories, settings, skills — in step across machines, through any git remote
the host can push to. Set the remote once, on each machine:

    caboose sync --remote git@github.com:you/caboose-state.git

(or answer `caboose setup sync`, which sets it the same way and asks
about `auto_sync`), and from then on `caboose sync` syncs: it sends this machine's changes
since the last sync, takes the other machines', and writes back only the
files that changed. Use a **private** repo: memories are notes about your
projects.

What syncs, and nothing else:

| in the data dir | merged |
|---|---|
| `.claude/projects/<project>/memory/` | as text; lines each side added to a `MEMORY.md` index are both kept |
| `.claude/settings.json` | key by key |
| `.claude/skills/`, `agents/`, `commands/` | as text, file by file — not `skills/synced/`, Claude Code's own cache of the skills your account provides |
| the `mcpServers` key of `.claude.json` | key by key; the rest of that file never leaves the machine |

It is an allowlist: the credential, transcripts, prompt history, the Claude
Code binaries, git/jj/gh config and anything new that appears in `~/.claude`
stay put, and an export that looks like it holds a token (an Anthropic,
GitHub or AWS key, a private key, an OAuth token field) is refused before
anything is committed. A file the remote adds outside the allowlist is never
written.

**Links are never followed.** The sandbox can write `.claude`, so the
launcher, which does the file work on the host, takes nothing there at its
name: a symlink or a hard link, or a directory on the way that is one, is
neither read nor written through, and the sync names it (`not synced,
being symlinks or hard links`). Whatever the remote has of such a path is
kept, not read as deleted. The sync repo gets no symlinks either, whatever
a remote committed.

**Project memory follows the checkout.** Claude Code keeps a project's
memory under a key made from its path, and the sandbox's paths are the same
on every machine (`/work/...`, [How it works](how-it-works.md)), so `you/project`
under the repo root is `-work-you-project` on each, and the sync stores it
as it is. With `[roots]`, name them alike everywhere. Projects outside
`/work` (a session started elsewhere) do not sync, and the sync names them.

**Conflicts.** Settings merge key by key, and text files with git's merge,
so edits to different keys or different lines just combine. When both
machines changed the same key or the same lines, a sync run from a terminal
asks: keep this machine's, take the remote's (for JSON, only the conflicting
keys), edit the file with conflict markers in `$VISUAL`/`$EDITOR`, or abort.
With no terminal, or on abort, it stops before changing anything in
the data dir; your changes are committed locally and go out with the next
sync.

**On launch, if you ask for it.** With `auto_sync = true` in `config.toml`
(or `CABOOSE_AUTO_SYNC=1`), a launch that finds nothing running in the
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
`caboose sync` refuses while any session is up and names them (`FORCE=1`
overrides; restart the sessions after). That is every Claude Code process
in the container, not only tmux sessions: a `CABOOSE_NO_TMUX` session, a
`caboose claude -p` run or background agents count too (`caboose status` lists
those as "outside tmux"). It starts the container if it is
stopped, as a launch would.

**git runs in the sandbox, not on your host.** The launcher reads and writes
the files, but every git command runs in the container through `docker
exec`, so the host needs no git, and neither your host's git config, hooks
and credential helpers nor anything a remote sends come near it. Pushes use
what the sandbox has: the forwarded SSH agent, `gh`'s token, and the
sandbox's own git config (`<data>/dot_config/git`). That is why the
image must have git (2.28 or later).

Over SSH, the sync's ssh keeps the remote's host key in
`<data>/sync/.git/known_hosts`, so the sandbox asks about it once, on the
first `caboose sync` from a terminal, and not again when the container is
recreated. That file is the sync's own, apart from the sandbox's
`~/.ssh/known_hosts`. A `core.sshCommand` in the sandbox's
git config is left alone, known hosts and all.

The sync keeps its own git repo in `<data>/sync/`, mounted in the
container at `~/.caboose-sync`. The data dir itself is never a work tree,
so no checkout, reset or stray ignore rule can ever touch the credential
or transcripts in it, and git never checks anything out over it: a sync
exports the allowlist into that repo, commits, fetches, merges there, and
applies the result file by file. Delete `sync/` to start
over; the next sync merges with the remote as a new machine would, and
nothing in the data dir is lost.
