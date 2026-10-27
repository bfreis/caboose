# Security: what caboose protects, and what it does not

This is caboose's threat model: who it defends against, what of your
machine each isolation keeps out of a sandbox's reach and by which
control, and what it does not cover. It is written for deciding whether to
trust caboose with an agent you are not watching.

It describes caboose as of the release it ships with. Every control below is **in effect**:
the code enforces it, under the conditions its entry gives (while the
daemon runs, for the paths you list).

What is in effect is not the same as what has been tested end to end:
[the matrix](#the-matrix) says which controls the conformance suite
checks; it records no results.

A control added, removed or weakened is a change to this document. Every
setting the [configuration reference](configuration.md) describes is
enforced as it says: `trust`, a root's `writable`, `[alias.NAME]`,
`egress_deny`, `open_urls_allow` and `[checkpoints]` included.

## The promise, in one screen

The model: **the sandbox gets nothing by default, and
every capability that crosses to the host is a grant held in the host's
configuration, which no sandbox can read or write, and enforced by the
host.** A session can only ask; you decide, on the host.

What you can rely on:

- The sandbox reaches your disk only through what caboose mounts: the
  roots you configure, its own data dir's kept directories, and caboose's
  own read-only instructions. Nothing else of your files is in it, under
  any isolation.
- **Everything under a root is readable from the sandbox, and the parts
  its `writable` lists are writable.** The rest of each root is mounted
  read-only. A mistaken `rm -rf`, or a hostile agent, can delete or change
  every file in a writable part.
- Commands on your machine (`host_exec`) are off unless you turn them on,
  and the sandbox cannot turn them on.
- What the sandbox sends the host (ports, URLs, notifications, outbound
  connections under `vm`, host commands) is treated as untrusted and
  decided by the host's settings. A URL, an outbound connection and a host
  command are decided by one policy, deny beating allow; what asks a
  person is refused at once as pending, and only the host answers it (a
  dialog on the host, `caboose requests`, the web page or the menu-bar
  app).
- The sandbox cannot change its own mounts, isolation, image or host
  settings; it can propose a change, which you review with `caboose apply`.

What not to rely on: the confidentiality of
anything under a root or in the sandbox's home; the network (under
`container` and `gvisor` it is the engine's, unrestricted); your SSH agent,
which the sandbox can use as it likes; and the [not covered](#not-covered)
list.

## Who it protects against

Three adversaries, each assumed able to run any code in the sandbox as its
user (and as root where the sandbox has one: in a `vm` guest, and under
gVisor on some engines):

1. **A fallible agent.** Well-meaning, and wrong: a test that deletes more
   than it meant to, a script run in the wrong directory, freshly written
   code under test. caboose's main promise is about this one: a mistake
   should not reach past what the sandbox was given.
2. **A malicious repository.** Files in a checkout the agent works in (a
   Makefile, a package's install script, a git hook, an instruction hidden
   in a README) that steer the agent or run when something builds them.
3. **A steered or hostile agent.** A session an attacker steers through
   injected content, or a model working against you, running arbitrary code
   with whatever the sandbox holds and looking for a way out.

Outside the model, and assumed not to happen:

- someone already running code on your machine as you (they need no
  sandbox escape);
- a malicious caboose release, or a malicious base image you chose;
- an escape from the kernel, gVisor, the Docker engine or its VM, or
  Apple's Virtualization.framework.

## Trust levels

`trust` in `config.toml` says which adversary to assume: `"fallible"` (the
default) or `"hostile"`. It is the host's setting; the sandbox has no way
to change it.

Under `"hostile"` a launch needs a `vm` isolation profile and refuses
anything else, creates the VM with no network of its own, ignores
`host_exec`, refuses an alias whose working directory, command or
argument is in a writable path, asks before every URL, asks before every
outbound connection through the proxy to a host `egress_allow` does not
name, and makes `egress = false` in the `vm` profile it uses an error. The
table says what each level changes:

| | `fallible` | `hostile` |
|---|---|---|
| isolation | any | `vm` only; a launch refuses `container` and `gvisor`, and a sandbox created under one (in effect) |
| `host_exec` | as configured | refused: not offered, and every request denied (in effect) |
| an alias whose command is in, or reached through, a writable path | refused (in effect) | refused (in effect) |
| an alias whose working directory or an absolute argument is in a writable path | allowed | refused (in effect) |
| `open_urls` | as configured | every URL asks first; `"allow"` and `open_urls_allow` ignored (in effect) |
| outbound connections through the `vm` proxy | public addresses; a private one asks | every host `egress_allow` does not name asks (in effect) |
| the `vm` network outside the proxy | macOS's NAT | none: the VM is created without a network, so only what goes through the host's proxy leaves it (in effect) |
| everything else | the same | the same |

Under `hostile`, then, the inner containers a `vm` sandbox's own `dockerd`
runs would reach no network, since they do not use the proxy.

## Isolations

The isolation is `config.toml`'s, fixed when the sandbox is created
([configuration](configuration.md#isolation-profiles)).

| | `container` | `gvisor` | `vm` (Mac) |
|---|---|---|---|
| kernel | the engine's Linux VM's, shared with every container | gVisor's own, in user space | the guest's own, in a VM of its own |
| the sandbox's user | the agent user, never root | the agent user, or a root that exists only inside gVisor | root in the guest |
| how a read-only mount holds | the kernel's read-only bind mount | the same, through gVisor | Virtualization.framework's read-only share: the Mac refuses the write |
| network | the engine's: anything it reaches, your Mac's services included | the same | macOS's NAT; under `egress = true` also a proxy run on your Mac |
| the engine's socket | only with `engine_socket = true` (root on the host) | the same | never |

Root in a `vm` guest can unmount or cover a read-only mount inside the
guest, until the next boot, but cannot write your files through a share the
Mac holds read-only. That is the whole of the `vm` claim, and it holds only
with a `caboose-vmm` that knows read-only shares: starting a VM checks its
version and says when it does not match.

In a `vm` guest each shared directory is at its mount point and nowhere
else: the guest mounts the Mac's one share, binds each of its directories
into place, then detaches the share itself. Root in the guest can mount
the share again and reach every shared directory through it, the
read-only ones still read-only, since the Mac holds them so.

## What is protected

| what | from what | by | status |
|---|---|---|---|
| your files outside the roots | reading, changing, deleting | not mounting them | in effect |
| files under a root | changing, deleting | read-only roots, with the writable parts you list | in effect (`vm`: [see below](#read-only-roots-and-writable-parts)) |
| a writable part's parent | changing, deleting, swapping a directory for a link | the parent mounted read-only; every component checked at each launch and again at creation | in effect |
| your host credentials (the Keychain, `~/.ssh`'s keys, your own `~/.claude`, a `gh` login on the host) | reading | not mounting them; the SSH agent forwarded, never the keys | in effect |
| host commands | the sandbox running anything on your machine | `host_exec` off by default, refused under `hostile`; host commands get only `PATH`, `HOME` and `LANG` | in effect |
| host commands | as above, with a few named commands allowed | aliases the user defines in `config.toml`: a fixed argv and listed values, decided at each run, never a command in or through a writable path | in effect |
| the daemon | the sandbox reaching it | its socket and directory in no mount, only your user can connect | in effect |
| the host's settings | the sandbox changing them | `config.toml` in no mount; proposals reviewed on the host | in effect |
| requests and grants | the sandbox answering its own | requests filed by the host, answered only on the host (the URL dialog, `caboose requests`); grants in no mount | in effect |
| checkpoints | a mistake losing work in a writable part | copies on the host, every 10 minutes while the sandbox runs, in no mount; restored only into a new directory, made safe to open | in effect, for the paths `[checkpoints]` lists |
| checkpoints asked for by the sandbox | the sandbox choosing what is copied, or flooding the store | only paths `[checkpoints]` lists, resolved on the host; `checkpoint_requests`; one per path a minute and 10 requests an hour | in effect |
| caboose's instructions to sessions | the sandbox rewriting them | a read-only mount | in effect |
| your Mac's network, under `vm` | connections through the proxy | the proxy's ports and address rules, `egress_deny`, a person's answer for a private address | in effect, for what uses the proxy |
| your network, under `vm` with `hostile` | any connection | no NAT; the proxy's rules decide | in effect |
| the web page and the menu-bar app that approve requests | the sandbox opening, reaching or using them | the page's ports (7171, 7172) refused for forwarding, URL opens, vm egress and `run_args` publishes; served on `127.0.0.1` only; a token on every API call, `Host`, `Origin` and `Sec-Fetch-Site` checks, a strict CSP, no framing, no CORS; the app refuses to run from a place a sandbox reaches | in effect |
| your browser and your desktop | the sandbox opening pages, showing text | URL checks, a person's answer by default (never waited on), printable text, rate limits | in effect |

## The matrix

Per control and per isolation, what caboose claims. The conformance suite
that tries each refused operation from inside each isolation is built
([How to verify](#how-to-verify)), and **this document records no results
of it**: a cell reads as not tested until one is recorded, and "no
check" where the suite has no check for the row. The macOS host is the one
caboose is built and tested on, and Linux hosts are untested.

<!-- matrix:begin -->
| control | `container` | `gvisor` | `vm`, `fallible` | `vm`, `hostile` |
|---|---|---|---|---|
| only the mounted paths are reachable | held (not tested) | held (not tested) | held (not tested) | held (not tested) |
| read-only roots and writable parts | held (not tested) | held (not tested) | held (not tested) | held (not tested) |
| read-only instructions mount | held (not tested) | held (not tested) | held, until the guest's root unmounts it (not tested) | held, until the guest's root unmounts it (not tested) |
| daemon out of reach | held (not tested) | held (not tested) | held (not tested) | held (not tested) |
| `host_exec` off unless set | held (not tested) | held (not tested) | held (not tested) | held, refused even when set (not tested) |
| aliases: only `config.toml`'s, no command in or through a writable path | held (not tested) | held (not tested) | held (not tested) | held, no working directory or argument in a writable path either (not tested) |
| ports forwarded only as the host says, on `127.0.0.1` | held (no check) | held (no check) | held (no check) | held (no check) |
| URL checks | held (no check) | held (no check) | held (no check) | held, always asks (no check) |
| outbound rules | not covered (not tested) | not covered (not tested) | partial: proxied traffic only; the NAT is open (not tested) | held, every unnamed host asks; no NAT (not tested) |
| request queue and grants | held (not tested) | held (not tested) | held (not tested) | held (not tested) |
| the host's settings: proposals and the sandbox config reach nothing of the host's | held (not tested) | held (not tested) | held (not tested) | held (not tested) |
| checkpoints (on the host, the same under every isolation) | held (not tested) | held (not tested) | held (not tested) | held (not tested) |
| web page: port refused, token and request checks | held, run_args --network host is allowed under fallible, warned by doctor and caboose ui, not refused (not tested) | held, run_args --network host is allowed under fallible, warned by doctor and caboose ui, not refused (not tested) | held (not tested) | held, D7 not run (hostile asks before every URL) (not tested) |
| `hostile` refused | held (not tested) | held (not tested) | n/a | n/a |
| `trust` is the host's: labels say what runs, `hostile` cannot be turned off from the sandbox | held (not tested) | held (not tested) | held (not tested) | held (not tested) |
| the SSH agent under `hostile`: none unless `ssh_agent` names one | n/a | n/a | n/a | held, an agent ssh_agent names is forwarded (not tested) |
| the sandbox's home holds what Claude Code keeps (a recorded gap) | not covered, the home is readable and writable by design (not tested) | not covered, the home is readable and writable by design (not tested) | not covered, the home is readable and writable by design (not tested) | not covered, the home is readable and writable by design (not tested) |
<!-- matrix:end -->

## Each control

### Mounts: what the sandbox can reach

In effect. A sandbox is created with these and nothing else of your
machine: each root at its fixed path under `/work`; the directories the
[sandbox config](sandbox-config.md) keeps of its home, each from the
environment's data dir (`~/.caboose/envs/<env>/data/home/`); Claude Code's
own install, the [sync](sync.md) repository and the
[proposals](proposals.md) directory, all from the same data dir; caboose's
instructions at `/etc/claude-code`, read-only; and, under `container` and
`gvisor`, the SSH agent's socket, when it gets one. A session cannot add a mount: mounts are
fixed when the sandbox is created, from the host's `config.toml`.

It relies on the engine's bind mounts (`container`, `gvisor`) and on
Virtualization.framework's shares (`vm`). It stops a mistake or an agent
from reaching any other file of yours. It does not stop anything inside
what is mounted writable: the writable parts of the roots, and the kept
directories of the home.

The sandbox config, which the sandbox writes, names only paths under the
sandbox's home, kept under the data dir; it cannot name a host path or one
of caboose's own mounts.

Your `config.toml` and the daemon's directory are in `CABOOSE_HOME`
(`~/.caboose`), and in no mount: of `CABOOSE_HOME`, caboose mounts only
the parts of the data dir named above. A root that contains it (your
whole home, say) would put it in reach, so such a root is refused when
the roots are read, in `caboose setup roots` and in a proposal, judged by
the directories' identity on disk (a `CABOOSE_HOME` not made yet by its
nearest existing parent), and the refusal names a narrower root to use
instead (`~/dev` rather than `~`).

### Read-only roots and writable parts

In effect. A root is read-only to the sandbox unless its `writable` list
names the parts it may write, each mounted read-write over the read-only
root at the same path. A root with no list, the default root included, is
read-only throughout. At every launch, and again just before the sandbox
is created (once a sandbox it replaces, which could write the root, is
gone), every component of a writable part must be a real directory (no
symbolic link), and the components above the last stay in the read-only
part, so the sandbox cannot swap one for a link afterwards. Only
`config.toml` sets the list, never the sandbox or a proposal, and changing
it takes `caboose restart`: until then, a launch and `caboose doctor` say
that the sandbox may write what the configuration no longer lists.

Under `container` and `gvisor` the root is the engine's read-only bind
mount and each part a writable one over it. Under `vm` the root is a
directory the Mac shares read-only, and each part a writable share of its
own: the guest's root can unmount or cover the read-only mount until the
next boot, but cannot write your files through it. A read-only
share with a writable one inside it is what the conformance suite's root
checks exercise under `vm`; this document records no result of them.

What it does not cover:

- A sandbox created by a caboose from before read-only roots writes every
  root whole, until `caboose restart` recreates it.
- Another process that can write the read-only part of a root (another
  environment with that part writable, or your own tools) could put a link
  in a writable part's path between the last check and the mount. The
  engine also restarts a stopped container by itself (after the engine
  restarts, say), mounting the parts again with no launch to check them.
- Paths are compared as written: on a case-insensitive disk, a root and a
  writable part spelled with other cases can name the same directory.
- Under `gvisor` and `vm`, your edits in the read-only part of a root reach
  no file watcher in the sandbox: the link raises its events only in the
  writable parts.

### The sandbox's home

In effect, by design: the kept directories (`~/.claude` with every
transcript, `~/.claude.json`, `~/.config/gh`, `~/.ssh`, the sandbox's git
config, whatever else the sandbox config keeps) are readable and writable
by the sandbox. Anything you store there, a `gh` login included, belongs
to whatever runs in the sandbox. `~/.ssh` there holds `known_hosts` and an
ssh config, never keys (`caboose doctor` notes one it finds).

### caboose's instructions

In effect. `/etc/claude-code` holds caboose's instructions, skills and
Claude Code mod for every session, mounted read-only under every isolation, so a session cannot
rewrite what the next one is told. Under `vm`, the guest's root can unmount
or cover it until the next boot, but not change the host's copy.

### The host link

In effect. Everything `caboose-agent` sends is untrusted
([details](host-link.md#what-the-sandbox-can-and-cannot-do-with-it)):

- ports forward only as `forward_ports` in the host's `config.toml` says,
  or as a grant you gave to a request for one says, on `127.0.0.1` only,
  at most 64 at once, never 7171 or 7172, the web UI's ports, whatever it
  says, never one below 1024 that `forward_ports` does not name, and never
  a port a program on this machine accepts connections on, checked each
  time the sandbox starts listening on the port and again after a forward
  of it ends with its grant; a listen never files a request, only
  `caboose-agent request port` does;
- a URL opens only if it is `http` or `https`, holds no user or password
  and nothing unprintable, is not to port 7171 or 7172 (whatever its
  host), and the policy allows it: by default a person
  does, in a dialog on the host; the sandbox is told at once that its
  request is pending, never kept waiting, and the host opens the URL once
  it is allowed;
- notification text is made printable and cut short; requests are
  rate-limited, and the waits for decisions bounded (at most 16 on a
  link); dialogs come one at a time;
- the protocol is framed and bounded, and a violation ends the link;
- file changes go one way, from the host, and the agent touches only paths
  under the roots that the host names, never through a symbolic link.

It does not stop a forwarded port from squatting one of your own services'
ports while that service is down, or one that starts on all addresses
(`*:PORT`) after the forward exists, or one that binds between the check
and the forward's listener; the check sees only a service that is already
up. Keep `forward_ports` to the ranges you use.

### The daemon

In effect. One daemon per `CABOOSE_HOME` runs every environment's link (a
checkout's build runs a second one, in `daemon-dev/`). Its
directory (`~/.caboose/daemon/`) is mode 0700 and in no mount; its socket is
mode 0600, and it checks each connection's peer credentials and accepts
only your own user. It is started with a minimal environment, and asks a
login shell once for `PATH` and a few others, so a secret exported in a
terminal does not reach it or the commands it runs.

It does not protect against another process of your own user, which can
talk to it as you do.

### Host commands and aliases

`host_exec` is in effect as it always was: off by default, and only the
host's `config.toml` turns it on, and `trust = "hostile"` refuses it
whatever `host_exec` says: the link does not offer it, and the policy
denies every request for it. On, it is **a hole in the wall, on
purpose**: the sandbox, and whatever steers it, can run anything on your
machine as you. A command gets exactly `PATH`, `HOME` and `LANG`, no
terminal and no shell; it runs in a directory under a root; it ends when the
sandbox's command ends. Keep it off for any environment you leave
unattended.

Aliases are in effect: named commands defined in the host's `config.toml`
only (never in the sandbox config, never synced, never in what the host
tells sessions a proposal is made against), a fixed argv plus parameters
chosen from fixed lists, no shell; the sandbox sees names, descriptions and
parameters, never the commands. At every run the host resolves the
command (every symlink followed, and a script's `#!` interpreter with it), its working directory and its absolute
arguments on its own disk, and refuses a command within, or reached through
a symlink within, anything any environment's sandbox can write, read off
the sandboxes' real mounts (running or stopped) and never off
`config.toml` alone; when that cannot be told, every alias is refused.
Paths are compared by device and inode as well as by name, so another
spelling of a writable directory (a firmlink, another Unicode
normalization) is the same directory. What runs is the path that was
checked. An alias runs only while its caller waits, and it, and what it
started in its process group, are killed when the caller goes, at its
timeout, and with the link; a process that leaves its group escapes that.
With no `cwd`, it runs in a fresh empty directory that is removed
afterwards, only inside the directory made for that. Under `hostile`, an
alias whose working directory or an absolute argument is in a writable
path is refused too, since the sandbox could rewrite what it reads. Under
`fallible`, an alias that runs what a project file says (`make test` in a
writable project) gives the sandbox a way to run code on your machine, and
is allowed: it is your choice. A session can propose an alias, which
`caboose apply` refuses outright when its command is a shell or a program
that runs what its arguments say, is in a root, a hidden directory of the
home or caboose's state, or does not exist ([proposals](proposals.md)).

### Requests and grants

In effect for URL opens, outbound connections through the `vm` proxy,
host commands and aliases: each is decided by one rule, allow,
deny or ask (host commands only allow or deny: `host_exec` is on or off,
and nothing of them asks), with deny beating allow, in the process that holds the
environment's link (only it serves the environment's requests). Nothing
blocks the agent: a crossing that asks is refused at once as pending,
with the number of the request it filed (the same number while one for
the same thing is pending), and an unanswered request expires after a
day as denied. Requests are bounded: at most 100 pending in an
environment and 10 new a minute, and what the sandbox says of one is
bounded, made printable and kept apart from the host's own description of
its effect. Requests, grants and the decisions log live in the
environment's directory under `CABOOSE_HOME`, in no mount; nothing the
sandbox sends answers a request or makes a grant. A grant given for a
time reaches a private address only if it holds that very address, so a
name that later resolves elsewhere private asks again; a name in
`egress_allow`, which you wrote, reaches whatever it resolves to.

Asking ahead, in effect: a session can file a request itself, with its own
words about why (`caboose-agent request`, or the `caboose_request` tool of
caboose's Claude Code mod, [host link](host-link.md#asking-the-host-ahead-of-time)).
That files the same request a crossing would, decided by the same rule and
shown to you in the same way: the sandbox's words are text beside the host's
description, never the decision, and the request is bounded in size and
number like any other. The mod only informs: it tells a session what the
host decided, and nothing it does or fails to do changes a decision, which
the host enforces. Any process in the sandbox can claim any session (or
none); that changes only which session is told, never what is allowed. The
count of Claude Code sessions and of those with the mod, shown by `caboose
permissions`, is the sandbox's own count, sent over the link: it can be wrong
if the sandbox lies, and is used for nothing but that line. The mod lives in
`/etc/claude-code/mods/caboose`, read-only like the rest of caboose's
instructions, and a setting that replaces `CLAUDE_CODE_PLUGIN_DIRS` removes
it from a session without removing any restriction.

Answering, in effect: a URL request from the dialog on the host, and any
request from the command line, `caboose requests` (the daemon starts if it
is not running; it answers while the environment's link runs). Each
request is shown with what allowing it does, in caboose's words, then what
the sandbox says of it, which is only its claim. `requests approve N`
allows it once: exactly the request shown (a URL opens; one connection to
that host on that port may be made, or the alias run with those very
arguments, once, within 15 minutes). A request for another port, or the
alias with other arguments, is a request of its own, filed and asked
apart, never merged into one already shown. `--for 8h` or `--for 2d`
allows for some hours (1h to 7d; listed by `caboose grants`, ended by
`caboose grants revoke ID`), and `--always` for good: both cover the
whole alias, with any arguments, or the whole host, on any port
`egress_ports` allows. Each offer says which, in the words of its
effect. `--always` edits `config.toml`: it shows the literal
change, line by line, and writes it only after you type `yes` on a
terminal, to the same lines `caboose setup` edits (comments and other
settings stay; the comments on the line it replaces, or inside an array it replaces, go with it, and the diff shows that), and
refuses to write if `config.toml` changed since it showed you the change.
The very next try is then allowed. `caboose permissions` lists what the
sandbox may do now: trust, roots and what is writable, aliases, host exec,
URLs, the outbound rules and the grants. A request is answered
in the dialog, with `caboose requests`, on the web page (`caboose ui`), or
in the menu-bar app ([below](#the-web-page-and-the-menu-bar-app)). With `CABOOSE_DATA_DIR` set, a detached helper owns the link
and the daemon answers nothing for the environment.

### Outbound connections

Under `vm` with `egress = true`, in effect for **the connections that go
through the proxy** ([details](host-link.md#outbound-connections)): only
`egress_ports`, only public addresses as your Mac resolves them, unless
`egress_allow` lets a name or a range through, or a person allows the
request a private address files; `egress_deny` refuses whatever it
names, beating every allow; loopback, link-local and your Mac's own
addresses are refused even for a name `egress_allow` allows, and on
ports 7171 and 7172 (the web UI's) even for an address or CIDR it allows,
or an answer. Under
`hostile`, every host `egress_allow` does not name asks. At `fallible` the VM
also keeps macOS's NAT, and a program that ignores the proxy settings
leaves through it, unrestricted: there the proxy is a way to reach what
your Mac reaches, not a lockdown. Under `hostile` the VM is created with
no NAT, so the proxy is the only way out (HTTP(S) through the proxy
settings, ssh through its `ProxyCommand`); a program that ignores them
reaches nothing, and neither do the containers the VM's `dockerd` runs.
Claude Code's own hosts then have to be in `egress_allow`
([which](configuration.md#trust)): a hostile launch refuses until they are (and until `egress_ports` allows 443). An existing sandbox that was not created as a hostile one is refused until `caboose restart`.

Under `container` and `gvisor`, outbound connections are the engine's and
are **not covered**: the sandbox reaches whatever the engine does,
including your Mac's own services (`host.docker.internal`, OrbStack's
`host.internal`) and your local network.

### Checkpoints

In effect while the daemon runs, for the paths you list; on a Mac a
checkpoint is taken by cloning the whole tree. Restored with `caboose
checkpoint restore` ([Checkpoints](checkpoints.md)). For the paths you list in
`[checkpoints]` (writable parts and kept home directories), the daemon
takes a copy every 10 minutes while the sandbox runs, one when its link
starts and one when the sandbox stops, cheaply on APFS (a clone of the
tree), kept every 10 minutes for 2 hours, hourly for 48 hours and daily for
7 days, counted from each path's newest checkpoint, which is always kept,
in `~/.caboose/envs/<env>/checkpoints/`, in no mount
([configuration](configuration.md#checkpoints)). Only the daemon that runs
an environment's link takes its checkpoints, one at a time, and every
take and prune holds the store's own lock, so two daemons sharing
`~/.caboose` never take the same one twice; pruning removes nothing but
the checkpoints the retention rule drops, inside the store, and never one
early for want of space (below the soft disk floor the daemon takes none
instead). Since ages count from the newest, a sandbox that stops being
checkpointed cannot age the good ones out. A sharp shrink (a checkpoint
holding half or less of the entries or of the bytes of the one before,
when that one held more than 1,000 entries or 100 MiB) is notified at
once, naming the earlier checkpoint, which is pinned in the store until a
later one holds as much again or the user releases it, since retention
would in time keep only the emptied tree. A checkpoint holds what the host's filesystem held
at the copy, not necessarily the sandbox's last few seconds of writes,
and holds whatever the paths hold but Claude Code's login.

A session may ask for one now (`caboose-agent checkpoint`, unless
`checkpoint_requests = "off"`): it names container paths, and the host
takes only those `[checkpoints]` lists, each resolved on the host as the
schedule resolves it, through the same gate and lock, at most one per path
a minute and 10 requests an hour per environment. It widens no reach, so
it is allowed under `hostile` too.

A restore always writes a new directory, never over anything and never
in place, after taking a checkpoint of the live path, and makes what it
writes safe for your own tools to open: setuid, setgid and sticky bits
are cleared. Nothing else of the tree is changed: a repository's git
config and hooks come back as the sandbox left them
([not covered](#not-covered)). Nothing is hard-linked out of the store, so editing the
copy never changes a checkpoint. A target that a sandbox can write, or
inside `~/.caboose`, is refused before anything is written, naming the
environments whose sandboxes can write it: such a sandbox could change
the copy while it is written and made safe. Which paths sandboxes can
write is asked of the daemon, which reads it off every sandbox at that
moment, and to it are added the writable parts every environment's
configured roots would mount, sandbox or not, since a restart mounts
them; a target in a read-only part of a root is restored, naming the
environments whose sandboxes can read it. Without the daemon, the
restore counts every root of every environment, every mount of their
sandboxes and every data dir's home as writable, and says so.

Which host directories are copied is the host's to decide: a path under a
root only while it is in a part the sandbox can write (as configured, or
as the running sandbox has it mounted), and a home path always from the
environment's data dir, whatever the sandbox's own config says. A path
reached through a symbolic link is not copied, nor one that is, holds or
is inside a checkpoint store or the daemon's directory. Nothing is to be
taken, pruned or restored while a root or a mount of any environment's
sandbox is, holds or is inside the store, judged by the directories'
identity on disk rather than by how their paths are spelled: a root over
`~/.caboose` would put the checkpoints in the sandbox's reach.

They recover from a deletion; they do not stop one, and nothing protects a
writable path between two checkpoints.

### The web page and the menu-bar app

In effect. `caboose ui` opens the page ([the web UI](web-ui.md)); the
menu-bar app is [below](#the-menu-bar-app). The daemon serves
`http://127.0.0.1:7171/` (`127.0.0.1:7172` for a checkout's build), and
nothing else:

- **Where it listens**: on `127.0.0.1` only, the address checked before
  and after binding: never `0.0.0.0`, `::`, IPv6, a LAN address or a
  name. Never another port: when the port is taken, the daemon runs
  without the page, says so in `caboose daemon status`, and tries again
  every 30 seconds.
- **Who can reach it**: any process on your Mac, and, under `container`
  and `gvisor` on OrbStack or Docker Desktop, a sandbox, which can open a
  connection to your Mac's loopback. A sandbox cannot reach it through
  caboose: `forward_ports` holding either port, alone or in a range, is
  refused when `config.toml` is read or written; the link never forwards
  either, whatever its set says; a URL to either port never opens,
  whatever host it names, and is never filed as a request; the vm
  outbound proxy never reaches either on a loopback, unspecified,
  link-local or own address of your Mac, whatever `egress_allow`, an
  answer or the trust level say; and a `run_args` `--publish` onto either
  is refused. The conformance suite's check `D3` exercises the
  configuration and policy layers of these; the link's own refusal to
  forward either port is pinned by a unit test of its own
  (`TestUIPortsNeverForwarded`).
- **The token is the boundary.** Every call of the page's API needs the
  page's token in a request header (never a cookie or the address); it is
  32 random bytes, made anew every time the daemon starts and kept in its
  memory alone (no file), compared in constant time, never logged; the
  page keeps it in the tab's `sessionStorage`, nowhere lasting. It is never
  in a URL, so never in a browser's history or a command's arguments.
  The page's own HTML and style load without it, and hold nothing secret.
- **Checks on every request**, before anything runs: the `Host` must be
  exactly the address served, `127.0.0.1:7171` (`127.0.0.1:7172` for a
  checkout's build): a name rebound to `127.0.0.1` sends its own,
  and is refused; a request from another site, or another port of
  `127.0.0.1` such as a forwarded sandbox page, is refused by its
  `Sec-Fetch-Site`; nothing loads inside a frame. An API call must also be
  a `POST` from the page's own `Origin`, with `Sec-Fetch-Site:
  same-origin`, a JSON body of at most 64 KiB and no query string. No
  cross-origin request is ever allowed: no `Access-Control-*` header is
  sent, and no preflight is answered.
- **What the page may call** is listed, method by method, in the daemon:
  the read-only methods (doctor among them), answering requests, revoking
  grants, editing a vm environment's egress rules, and applying or
  deleting a proposal that needs no build; each write to `config.toml` or
  a Dockerfile only with the hash of the diff it was shown. Never
  stopping or replacing the daemon, the links, the list of
  writable paths, or the token. A method added later is refused to the
  page until it is listed. An answer from the page is recorded as the
  page's, and an "always" answer is written only with the hash of the
  `config.toml` diff it was shown.
- **Headers on every response**: a strict Content-Security-Policy (no
  script, style or connection but the page's own, no inline script, no
  framing: `frame-ancestors 'none'`), `X-Frame-Options: DENY`,
  `nosniff`, no referrer, no caching, no cookies.
- **Limits**: at most 64 connections at once, each given 5 seconds to
  send its headers; 20 authenticated calls a second. A sandbox that can
  reach your Mac's loopback can hold every connection open and keep the
  page from loading while it does: that denies only the page, never
  grants anything, and the command line is unaffected.
- **Before every request that carries the token**, the page asks the
  server to prove it knows it (`/api/proof`, a MAC of the page's own
  origin and a fresh nonce, which reveals nothing usable), and sends
  the request only once that proof has verified; no proof is reused for
  a later request. A server that cannot prove it is never sent the
  token. Proofs are limited to 20 a second across every caller, as the
  API is, so anything that reaches your loopback can keep the page from
  connecting (it says so and retries); that too grants nothing.

The token reaches the page only in the answer to `/api/redeem`, which
trades a one-time code for it. `caboose ui` and the menu-bar app open the
page with a fresh code in the address's fragment (never sent to a server
but by the page's own redeem): 32 random bytes, good once and for 60
seconds, kept only in the daemon's memory, at most 16 outstanding, all
dropped when the token is replaced. Every code that opens nothing (wrong,
used, expired or malformed) gets the same answer, redeems are limited to
one a second, and the answer carrying the token is never cached. The
`web` method that makes a code is the command line's and the app's only,
never the page's. See [the web UI](web-ui.md) for what is still exposed.
`--network=host` in `run_args` of a `container` or `gvisor` profile is
not refused under `trust = "fallible"`: `caboose doctor` reports it as a
problem and `caboose ui` warns about it, since a sandbox on the host's network can listen on
7171 while the daemon is not running and take the page's place. Another
local process can do the same then. A page loaded from it, a reload of an
open tab say, can read the token that tab kept (the page keeps it in the
tab's `sessionStorage`, nowhere lasting), and can show you a page of its
own; but the daemon makes a new token every time it starts, so a token
read while it was down lets nothing in once it is back. A page opened
fresh against it gets no token at all: the page's proof keeps it from
being sent one. A `vm`
profile takes no `run_args`, and `hostile` allows only `vm`, so a `vm`
sandbox cannot be put on the host's network this way and `D3` passes
there without this case; the page is the host's, so a `container` or
`gvisor` environment beside it with `--network=host` still can.

#### The menu-bar app

In effect where it runs: on a Mac, `install.sh` puts a release's in
`~/Applications`, and a checkout builds its own
([the menu-bar app](menu-bar-app.md)). It is one more client of the
daemon, decides nothing itself, and:

- runs as you, so it refuses to start, and to be turned on at login, from
  a place a sandbox could change or reach into: a root of any environment,
  a kept home, a `run_args` mount source, what a sandbox mounts, judged by
  name and by identity, through no symbolic link; when what the sandboxes
  reach cannot be told, it refuses too;
- answers a request only after you confirm it in a dialog, recorded as
  the app's answer; the methods only the app may call are refused to
  every other client, and the app never forces a running daemon to make way;
- never shows the sandbox's text as a menu item's or a notification's
  title, only in its body, made printable;
- is replaced by `install.sh` and updates only with a bundle checked
  against the release's checksums whose signature is valid, for caboose's
  identifier and, on an update, by the same team as the one installed;
  nothing but caboose's own release bundle is ever replaced.

### Proposals

In effect. A session can propose only what [proposals](proposals.md) list:
packages, a Dockerfile section, one root, one alias alone, or one writable
part of a root alone. Anything more is refused whole, not offered; so is
text with control or invisible formatting characters. A proposed root is
refused when it is your home, a hidden directory in it, caboose's own
state, or overlaps another, judged by the directories' identity on disk
so that another name for one is refused too; you type its name to accept
it. A proposed writable part is refused when it is in no root, reached
through a symbolic link, already writable, or the home, a hidden directory
of it or caboose's state; you type its root's name to accept it, and it
takes effect at the next `caboose restart`.
`caboose apply` shows each proposal whole before anything is written.

### What the host reads from the sandbox

In effect. Wherever the host reads or writes a directory the sandbox can
also write (the kept home, the sync repository, the proposals), it never
follows a symbolic or hard link the sandbox could have planted. Every
file it reads there has a size limit, checked before the file is read,
and every directory it walks a limit on entries and depth, so a huge or
sparse file, or a deep tree, the sandbox made costs the host nothing: the
sandbox config over 256 KiB is not used (the last good copy stands in),
and a file over 16 MiB is not read when the host shows what a sync would
send (the sync itself runs in the sandbox, on the sandbox's own files).

### Sync

In effect, as [sync](sync.md) describes: the sync runs in the sandbox, a
remote is untrusted, the sandbox's git config is never read for it, git
hooks are off, and the Claude login never syncs. It cannot protect the
remote from the sandbox: a wipe in the sandbox can sync as deletions.
Protect the remote's branch (no force-push, no deletion) where it is hosted,
and recover from its history.

### Images and updates

In effect. caboose never installs packages into a base image of yours; an
`apko` base is built by apko, which runs no package's scripts on your
machine. Updates are checked against the release's checksums, which catches
a damaged download, not a release someone else published: releases are
not signed.

## Not covered

Stated plainly, so that nobody relies on them:

- **Kernel, hypervisor or engine escapes**: in the engine's Linux VM, in
  gVisor, in Virtualization.framework; side channels.
- **Everything in a writable part.** A writable part belongs to the
  sandbox: it can delete or corrupt all of it. Checkpoints, for the
  paths you list, recover it up to the last one; nothing else will.
- **The sandbox's home**: readable and writable by the agent, by design,
  logins included.
- **Files your tools run, in a writable repository**: `.envrc`,
  `.vscode/tasks.json`, a `Makefile`, `package.json` scripts, a
  repository's Claude Code settings and hooks, anything your tools execute.
  That includes a repository's git config and hooks, like any file a
  sandbox writes: they are not guarded, and they run on the host when the
  host's git uses them (a `core.fsmonitor` the sandbox writes runs at the
  host's next `git status`, a hook at the next commit or push). It also
  includes a caboose checkout in a writable part, which the host builds
  and runs ([caveats](caveats.md)).
- **`host_exec`**, when on: an escape by design. An alias that runs a
  project's files, at `fallible`: the same. And whatever an alias's own
  program reads or does is that program's: an alias for an interpreter
  you wrote into `config.toml` yourself runs whatever its arguments say,
  and a process it starts that leaves its process group outlives it. The
  writable set aliases are judged by covers the sandboxes of every
  environment under this `CABOOSE_HOME`; a sandbox some other caboose
  made, with another `CABOOSE_HOME`, is not in it.
- **Your SSH agent**: forwarded to the sandbox, which can sign and log in
  wherever your keys reach, as long as it runs. Under `trust = "hostile"`
  the sandbox gets no agent unless `ssh_agent` in `[link]` names one
  outright; one it names is carried into the VM unrestricted, and only
  where its ssh connections can go is decided by the proxy's rules
  (`ssh_agent = "none"` gives it none at either level). On a Mac with OrbStack or
  Docker Desktop, `container` and `gvisor` sandboxes get the engine's
  forwarded agent, whatever socket `ssh_agent` names; `ssh_agent =
  "none"`, or an `IdentityAgent none` for every host in `~/.ssh/config`,
  gives them none there too, from the next `caboose restart`.
- **GitHub and other tokens**: caboose provides none to the sandbox; a
  token you put there is the sandbox's to use, for anything it allows.
- **The network**: unrestricted under `container` and `gvisor`; under `vm`,
  the NAT beside the proxy at `fallible`, and at `hostile` only the proxy. A name in `egress_allow` reaches whatever it
  resolves to, private addresses included, so whoever controls that name's
  DNS chooses where it leads (DNS rebinding, which a grant, unlike
  `egress_allow`, does not let through). And no rule stops what goes out over a channel
  that is allowed: caboose limits where a sandbox may connect, not what it
  sends.
- **Confidentiality inside the roots**: read-only does not mean secret;
  everything under a root is readable.
- **Denial of service**: filling the disk, using the CPU and memory,
  flooding the link with requests, holding the web page's connections
  open (bounded, not prevented).
- **Another process of your own user**: it can talk to the daemon, and
  answer requests, as you; it can get the web page's token (from the
  browser, or the daemon's memory), and take the page's port while the
  daemon is not running.
- **Your own settings that widen the sandbox**: `engine_socket = true`
  (root on the host), `run_args` (caboose refuses only those that override
  its own flags and mounts or publish onto the web UI's ports; doctor
  reports `--network=host` as a problem), `host_exec`, wide `forward_ports`,
  `open_urls = "allow"`, a root holding `~/.caboose` or other secrets.
- **Time**: a grant for some hours is open for those hours, whatever the
  sandbox does with it.
- **Old agents and pending answers**: a sandbox whose image predates
  pending answers shows them as a refusal with the request's number; the
  request is filed all the same.
- **Unsigned releases**: an install trusts what the GitHub release holds.
- **Prompt injection**: caboose limits what an injected session can do; it
  does not detect injection, and an approval you give is yours.
- **A second name for a directory inside the checkpoint store, made by a
  mount**: a root or a sandbox's mount source is refused when it is, holds
  or is inside the store, judged by the identity of each directory from
  it up to `/`, so symbolic links, firmlinks and a case-insensitive
  spelling are all caught. A bind mount (or another filesystem that
  mirrors one) of a directory inside the store, mounted somewhere else,
  is a name that no walk up from it leads back to the store: a root or a
  mount source naming it is not refused. Do not mount any part of
  `~/.caboose` elsewhere.
- **A machine asleep, the engine down, the daemon stopped**: what the
  daemon does (the link and its requests, and the checkpoints) is not
  running then; what the mounts enforce still holds.
- **Linux hosts**: caboose is designed to work there, and nothing in this
  document has been tested on one.

## What you can do

- Keep `host_exec` off for any environment you leave unattended.
- Keep `engine_socket` off, and `run_args` empty (no `--network host`
  above all), unless you mean what they give.
- Keep the roots to what the sandbox needs, and `~/.caboose`, your home and
  other secrets out of every root; list as writable only the parts it has
  to change, and treat those as the sandbox's to delete.
- Keep a caboose checkout, and anything your own tools execute without
  asking, out of the writable parts, or accept that the sandbox can
  change it.
- Keep `forward_ports` to the ranges you develop on, and `open_urls` at
  `"ask"`.
- Keep tokens and credentials out of the sandbox's home; a push from the
  sandbox can go wherever your SSH agent's keys reach, so protect the
  branches that matter on the server (no force-push, no deletion).
- Read what a request or a proposal would do before you approve it, not
  the agent's account of it.
- List what the sandbox works on in `[checkpoints]`, and push often:
  what is neither pushed nor in a checkpoint exists only where the sandbox
  can delete it.
- `caboose status` and `caboose doctor` say how a sandbox is set up: the
  isolation, whether the engine socket or host commands are on, the SSH
  agent it gets.

## How to verify

A conformance suite is built. You run it on your own machine, from a
caboose checkout with Go (it is in no release), and never inside a sandbox:

    make conformance ISOLATION=container|gvisor|vm [TRUST=hostile] [OUT=FILE]

It prints its plan, what it will create and try, then creates a throwaway
environment under a temporary directory, with roots holding only files it made, and from inside it tries each operation a
control exists to refuse (writing a read-only root, following a link out of
it, reaching the daemon, running a host command without a grant), then
checks on the host that nothing outside the writable files changed. What
must be refused at the system call itself (a remount, an unmount) is
called by the suite's own static probe, which it places in the writable
part, never through whatever tools the image has. The probe also enters a
new user namespace (which a container's seccomp refuses and gVisor allows
an unprivileged process) and from inside it bind-remounts a read-only root
read-write, unmounts the part, and writes a read-only canary as a witness;
a mount still locked read-only refuses the write, and the suite reads the
witness on the host, so a write that landed is a failure. It
tries as the user the sandbox's sessions run as (root under vm, and under
gVisor where caboose runs it as root), and as root too where a check says
so. Each
check reports PASS when the operation is refused, FAIL when it is not, and
ERROR when it could not tell; a documented limit reports as a gap, and a
check that did not run as SKIP or INFO. The checks marked destructive (they
remove, move or unmount things, each on files of its own) run last, after
every other check, and none of them runs once any other check was an
ERROR. Last of all, on a freshly restarted
sandbox, comes the wipe (`R14`): one `rm -rf`, run as root behind the same
checks as every destructive command, of the suite's two roots and its own
files in the instructions mount, each named by its literal path, never
`/`, the home, `/work` itself or a pattern. Then, from the host, every file
the suite made outside the writable part (in the read-only root, beside the
part, in the instructions mount, and wherever no mount reaches) must be
there with the same contents and identity; only the writable part's may be
gone, and the suite checks that they are, so a wipe that did not run is no
pass. The wipe is skipped by `./caboose-conformance conformance run
--skip-wipe`, and once any earlier check,
destructive or not, was an ERROR. It leaves the sandbox broken on purpose:
the teardown removes it.

After an aborted run, `./caboose-conformance conformance leftovers` lists
what it left and the commands that remove it; a container or image named
like the suite's whose environment is one of yours, and no run's, is
listed with a note and no command. `./caboose-conformance
conformance matrix FILE` records a results file in the cells of the
[matrix](#the-matrix), each naming the day, the macOS version, the engine
and the caboose version it was tested with, and `conformance verify-doc`
checks that this document and the suite agree.

If you find a control described here as in effect that does not hold,
please report it privately to the maintainer, through the contact options
of the repository's GitHub page, rather than in a public issue.
