# The web page

caboose's daemon serves a page on this machine where you can see what the
sandboxes asked for and answer, and see the proposals waiting for
[`caboose apply`](proposals.md). `caboose ui` opens it.

```
caboose ui                 # open the page
caboose ui requests        # open the requests; also: proposals
caboose -e work ui         # open environment work's page (or: caboose ui work)
caboose ui --print         # print the address instead of opening it
caboose ui --new-token     # replace the token first
```

`caboose ui` starts the daemon when none runs, asks it for the page's
address, and opens that in your default browser. If the daemon does not
serve the page (another process holds the port), it says so and opens
nothing: opening the address then would hand its code to whatever holds
the port. Run `caboose status` to see the page's state on its `web` line,
and `caboose doctor` for the listener.

## The address

The page is at `http://127.0.0.1:7171/`, and a checkout's build of
caboose, which runs a daemon of its own, uses `http://127.0.0.1:7172/`. The
port is fixed on purpose: the browser keeps the page's token per origin,
port included, so a fixed port is what lets it survive the daemon
restarting. The daemon listens on `127.0.0.1` only, never on another
address, and never on another port: if 7171 is taken, the daemon runs
without the page and tries again every 30 seconds. To see what holds the
port, run `lsof -nP -iTCP:7171 -sTCP:LISTEN` on a Mac or `ss -ltnp` on
Linux.

Neither port can be forwarded, opened or reached from a sandbox through
caboose: `forward_ports` refuses them, `open_urls` never opens a URL to
them, the `vm` outbound proxy never reaches them, and `run_args` refuses a
`--publish` onto them. See [the host link](host-link.md#ports).

## The token

Every call the page makes carries a token, 32 random bytes that the daemon
makes anew every time it starts and keeps in its memory alone, never on
disk, so no sandbox can read it and a token taken from before lets nothing
in once the daemon is back. The page's own files load without it and hold
nothing secret. The token is the boundary against other local processes, and
against a sandbox that can reach your machine's loopback (under `container`
and `gvisor` on OrbStack or Docker Desktop): without the token it can load
the page, and nothing else.

The token is never in an address. `caboose ui` (and the menu-bar app)
opens `http://127.0.0.1:7171/#code=...`, with a one-time code the daemon
makes for that one open: it is good once, for 60 seconds, kept only in the
daemon's memory, and gone when the token is replaced. The page removes the
code from the address bar before it sends anything, then trades it for the
token (`POST /api/redeem`, which passes the same checks as every other
call). It keeps the token in the tab's `sessionStorage`, which no other
tab reads and which goes when the tab is closed, never in the origin's
lasting storage, and sends it as a header on every request; it sets no
cookies. A reload of the tab keeps the token; a new tab, or the same page
after the daemon started again, needs `caboose ui` again. Before
every request that sends the token, it asks the server to prove it knows
it, with a fresh challenge each time, so a process that took over the port
while the daemon was down is not handed it.

A code that is used, out of date or not the daemon's opens nothing, and
the page says to run `caboose ui` again (a page that already holds the
token goes on with it). The daemon gives every such code the same answer,
and limits redeems to one a second across every caller.

What that leaves exposed, which you should know before sharing a screen:

- While `open` (macOS) or `xdg-open` (Linux) runs, the address, code
  included, is in its arguments, which another process of yours can read.
  One that reads it and redeems it before the page does gets the token;
  the page then says its code is used. The browser's history keeps the
  address too, but the code in it is spent within seconds and dead within
  a minute.
- A process that binds the port in the instant between the daemon
  answering a proof and the request that follows it can receive that one
  request with the token. Reloading the tab while such a process holds
  the port (the daemon down, say) loads its script, which can read the
  token that tab kept: but that token died with the daemon, which makes
  a new one when it starts again, so it lets nothing in. Only while the
  daemon runs on is a token so taken still good.

