# Checkpoints

A long unattended run should never lose much work, whatever it deletes.
For the paths you list in [`[checkpoints]`](configuration.md#checkpoints),
caboose's daemon copies their files on this machine every 10 minutes while
the sandbox runs, into a store no sandbox can reach, and you can restore
any of those copies into a new directory.

```toml
[checkpoints]
paths = ["/work/dev/myproject", "~/.claude"]
```

Each path is written as the sandbox sees it: a writable part of a root, or
a path the sandbox keeps of its home. None are copied by default.

## Choosing the paths

```
caboose setup checkpoints
```

lists what is worth copying (each writable part of a root, each root that
is writable whole, and `~/.claude`), shows where the checkpoints are kept,
how often and for how long they are taken, and how each candidate would be
copied on this machine, then asks which to take: numbers from the list, or
paths, separated by spaces; Enter keeps the current ones, and `-` (or
nothing, when none are set) takes none. It writes `[checkpoints] paths` in
`config.toml` and leaves the rest of the file as it was. No restart is
needed: the daemon reads the list again.

## What is kept, where, for how long

- **When**: one checkpoint of each path every 10 minutes while the sandbox
  runs, the first as soon as the daemon's link to it starts, and one more
  when it stops, unless the newest is under 30 seconds old (the
  checkpoints' log, `checkpoints.log` in the store, says when that kept
  one from being taken). The sandbox is looked at every 30 seconds; a
  restart quicker than that, which no look sees stopped, is told by its
  new start time and takes one checkpoint with the reason `start` in
  place of the stop's, even before the 10 minutes are up (and again none
  when the newest is under 30 seconds old). `caboose checkpoint
  create` takes one whenever you ask, whether the sandbox runs or not.
- **For how long**: every checkpoint for 2 hours, then the newest of each
  hour for 48 hours, then the newest of each day for 7 days. Ages count
  from the path's newest checkpoint, not from the clock, and the newest is
  always kept: time in which none is taken (an environment left stopped
  for a month) ages none, so what was kept when the last one was taken is
  still kept. A newest stamped later than now (the clock was set back)
  counts from now instead, so the checkpoints taken now are not judged
  old against it.
- **Pinned after a sharp shrink**: when a checkpoint holds half or less of
  the entries, or of the bytes, of the one before it, and that one held
  more than 1,000 entries or 100 MiB, the one before it is pinned: kept
  whatever its age, until a later checkpoint of the path holds at least
  as many entries and as many bytes as it did, or until you release it
  with `caboose checkpoint release ID`. The pins are in the store's
  `pins.json`; the newest 8 of a path are kept.
- **Where**: `~/.caboose/envs/<env>/checkpoints/`, beside the environment's
  `data/`, mode 0700 and never mounted into any sandbox. Deleting the
  environment's directory deletes them. While a root or a mount of any
  environment's sandbox would put the store in reach (a root of `~`, say),
  nothing is taken, pruned or restored.
- **How**: on a Mac, when the store and the path are on the same APFS
  volume (the usual case), each checkpoint is a clone of the whole tree,
  which takes milliseconds and shares the files' blocks. Elsewhere, and
  across volumes, it is a copy, by file clones where the filesystem has
  them, else a plain copy that hard-links the files unchanged since the
  previous checkpoint: slower, and full size the first time.
  `caboose checkpoint list` says which method each took, and `caboose
  setup checkpoints` and `caboose doctor` say which each path gets on this
  machine.
- **What is in one**: the files, directories and symbolic links (never
  followed) as this machine's disk holds them at the copy, which may be a
  few seconds behind what the sandbox last wrote. A tree being written
  while it is copied gives each file as it was at some instant of the copy:
  one taken in the middle of a `git gc` or a build may hold a mixture.
  Claude Code's login, `~/.claude/.credentials.json`, is never kept; any
  other secret a path holds (a `gh` token, a `.env` file) is.
- **Disk space**: clones share blocks only until files are rewritten, so
  checkpoints can grow. Below the larger of 10 GiB and 5% of the disk free
  (the soft floor) the daemon skips its checkpoints, those the sandbox
  asks for included, tried again every 10 minutes, and tells you; `caboose checkpoint create` still takes one, and
  says the disk is low. Nothing is pruned early for want of space: the
  retention rules alone decide. Below the larger of 1 GiB and 1% (the
  hard floor) no checkpoint is taken at all, and one that gets there
  stops and its partial copy is removed.

You get a notification when a checkpoint fails, when a path stops standing
for a directory, when the disk runs low (at most once an hour), when a
sandbox could reach the store, and when a checkpoint holds half or less of
what the one before it held: that one names the earlier checkpoint, now
pinned, and the commands to restore and to release it.

## Asked for by the sandbox

A session can have the daemon take one now, before something risky:

```sh
caboose-agent checkpoint                    # every path [checkpoints] lists
caboose-agent checkpoint /work/dev/myproject ~/.claude
```

