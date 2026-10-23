# The host link: ports, URLs, notifications, file changes, outbound connections, host commands, aliases

A server the agent starts in the sandbox is reachable from your browser, at
the same port on `localhost`; `caboose-agent open URL` in the sandbox opens
a page in your browser; `caboose-agent notify TEXT` shows a notification;
and under gVisor and vm, a file you save here is seen by the watchers in
the sandbox; and, where you turn it on, `caboose-agent host CMD` runs CMD on
your machine. All of it goes through the host link: `caboose-agent` in the
container, and a link on your machine, run by the caboose daemon.

## How it runs

One daemon per `CABOOSE_HOME` runs the links of all your environments, one
each. Every launch asks it for its environment's link (`caboose link` does
too), and the daemon runs `docker exec -i <container> caboose-agent link`,
speaks a small multiplexed protocol over that exec's stdin and stdout, and
reconnects if it drops. A link ends by itself when its container stops; the
next launch starts it again. It logs to the data dir's `link.log`
(`~/.caboose/envs/default/data/link.log` for the default environment).

`caboose status` says how the daemon and the environment's link are
(`daemon` and `link` lines, and a `requests` line while any wait for an
answer), and `caboose doctor` checks that the daemon
answers, runs the same version as the `caboose` you typed, and holds a link
while the sandbox runs, naming the command that fixes each problem.
`caboose link` asks the daemon for the link now, `caboose link --restart`
has it replace the link whatever its settings, and `caboose link
--foreground` runs the link in the terminal instead, to debug it (it is
refused while the daemon serves the environment: `caboose daemon stop`
first).

### The daemon

It is started on demand: a launch, `caboose shell`, `caboose sync` or
`caboose link` starts it when nothing answers its socket, and `status` and
`doctor` only ask, never start it. Nothing is installed for it: no
LaunchAgent, no systemd unit. It stays while a sandbox it serves runs or a
command is connected, and exits five minutes after the last of them goes;
the next command starts it again, and it brings back the links of every
sandbox that runs by itself, so a daemon that crashes costs a few seconds
without links, not a launch. `caboose daemon status` says how it is,
`caboose daemon stop` ends it at once (its links stop, and the host
commands they started: a command gets SIGTERM, then SIGKILL, and the
stop waits up to 15 s for them, saying how many it is ending), and `caboose daemon restart` stops it and starts
another, returning once that one has brought the links back. Until a
daemon that has just started has, status and doctor wait a moment, then
say it is still bringing them back.

After `caboose update` (or the daily check) installs a new release, the
running daemon is asked to make way for it: it exits as soon as no host
command runs (a long one, such as a keep-awake command, makes it wait,
`caboose daemon status` says it is "replacing", and while it waits it
starts no new host command, so that new ones cannot hold it off), the new
release's daemon
starts, and the sandboxes' links come back by themselves. The version it
ran stays installed until it has gone. A caboose whose daemon protocol is
newer than the running daemon's replaces it at once, ending the host
commands it ran; an older caboose does not use a newer daemon and says to
update (`status` and `doctor` only say which is older). A newer caboose's
daemon refuses an older caboose's commands, `caboose daemon stop` and
`restart` included: update caboose, or end the daemon with `kill` (its pid
is in `daemon.json`).

The daemon also answers `caboose requests`, `grants` and `permissions`
(see [Requests and grants](security.md#requests-and-grants)), for the
environments whose link it runs. A daemon older than your `caboose` does
not know those commands and says so: `caboose daemon restart`. With
`CABOOSE_DATA_DIR` set the daemon does not serve the command's
environment, so these commands find nothing to answer for.

The daemon can also stop or restart a sandbox for a client that asked you
first (the menu-bar app; the web page cannot). It runs `caboose stop` or
`caboose restart` as a job of the environment's, one at a time (a second
is refused while one runs), and answers at once: a restart may build the
image first, for minutes. The job takes the sessions it ends as
confirmed; any other question fails it, with the command to run on a
terminal instead. What the command would print goes to `jobs.log` in the
environment's directory (`~/.caboose/envs/ENV/jobs.log`, capped, in no
mount), `caboose daemon status` and the app see the job for ten minutes
after it ends, and its end is a notification. The link follows the
sandbox: a stopped one's stops, a recreated one's starts again. A
packages proposal applied from the web page is a job of the same kind
(see [proposals](proposals.md)): one at a time with a stop or restart,
its build's output in `jobs.log`, its end a notification. Nothing
locks a sandbox's lifecycle beyond that: a terminal's `caboose restart`
at the same moment as a job races with it, as two terminals' do. An
update replacing the daemon waits for a running job as it waits for host
commands, and starts no new one meanwhile.