If any of that may have happened, run `caboose ui --new-token`: it replaces
the token at once, and every code not used yet with it; a daemon that
starts again does the same. Every page open with the old token sends
nothing more and says that its token is out of date, or that something
else answers on the port (from the page's side the two look the same),
and to run `caboose ui` again, which opens a page with the new one.
`caboose ui --print` prints the address for a machine with no browser; it
opens the page once, within 60 seconds.

## What the page does

It lists the requests the sandboxes filed (a URL to open, a connection
through the `vm` proxy, a port to forward, an alias whose policy is `"ask"`) with what
allowing each would do, and answers them once, for hours or days, or
always; the "always" answer shows the exact change to `config.toml` and
makes it only if the file is unchanged. When that change cannot be shown
exactly as it would be written (too long, or a line holding a tab or a
character the page shows otherwise), "always" is not offered, and the
other answers still are. It lists the proposals waiting for
`caboose apply`, each with the change it makes, and deletes or applies one
after you confirm the whole change again, a root or an alias by typing its
name, a writable part by typing its root's name, as `caboose apply` asks
(a proposal that changes more than one thing is shown with the `caboose
apply` command that applies it in a terminal; the one exception is
packages with a root, which the page applies); a proposal is applied only if nothing it
changes moved since you saw it, and the image is rebuilt at the next
`caboose restart`. A proposal of packages is applied with **Build and
apply**: the daemon builds the image first, as a job of the environment's,
and the page shows the build's last lines while it runs, then what was
written, or why nothing was (a list that does not build, or a change to
the proposal or `config.toml` while it built); the sandbox keeps its image
until its next restart. It shows the daemon's and links' state. Everything the
sandbox wrote is shown as text, never as markup.

Each environment has a page of its own, `#/env/NAME` (the overview links
to it): what its link allows, as `caboose permissions` says (with a note
when the running sandbox has other roots or trust than `config.toml`,
which take effect at its restart); its grants,
each of which you can revoke after confirming; under `vm`, the outbound
proxy's `egress_allow` and `egress_deny`, where you add or remove an entry
after seeing the edit to `config.toml` it makes (written only if the file
is unchanged); its checkpoints, with the `caboose checkpoint restore`
command to copy; details as `caboose status` gives them (isolation, image,
sessions, resources, forwarded ports, kept paths); and the last lines of
its link log.

Its doctor page, `#/env/NAME/doctor`, runs `caboose doctor`'s checks for
that environment when you ask and shows the findings. They run in the
daemon's environment, which can differ from a terminal's (`PATH`, the SSH
agent), so the page also gives the command to run them from a terminal.

It works in the current Safari, Chrome and Firefox. A browser without
`crypto.subtle` cannot verify the server, so the page refuses to send the
token there.

Some things stay in the terminal: restoring a checkpoint, and restarting
the sandbox.

## Risks you accept

- **The page can be locked out.** A sandbox that can reach your machine's
  loopback can hold the page's 64 connections open, and keep doing so, to
  deny you the page. Nothing is granted by that, and the command line talks
  to the daemon over its socket and is unaffected: use `caboose requests`
  and `caboose apply` meanwhile.
- **The page can be kept from connecting.** The daemon answers at most
  20 proofs a second, across every page and process, since a proof needs
  no token. Anything that can reach your loopback, a sandbox included, can
  spend them all; the page then says the daemon is refusing proofs, sends
  nothing, and tries again on its own, waiting longer each time up to 30
  seconds. After five refusals in a row it adds that something else may
  answer on its address and refuse every proof, as a program holding the
  port while the daemon is not running would. Nothing is granted by that
  either, and the command line is unaffected.
- **Some error replies carry none of the page's headers.** A request so
  malformed that Go's HTTP server refuses it before caboose sees it (400),
  or whose headers are over the limit (431), is answered by that server in
  plain text, without the security headers every other response carries.
  Such a reply holds nothing of caboose's.
- **`--network host` in `run_args` is a problem.** On the host's network a
  sandbox can listen on port 7171 while the daemon is not running, and
  reach everything on your loopback. `caboose doctor` reports it as a
  problem and `caboose ui` warns about it; remove the argument.
