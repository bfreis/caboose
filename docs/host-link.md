# The host link: ports, URLs, notifications, file changes, outbound connections, host commands

A server the agent starts in the sandbox is reachable from your browser, at
the same port on `localhost`; `caboose-agent open URL` in the sandbox opens
a page in your browser; `caboose-agent notify TEXT` shows a notification;
and under gVisor, a file you save here is seen by the watchers in the
sandbox; and, where you turn it on, `caboose-agent host CMD` runs CMD on
your machine. All of it goes through the host link: `caboose-agent` in the
container, and `caboose link` on your machine.

## How it runs

Every launch starts `caboose link --background` for its environment, unless
one is already running. It runs `docker exec -i <container> caboose-agent
link`, speaks a small multiplexed protocol over that exec's stdin and
stdout, and reconnects if it drops. It ends by itself when the container
stops; the next launch starts it again. It logs to the data dir's
`link.log` (`~/.caboose/envs/default/data/link.log` for the default
environment). `caboose link`, run in a terminal, does the same in the
foreground, logging there.

Nothing on your machine listens for the container, and nothing of your
machine is mounted into it for this: the exec is the whole channel, so it
works the same on OrbStack, Docker Desktop and Docker on Linux.

It rereads `config.toml` by itself within a couple of seconds of a change:
new `forward_ports`, `open_urls`, `egress_*` or `host_exec` settings reconnect the link with them (forwarded
connections open at that moment are cut, and host commands running are ended), and an edit it cannot read is
logged and leaves the running settings in place. A `CABOOSE_` variable it
was started with still wins over the file. A launch whose settings differ
from the running helper's, a variable set differently in that shell for
instance, replaces the helper. `caboose link --restart` stops the running
helper and starts a new one in the background, whatever changed.

A container created by a caboose older than the link has no
`caboose-agent`; the link says so in its log and stops, and `caboose
restart` moves the container onto an image that has it.

## Ports

The agent watches the container's listening TCP ports, once a second. Each
one that `forward_ports` allows is forwarded to the same port on this
machine's `127.0.0.1`, while it keeps listening; a port already in use here
is skipped. A server bound only to `127.0.0.1` in the sandbox, as many dev
servers are by default, is forwarded too: the agent connects to it on the
container's own loopback.

```toml
forward_ports = "3000-3999 5173 8000-8999"   # the default
forward_ports = "none"                       # forward nothing
```

`caboose-agent ports`, in the sandbox, lists what listens and what came of
each: forwarded, not in `forward_ports`, or in use on the host.

## URLs and notifications

`caboose-agent open URL` opens an `http` or `https` URL in your default
browser. With `open_urls = "ask"`, the default, a dialog shows the URL and
asks first; `"allow"` opens without asking, `"off"` refuses every URL. Any
other scheme, a URL with a user or password in it, or one holding
unprintable characters is refused whatever the setting.

`caboose-agent notify [-t TITLE] TEXT` shows a notification. The text is
made printable and cut short first.

The link also notices, itself, when a session writes a new
[proposal](proposals.md), and shows a notification with its title and the
`caboose apply` that reviews it. It reads the proposals only as `caboose
apply` does, never through a symlink, and applies nothing.

On a Mac these use `open` and `osascript`; on Linux `xdg-open`,
`notify-send`, and `zenity` or `kdialog` for the dialog (with neither, set
`open_urls` to `"allow"` or `"off"`).

## File changes

Under `isolation = "gvisor"` or `"vm"`, an edit made on this machine under
the roots never becomes an inotify event in the sandbox: a dev server's reload, a
watch-mode test or a `--watch` build there would not see it. So the link
watches the roots the container mounts (FSEvents on a Mac, inotify on
Linux) and sends the agent the paths that changed. The agent sets each
one's mode to what it already is, which raises `IN_ATTRIB` inside and
leaves the file's contents and times alone; for a file deleted or renamed
away, it does that to its directory. Under `docker` the engine passes
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
4096 at once as the roots themselves, for watchers to look again. A file
the sandbox writes is seen here too, and relayed back, so a watcher there
sees that change twice. `link.log` says which roots it watches, or why it
cannot.