Its files are in `CABOOSE_HOME/daemon/` (`~/.caboose/daemon/`), a directory
only you can enter, and in no mount of any sandbox: `daemon.sock` (the
socket, which takes connections from your user only), `daemon.lock`,
`start.lock`, `daemon.json` (its pid, version and start time),
`daemon.log`, and `replaced.json` once an update has replaced a daemon
(the version that replaced it, so that nothing starts the older one
again). A `caboose` built from a checkout runs a daemon of its own in
`CABOOSE_HOME/daemon-dev/`, and serves only the environments it launches.
A Unix socket's path is limited to about a hundred bytes; with a long
`CABOOSE_HOME`, `caboose doctor` says to shorten it.

The daemon is started with a minimal environment, never your terminal's:
it asks a login shell (`$SHELL -l`) once, when it starts, for `PATH`,
`SSH_AUTH_SOCK`, `DOCKER_HOST`, `DOCKER_CONTEXT` and `DOCKER_CONFIG`. So it
finds `docker` and your SSH agent the same way whichever terminal or app
started it, and a variable you exported only in the terminal you launched
from does not reach it (see [Host commands](#host-commands) for what that
means there). The SSH agent a `vm` sandbox gets is `ssh_agent` in `[link]`
if set, else the one ssh would use, found from that login shell.

A command run with `CABOOSE_DATA_DIR` set is not the daemon's, which serves
each environment at its default data dir only: its launches keep starting a
detached `caboose link --background`, logging to that data dir's
`link.log`. The daemon never takes over such a helper's link, nor stops it.

Nothing on your machine listens for the container, and nothing of your
machine is mounted into it for this: the exec is the whole channel, so it
works the same on OrbStack, Docker Desktop and Docker on Linux.

It rereads `config.toml` by itself within a couple of seconds of a change:
new `[link]` settings, or a `vm` profile's `egress_*`, reconnect the link with them (forwarded
connections open at that moment are cut, and host commands running are ended), and an edit it cannot read is
logged and leaves the running settings in place. A launch whose settings differ
from the running link's replaces the link. `caboose link --restart` replaces it whatever
changed.

## Ports

The agent watches the container's listening TCP ports, once a second. Each
one that `forward_ports` (in `[link]`) allows is forwarded to the same port on this
machine's `127.0.0.1`, while it keeps listening; a port already in use here
is skipped. A server bound only to `127.0.0.1` in the sandbox, as many dev
servers are by default, is forwarded too: the agent connects to it on the
container's own loopback. Nothing listens on this machine for a port
nothing listens on in the sandbox, so a wide range costs nothing until a
program binds one of its ports.

```toml
[link]
forward_ports = ["3000-3999", 5173, "8000-8999", "32768-60999"]   # the default
forward_ports = []                                                # forward nothing
```

The last range is Linux's ephemeral one, where a program listens when it
asks for any free port, as a login does for its loopback callback
(`redirect_uri=http://127.0.0.1:37959/callback`): the browser on this
machine is sent there, and finds the sandbox. It also forwards whatever
else listens there, test servers and debuggers included, while it does.
Under gVisor the sandbox picks such ports from 16000-65535 instead, so a
callback below 32768 or above 60999 is not forwarded unless you add those
ports; under docker and vm it is Linux's range.

Ports 7171 and 7172 are never forwarded, whatever `forward_ports` says
(`config.toml` refuses a set that holds them): they are caboose's web UI
on this machine, 7171 a release's and 7172 a checkout's build's, which
approves what a sandbox asks for.

`caboose-agent ports`, in the sandbox, lists what listens and what came of
each: forwarded, not in `forward_ports`, the web UI's port, or in use on
the host.