It prints each checkpoint's ID (reason `agent` in `caboose checkpoint
list`), and exits 0 when every path was taken, 1 when some were not, 75
when asked too often or while another of its requests is being taken (try
again later), 77 when `checkpoint_requests = "off"`
([configuration](configuration.md#link)), 69 when the host takes none (no
paths configured, the store unreachable, a link not the daemon's). The sandbox only picks among the
paths `config.toml` lists: any other is refused as an unknown path, and
which host directory is copied is resolved on this machine as for the
schedule's, never from what the sandbox says. The take goes through the
schedule's own gate and the store's lock, so it never runs beside another
checkpoint, a prune or a restore, and waits at most 5 minutes. At most one
checkpoint of a path a minute, and 10 requests an hour (a request counts
once it has a path to take), per environment; what the link logs of them
is bounded too.

## Listing

```
caboose checkpoint list [PATH|KEY] [--json]
```

Each configured path, whether it is copied now (and if not, why: a path
under no writable part, missing, or reached through a symbolic link), the
daemon's next one when it answers, then its checkpoints, newest first: the
ID, when it was taken, why (`interval`, `start`, `stop`, `manual`, `agent`,
`before-restore`), the method and the number of entries, and `(pinned)`
for one pinned after a sharp shrink, which is also named above the list
with the command that releases it. Then the checkpoints of paths no
longer listed, which age out by the same rule. `--json` prints the same
as JSON, a pinned checkpoint with `"pinned": true` and the checkpoint
that shrank from it in `pinned_by`.

An ID is `KEY@STAMP`, as in `myproject-3fa1c2d0@20261017T231005Z`: the
path's last part and a hash of it, then the UTC time it was taken.
`KEY@latest` names the newest.

## Restoring

```
caboose checkpoint restore ID --to DIR
```

copies checkpoint ID into DIR, which must not exist: a restore never
writes over anything, and never in place. Compare the copy with the live
tree, and copy back what you need by hand.

- First, while the path is still listed and copied, it takes a checkpoint
  of the live path and prints its ID, so whatever you then do with the
  copy can be undone.
- The copy is made beside DIR and renamed into place only when complete,
  by a rename that never replaces anything; a filesystem that cannot do
  that gets the finished copy left where it was made, and the message says
  where.
- It is made safe for your own tools to open, since a sandbox wrote it:
  setuid, setgid and sticky bits are cleared, and each change is listed.
  Nothing else is changed: a repository's git config and hooks come back
  as the sandbox left them, and run on your machine when your git uses
  them, so read them before you run git in the copy.
- Nothing in the copy is hard-linked to the store: editing it never
  changes a checkpoint.
- A DIR inside `~/.caboose`, or one a sandbox can write, is refused
  before anything is written (naming, for the latter, the environments
  whose sandboxes can write it): such a sandbox could change the copy
  while it is written and made safe. Which paths sandboxes write is asked
  of the daemon, and the writable parts of every environment's configured
  roots count too, mounted yet or not; when the daemon does not answer,
  every root of every environment, every mount of their sandboxes and
  every data dir's home count as writable, and the restore says so. A DIR
  in a read-only part of a root is fine, and it says which sandboxes can
  read it.
- A copy of a home path holds whatever secrets that path held, but the
  Claude Code login: keep it private.

It checks everything before writing anything, and exits 1 on any failure.
No command deletes a checkpoint: retention does.

## Releasing a pin

```
caboose checkpoint release ID
```

unpins checkpoint ID, under the store's lock. Nothing is removed then:
from the next prune, within the hour, the retention rules keep or drop it
like any other. An ID that is not pinned is refused.

## When something is wrong

`caboose status` has a line for the checkpoints when paths are set: how
many, and how long since the newest and by which method, or how many fail
and the first reason. It shows a failing schedule (a store a sandbox can
reach, the daemon skipping its checkpoints below the soft disk floor, a
path that is not copied, a take that failed, among them one stopped by the
hard disk floor, a newest checkpoint over 25 minutes old while the sandbox
runs) and never hides it. Pinned checkpoints are counted after the
newest, as in `checkpoints: 1 path, newest 4 min ago (clonefile); 1
pinned after a sharp shrink (caboose checkpoint list)`.

`caboose doctor` goes further, for an environment with `[checkpoints]
paths`:

- each path: copied from which directory, or why not, and the fix;
- the method each path gets: a whole-tree clone on one APFS volume with the
  store; otherwise copies that are full at first and linked after (slower),
  which doctor warns of on a Mac and only notes on Linux;
- the store: mode 0700, and out of reach of every root and every mount of
  every environment's sandbox, with the fix (narrow the root, or move
  `CABOOSE_HOME`) when it is not;
- the free space where the store is, against the two floors above (below
  the soft one, that the daemon skips its checkpoints);
- a newest checkpoint over 25 minutes old while the sandbox runs, with the
  daemon's last error;
- a sharp shrink, naming the earlier checkpoint to restore from, and a
  pins file that cannot be read (nothing is pruned while it is so);
- as notes: each pinned checkpoint, with the command that releases it;
  leftovers of a checkpoint that did not finish, checkpoints that
  cannot be read, checkpoints kept for paths no longer listed (with their
  size), and `CABOOSE_DATA_DIR` being set, which the daemon's schedule does
  not serve.

Doctor only reads: it never makes the store.