## Outbound connections

Under `isolation = "vm"`, the VM's traffic leaves through macOS's NAT,
which reaches none of the routes a VPN gives your Mac: a host your Mac
reaches over Tailscale or a company VPN times out from the VM. So, with
`egress_proxy = "on"` (the default), the link offers the agent an
outbound proxy, and each of the sandbox's connections through it is made
by `caboose link` on your Mac, with its own resolver and routes, as Docker
Desktop and OrbStack do for a container. Under `docker` and `gvisor` the
engine already does, and the setting is ignored.

```toml
egress_proxy = "off"                    # the VM's own NAT instead
egress_ports = "22 80 443"              # the default
egress_allow = "git.corp.example *.internal.example 10.20.0.0/16"
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
is refused whole, not dialled at the public one: that is how DNS
rebinding works. `egress_allow` lets through a name (or every name under
a `*.suffix`) whatever it resolves to, or an address in one of its CIDRs
-- with one exception: a loopback, unspecified (`0.0.0.0`, `::`) or
link-local address, or one of your Mac's own, is refused even for a name
it allows, since a pattern such as `*.nip.io` would otherwise reach your
Mac's loopback (`127.0.0.1.nip.io`). Only an address or CIDR within that
range lets one through (`127.0.0.1`, `169.254.169.254`), and only that
very address for one of your Mac's own. Only the addresses checked are
dialled; the name is never looked up again.

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
git print: 403 for a port or address it does not allow, 502 for a name
that does not resolve or a host that does not answer, 503 for too many
connections or a link that is down.
`caboose-agent connect HOST PORT` makes one connection through it, on its
stdin and stdout, as ssh's `ProxyCommand caboose-agent connect %h %p`.

What uses it, with `egress_proxy` on as the VM is created:

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
`egress_proxy` takes `caboose restart`, which a launch, `caboose status`
and `caboose doctor` say. `egress_ports` and `egress_allow` apply as the
link rereads them.

## Host commands

Off by default. With `host_exec = true` in `config.toml` (or
`CABOOSE_HOST_EXEC=1`), a session can run a command on this machine:

```sh
caboose-agent host make release           # in the host directory of the current one
caboose-agent host -C /work/site ls       # in another
caboose-agent host sh -c 'cd .. && ls'    # shell syntax takes a shell
```

The command runs as you, the user who ran `caboose`, with the environment
the link has: it was started from your terminal, so `PATH`,
`SSH_AUTH_SOCK` and the rest are yours. It runs in the host directory of
the sandbox's current one (or `-C DIR`'s), which must be under a root the
sandbox mounts: `/work/site` is wherever that root is here. Its stdin,
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
status` say when it is on. Sessions read whether it is on in their
`CLAUDE.md`, which a launch rewrites, so a session started before a change
still describes it as it was; the link itself follows `config.toml` within
seconds, as above.

## What the sandbox can and cannot do with it

Everything the agent sends is treated as untrusted, as everything from the
sandbox is:

- which ports forward is `forward_ports`, in the host's `config.toml`,
  which the sandbox cannot write; forwards listen on `127.0.0.1` only, so
  nothing else on your network reaches them; at most 64 ports at once;
- a forwarded port can squat one of your own services' ports while that
  service is down: a program here connecting to it would talk to the
  sandbox. Keep `forward_ports` to the ranges you use for development;
- requests are rate-limited, a URL opens only as the setting says, and a
  dialog is one at a time;
- the protocol is framed and bounded: a frame over 64KiB, a stream past
  its window (1MiB unread on the host, at most 256 streams), or any other
  violation ends the link;
- file changes go one way, from the host: the agent only touches paths
  under `/work` the host names, never through a symlink, and only regular
  files and directories;
- an outbound connection reaches only `egress_ports`, and only public
  addresses as your Mac resolves them, unless `egress_allow` says
  otherwise.
- a host command runs only when `host_exec`, in the host's `config.toml`,
  says so: the agent asking is not enough. It runs the request's own
  command line, through no shell, in a directory under a mounted root,
  with no terminal, as no other user, and with no variables of the
  sandbox's; and once it runs, it can do anything you can.
