# The host link: ports, URLs, notifications

A server the agent starts in the sandbox is reachable from your browser, at
the same port on `localhost`; `caboose-agent open URL` in the sandbox opens
a page in your browser; `caboose-agent notify TEXT` shows a notification.
All three go through the host link: `caboose-agent` in the container, and
`caboose link` on your machine.

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
new `forward_ports` or `open_urls` reconnect the link with them (forwarded
connections open at that moment are cut), and an edit it cannot read is
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

On a Mac these use `open` and `osascript`; on Linux `xdg-open`,
`notify-send`, and `zenity` or `kdialog` for the dialog (with neither, set
`open_urls` to `"allow"` or `"off"`).

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
  its window, or any other violation ends the link.