## URLs and notifications

`caboose-agent open URL` opens an `http` or `https` URL in your default
browser. With `open_urls = "ask"`, the default, the URL is a request a
person answers on the host: `caboose-agent open` returns at once, refused
for now with `pending #N`, and a dialog on this machine shows the
environment and the URL as the host checked it (one dialog at a time,
whichever environment asks). Open allows it, and the host opens the URL
then; Cancel denies it. A dialog that goes away unanswered (the link or
caboose's daemon stopped while it showed) leaves the request denied, "ask
again": it is never asked a second time another way, and the sandbox asks
again if it still wants the URL. A dialog that cannot be shown leaves the
request pending; an unanswered one expires after a day, denied. `caboose requests`
answers it too (and shows what is waiting), and `caboose requests approve N
--always` adds the URL's origin to `open_urls_allow` in `config.toml`, after
showing you the change. `"allow"` opens
without asking, and so does an origin in `open_urls_allow`; `"off"`
refuses every URL. Under `trust = "hostile"` every URL asks, whatever
`open_urls` and `open_urls_allow` say (`"off"` still refuses). Any other
scheme, a URL with a user or password in it, or one holding unprintable
characters is refused whatever the setting, and so is any URL to port 7171
or 7172, whatever host it names: those are the host's caboose web UI, which
a sandbox never opens (a request to open one is refused, never filed).

Programs open a browser the usual way, and it ends there. The layer puts
an `xdg-open` in `/usr/local/bin` and sets `BROWSER` to it, so `gh`, Go's
`pkg/browser`, Python's `webbrowser` and npm's `open` all reach
`caboose-agent open`, and a login flow opens your browser. The sandbox has no
display, so this one is meant to win over a base's own `xdg-open` in
`/usr/bin`, which would start a text browser in the terminal; one in
`/usr/local/bin` is replaced, like `caboose-agent`. A file path or a URL
that is not `http` or `https` is refused with exit 1, as `caboose-agent open`
refuses it; a host that refuses, or none linked, exits 4. A login that
sends the browser back to a loopback port reaches the sandbox through the
ephemeral range in `forward_ports` ([Ports](#ports)).

`caboose-agent notify [-t TITLE] TEXT` shows a notification. The text is
made printable and cut short first.

The link also notices, itself, when a session writes a new
[proposal](proposals.md), and shows a notification with its title and the
`caboose apply` that reviews it. It reads the proposals only as `caboose
apply` does, never through a symlink, and applies nothing.

On a Mac these use `open` and `osascript`; on Linux `xdg-open`,
`notify-send`, and `zenity` or `kdialog` for the dialog (with neither, a
request waits unanswered: set `open_urls` to `"allow"` or `"off"`).

## File changes

Under a `gvisor` or `vm` profile, an edit made on this machine under
the roots never becomes an inotify event in the sandbox: a dev server's reload, a
watch-mode test or a `--watch` build there would not see it. So the link
watches the writable parts of the roots the container mounts (a whole
root where all of it is writable; FSEvents on a Mac, inotify on Linux)
and sends the agent the paths that changed. The agent sets each
one's mode to what it already is, which raises `IN_ATTRIB` inside and
leaves the file's contents and times alone; for a file deleted or renamed
away, it does that to its directory. Under a `container` profile the engine passes
events on itself, and the link relays nothing.

Watchers that take any event on a file as a change see it: Node's
`fs.watch` and the tools built on it, for instance. One that ignores
attribute changes does not: Go's fsnotify reports them as `Chmod`, which
some tools skip. For those, the tool's polling mode, where it has one,
works under any isolation.

Limits: a deleted file raises nothing itself, so a watcher sees its
directory change, not the file's name (tools that look at the directory
again when it changes notice); nothing in a `.git` directory is relayed; a burst of more than 64
changes in one directory is relayed as that directory, and of more than
4096 at once as the watched directories themselves, for watchers to look
again. The read-only rest of a root is not relayed: the agent's touch
would fail on a read-only mount, so a watcher there needs its polling
mode. A file
the sandbox writes is seen here too, and relayed back, so a watcher there
sees that change twice. `link.log` says which roots it watches, or why it
cannot.

## Outbound connections

Under `trust = "hostile"` the VM is created without the NAT described next: the proxy is the only way out, and it is always on ([trust](configuration.md#trust)).

Under a `vm` profile, the VM's traffic leaves through macOS's NAT,
which reaches none of the routes a VPN gives your Mac: a host your Mac
reaches over Tailscale or a company VPN times out from the VM. So, with
`egress = true` (the default), the link offers the agent an
outbound proxy, and each of the sandbox's connections through it is made
by `caboose link` on your Mac, with its own resolver and routes, as Docker
Desktop and OrbStack do for a container. Under `container` and `gvisor` the
engine already does, and there is no such setting.

```toml
[vm.default]
egress = false                         # the VM's own NAT instead
egress_ports = [22, 80, 443]           # the default
egress_allow = ["git.corp.example", "*.internal.example", "10.20.0.0/16"]
```

The sandbox sends a name and a port, never an address. The port must be
in `egress_ports`. Your Mac resolves the name, and every address it gets
must be public: loopback, private (RFC 1918, IPv6 ULA), link-local,
shared (`100.64.0.0/10`, which holds tailnet peers), the VM network's own
(`192.168.64.0/24`), multicast and reserved addresses are refused, in
their IPv6 forms too (IPv4-mapped, IPv4-translated, NAT64's well-known
and local-use prefixes, 6to4), and so are your Mac's own addresses, as
its interfaces list them (a public IPv6 address, a VPN's, re-read every
few seconds). A name that resolves to a public address and a refused one
is not dialled at the public one: that is how DNS rebinding works.
`egress_deny` refuses a name (or every name under a `*.suffix`), or an
address in one of its CIDRs, whatever else says; it beats every allow.
`egress_allow` lets through a name (or every name under a `*.suffix`)
whatever it resolves to, or an address in one of its CIDRs -- with one
exception: a loopback, unspecified (`0.0.0.0`, `::`) or link-local
address, or one of your Mac's own, is refused even for a name it allows,
since a pattern such as `*.nip.io` would otherwise reach your Mac's
loopback (`127.0.0.1.nip.io`). Only an address or CIDR within that range
lets one through (`127.0.0.1`, `169.254.169.254`), and only that very
address for one of your Mac's own. Ports 7171 and 7172 on any of those
addresses are refused even then, whatever `egress_allow`, `egress_ports`,
an answer or the trust level say: they are caboose's web UI. Only the
addresses checked are dialled; the name is never looked up again.

A name that resolves to a private address `egress_allow` does not name
is a request a person answers on the host, as is, under
`trust = "hostile"`, every host `egress_allow` does not name: the
connection is refused for now, with `pending #N` in the proxy's answer
(a 403), and goes through once the request is allowed. An answer given
for a time lets the name through as it resolved when the request was
filed: should it later resolve to a private address the answer did not
see, it asks again. A loopback, link-local or own address never asks:
it is refused. `caboose requests` lists them and answers them: allow once,
for some hours (`--for 8h`), or always, which adds the name to
`egress_allow` of the `vm` profile in use in `config.toml` after showing
you the change; unanswered, they expire a day later, denied.

What it cannot tell: your Mac's public IPv4 address behind a router's
NAT is on no interface of the Mac, so the router's WAN address, which
some routers hairpin back to the Mac's forwarded ports, is not refused;
nor is a network-specific NAT64 prefix (RFC 7050) unwrapped, so an
address under one is judged as the IPv6 address it is.

At most 128 connections are open at once, new ones open at most 20 a
second (in bursts of 100), a connect gives up after 10 seconds (shared
among a name's addresses, each getting at least 2, so one that never
answers leaves time for the next), and a connection nothing crosses for
15 minutes is closed. `link.log` notes each refusal and failed connect,
by host and port, at most 10 a minute and then how many more there
were, and an hourly count of connections by host and port; never what
they carry. Each link appends to `link.log`, which at 10 MiB is moved to
`link.log.1`, replacing the one before.

In the sandbox, `caboose-agent` serves the proxy on `127.0.0.1:9128` while
the link is up, and leaves that port out of what it forwards. It takes
`CONNECT host:port`, for HTTPS and any other TCP, and plain `http://`
requests, keeping a client's connection, and its way to the server, from
one request to the next while they go to the same host and port, for at
most 90 seconds between two. Requests a client sends without waiting for
the answers, as apt does, go on to the server the same way, up to 10 at
once when they are a GET, HEAD, OPTIONS or TRACE without a body, and are
answered in order. It parses HTTP and sends the name and port on; it
never resolves or dials anything itself. A connection the host refuses
is answered in HTTP, with the host's reason as the body, which curl and
git print: 403 for a port or address it does not allow, or one that waits
for a person's answer (`pending #N`), 502 for a name
that does not resolve or a host that does not answer, 503 for too many
connections or a link that is down.
`caboose-agent connect HOST PORT` makes one connection through it, on its
stdin and stdout, as ssh's `ProxyCommand caboose-agent connect %h %p`.

What uses it, with `egress` on as the VM is created:

- every process in the VM has `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`
  and `https_proxy` set to `http://127.0.0.1:9128`, and `NO_PROXY` and
  `no_proxy` to `localhost,127.0.0.1,::1,.localhost,172.16.0.0/12` and
  the VM's hostname -- what is the VM's own, its dockerd's bridges among
  it (a client that reads no CIDR there, as wget, still sends a bridge's
  address to the proxy, which refuses it, and so does any client with a
  single-label name, as compose's services have, which NO_PROXY has no
  way to say): the entrypoint (its first
  install of Claude Code waits up to a minute for the link, which a launch
  starts as the VM boots), sessions, `caboose shell`, and the VM's own
  dockerd, whose pulls go through it. The containers that dockerd runs get
  none of it and stay on the VM's NAT;
- ssh, through a drop-in the agent writes at every boot,
  `/etc/ssh/ssh_config.d/50-caboose-egress.conf`: `Host * !localhost
  !*.localhost !127.* !::1`, the VM's hostname and `!172.16.*` to
  `!172.31.*`, with `ProxyCommand /run/caboose/agent connect %h %p`
  (the agent the VM booted with). ssh runs a `ProxyCommand` through a
  shell with the host name in it, which an OpenSSH before 9.6 lets a
  hostile name (a git submodule's URL) inject commands into
  (CVE-2023-51385): so the agent sets ssh up only when `ssh -V` says 9.6
  or later, or Debian 12's (`deb12u2` on) or Ubuntu 22.04's
  (`3ubuntu0.6` on) build with the fix; any other ssh keeps the VM's own
  network, and the VM's console (`caboose logs`) says why. ssh reads
  `~/.ssh/config` first and keeps an option's first value, so a `ProxyCommand` or `ProxyJump` of yours for
  a host, or `ProxyCommand none`, wins. An image whose
  `/etc/ssh/ssh_config` includes no `ssh_config.d/*.conf` gets
  `GIT_SSH_COMMAND` instead, unless it sets one: git's ssh goes through
  the proxy, a plain `ssh` does not;
- `caboose build` (and `caboose check-image`) link the builder VM for the
  build's duration: its dockerd's pulls go through the proxy, the image
  check runs with the variables, and each `docker build` runs on the
  builder's own network (`--network=host`, where the proxy is) with the
  variables as build arguments, which Docker keeps out of the image and its
  cache keys. Refusals are said among the build's output.

The environment is fixed when the VM is created: a change to
`egress` takes `caboose restart`, which a launch, `caboose status`
and `caboose doctor` say. `egress_ports`, `egress_allow` and
`egress_deny` apply as the link rereads them.

## Host commands

Off by default. With `host_exec = true` in `[link]` of `config.toml`, a session can run a command on this machine
(never under `trust = "hostile"`, which refuses it whatever `host_exec`
says, and whose link does not offer it):

```sh
caboose-agent host make release           # in the host directory of the current one
caboose-agent host -C /work/dev/site ls     # in another
caboose-agent host sh -c 'cd .. && ls'    # shell syntax takes a shell
```

The command runs as you, the user who ran `caboose`, with exactly three
variables: `PATH`, `HOME` and `LANG`. Nothing else of the link's
environment reaches it, so a variable you exported only in a terminal
(an agent socket, a token) is not there; `env NAME=VALUE CMD` sets one.
`PATH` is the daemon's, which a login shell gave it when it
started (`$SHELL -l`, bounded to five seconds; if that fails the daemon
keeps the `PATH` it has and says so in its log), so a `PATH` change in your
shell's startup files reaches new commands after `caboose daemon restart`. It runs in the host directory of
the sandbox's current one (or `-C DIR`'s), which must be under a root the
sandbox mounts: `/work/dev/site` is wherever that root is here. Its stdin,
stdout and stderr are the sandbox command's, and its exit status is
`caboose-agent host`'s; 127 is a command not found in your `PATH`, 126 one
that could not run, or a refusal (said on stderr). There is no shell, so
no `*`, `|` or `&&` unless you run one, and no terminal: an interactive
program that needs one does not work. Interrupting `caboose-agent host`, or
anything that ends it (a tool's timeout), ends the command on this machine
and whatever it started (its process group, sent `SIGTERM`, then `SIGKILL`
three seconds later), as does the link's own end. At most 16 run at once.
Each one is a line in `link.log`: the command, where, its exit status and
how long it took.

**This is a hole in the wall, on purpose.** With it on, sessions in the
environment, and whatever steers them -- a web page they read, a repo they
work on, a dependency's install script -- can run anything on this machine
as you: read your files, use your SSH agent and your logins, change what
they like. It is meant for an environment whose point is a separate Claude
login, tools or network, not containment. `caboose doctor` and `caboose
status` say when it is on. Sessions read whether it is on in caboose's
instructions (`/etc/claude-code/CLAUDE.md`), which a launch rewrites, so a session started before a change
still describes it as it was; the link itself follows `config.toml` within
seconds, as above.

## Aliases

An alias is a host command `config.toml` names (`[alias.NAME]`,
[configuration](configuration.md#aliasname)), which a session runs by name,
whether or not `host_exec` is on:

```sh
caboose-agent aliases                 # names, descriptions, parameters
caboose-agent run awake &             # until the job ends
caboose-agent run test suite=unit     # a parameter, from the alias's list
```

The sandbox sends a name and `KEY=VALUE` arguments, never a command: the
host looks the alias up, puts the values in, resolves its command, its
directory and its absolute arguments on this disk, and decides, as it
decides every crossing (`trust`, the alias's `policy`, a grant). A command
within, or reached through, anything a sandbox can write is refused. What
runs is the path that was decided, as you, with `PATH`, `HOME` and `LANG`,
no shell and no terminal, in the alias's `cwd` or a fresh empty directory
removed afterwards; stdin, stdout and stderr pass through, and its exit
status is `caboose-agent run`'s (75 pending, 77 refused, 127 no such alias,
126 not runnable here, when it never ran). It ends when `caboose-agent run`
does, at its `timeout`, and with the link, and so does what it started.
An alias with `policy = "ask"` is refused as `pending #N` at once, and
runs when asked again once you allow the request (`caboose requests
approve N`, `--for 8h`, or `--always`, which writes `policy = "allow"`).
Each run is a line in `link.log`, by name and arguments; each decision is
in `decisions.log`. At most 16 aliases and host commands run at once, and a
sandbox that asks too often is told to try again shortly.

## Checkpoints

`caboose-agent checkpoint [PATH...]` has the daemon take a checkpoint now
of paths [`[checkpoints]`](checkpoints.md#asked-for-by-the-sandbox) lists,
as `checkpoint_requests` says (`"allow"` by default, or `"off"`). The host
offers it in its hello only from a link the daemon runs; the agent says so
when the host offers none.

## Asking the host ahead of time

A crossing that asks is refused as `pending #N` at once (above), and the
request it files carries only caboose's own description of what allowing it
does. A session can also say why, and what it would do meanwhile, by filing
the request itself:

```sh
caboose-agent request egress --host registry.example.com --port 443 \
  --context "installing the project's dependencies" --recommend "allow for 8 hours" \
  --meanwhile "writing the tests"
caboose-agent request open_url --url https://example.com/login
caboose-agent request alias --alias test --arg suite=unit
caboose-agent requests --all          # what the host decided, and what waits
```

`request` never waits: it exits 0 when the host allows it, 75 and prints
`pending #N` when it waits for a person (`caboose requests` on this
machine), 77 when it is denied, 1 when it could not ask, 2 for a mistake in
the command. `--session` (default `$CLAUDE_CODE_SESSION_ID`) says which
Claude Code session asks; `--json` prints a result a program can read.
`requests` lists the host's description of each request, with its state and
the reason for a denial, and a cursor: `--since N` lists only decisions after
cursor `N` (`--since now` prints the current cursor), and `--wait SECONDS`
(at most 300) waits for a new one. At most 16 waits are open on a link at
once, and a wait whose client has gone away (the mod's, when its session is killed) holds its slot until it times out, at most 300 seconds; a sandbox that asks too often is told to try again shortly. Both need a
host that files requests; an older one is refused with a message saying so.

Under `vm` and the other isolations alike, Claude Code loads a small mod of
caboose's, from `/etc/claude-code/mods/caboose`, through
`CLAUDE_CODE_PLUGIN_DIRS`, which a launch sets in the sandbox's environment
(a change takes `caboose restart`, as every setting fixed at creation does,
and `run_args` cannot set it). The mod gives the model two tools,
`caboose_request` and `caboose_requests`, which run the commands above, and a
background wait that says to the session, as a message from the plugin, what
the host decided on its own requests. It informs only: the host enforces every
decision whether the mod is loaded or not. A setting's `env` block that
replaces `CLAUDE_CODE_PLUGIN_DIRS` drops the mod from that session. To
count, the mod's wait tells the link which Claude Code process it runs under;
the link counts the sandbox's Claude Code processes and sends the host both
numbers when they change (at most every 5 seconds), and `caboose permissions`
shows "N Claude Code sessions, M without caboose's mod". That count is the
sandbox's own word, shown to you and used for nothing else.

## What the sandbox can and cannot do with it

Everything the agent sends is treated as untrusted, as everything from the
sandbox is:

- which ports forward is `forward_ports`, in the host's `config.toml`,
  which the sandbox cannot write; forwards listen on `127.0.0.1` only, so
  nothing else on your network reaches them; at most 64 ports at once;
  the web UI's ports, 7171 and 7172, never forward, a URL to either never
  opens, and the outbound proxy never reaches either on this machine;
- a forwarded port can squat one of your own services' ports while that
  service is down: a program here connecting to it would talk to the
  sandbox. Keep `forward_ports` to the ranges you use for development,
  and the ephemeral range if you log in from the sandbox;
- requests are rate-limited, a URL opens only as the setting says, and a
  dialog is one at a time; a crossing that asks a person is refused at
  once as pending, never waited on, and nothing the sandbox sends
  answers it: only the host does;
- the protocol is framed and bounded: a frame over 64KiB, a stream past
  its window (1MiB unread on the host, at most 256 streams), or any other
  violation ends the link;
- file changes go one way, from the host: the agent only touches paths
  under `/work` the host names, never through a symlink, and only regular
  files and directories;
- under `vm`, an outbound connection made through the proxy reaches only
  `egress_ports`, and only public addresses as your Mac resolves them,
  unless `egress_allow` says otherwise (or a person allows the request it
  filed), never what `egress_deny` names. A program that ignores the proxy
  settings leaves through the VM's NAT instead (except under `trust = "hostile"`, where the VM has no NAT and reaches nothing that way), and under `container` and
  `gvisor` the engine's network is unrestricted;
- a checkpoint the sandbox asks for copies only a path `[checkpoints]`
  lists, resolved on the host; the paths it sends are bounded, must be
  printable, absolute and clean (or `~/...`), and any other is refused;
- a host command runs only when `host_exec`, in the host's `config.toml`,
  says so, and never under `trust = "hostile"`: the agent asking is not
  enough. It runs the request's own
  command line, through no shell, in a directory under a mounted root,
  with no terminal, as no other user, and with no variables of the
  sandbox's; and once it runs, it can do anything you can;
- an alias runs only what `config.toml` says, with the parameters it lists:
  the sandbox names it and picks values, and the host resolves and decides
  the rest at every run.

This is the link's part of the picture. What the sandbox can reach as a
whole, and which controls are enforced and under what conditions, is in
[security](security.md).
