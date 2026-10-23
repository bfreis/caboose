# The menu-bar app

On a Mac, caboose has a menu-bar app, Caboose.app: an icon in the menu bar
whose menu shows each environment, the requests waiting for you, and the
proposals waiting for [`caboose apply`](proposals.md). It is one more
client of caboose's daemon, beside the CLI and the [web page](web-ui.md),
and it is optional: everything it does can be done with `caboose requests`,
`caboose ui` and the rest. Releases do not ship it; you can build it
from a checkout ([below](#building-it-from-a-checkout)).

## Starting and quitting

Opening Caboose.app (from Finder, `open ~/.caboose/app-dev/Caboose.app`,
or its login item) runs `caboose app`, the binary inside the bundle. The app starts the daemon
when none runs, as a launch or `caboose ui` does. It never replaces a daemon
that runs: when caboose was updated and its daemon is newer than the app,
the menu says "caboose is newer than this menu-bar app: update the app, or
quit it"; when the app is the newer one, it asks the older daemon to make
way once its host commands have ended, never killing them.

Only one app runs at a time: a second one says "Caboose is already running
in the menu bar" and exits once you close that alert (or two minutes
after, if nobody does). Quit ends the app only; the daemon goes on, and
exits on its own once nothing else needs it. While the app is not running
(or before it attaches), notifications and the question about opening a URL
come from macOS's `osascript`, as they do without the app.

An app started from Finder or at login gets the default `CABOOSE_HOME`
(`~/.caboose`). If you use another, start it from a terminal that sets it:

```
CABOOSE_HOME=/path/to/home ~/.caboose/app-dev/Caboose.app/Contents/MacOS/caboose app
```

The app runs as you, on the Mac, outside any sandbox, so it refuses to run
from anywhere a sandbox could change it, or reach into its bundle: a root
of any environment (read-only or not), its kept home, the host paths its
`run_args` mount (`--volume` and `--mount` sources), the directory a
named volume they or its sandbox mount binds (one made with `docker volume
create -o o=bind -o device=PATH`, as the engine says), anything its sandbox
mounts now, and what a running daemon says the sandboxes write and read;
or a path that goes through a symlink, or an executable with a second
hard link. It says why and exits with status 1. When docker does not answer, a sandbox it made cannot be asked what it
mounts: then everything else in that list is judged, which is everything
caboose would mount at the sandbox's next start. When what the sandboxes
reach cannot be told (an environment whose `config.toml` does not load,
`run_args` with `--volumes-from`, a relative mount source or a `--mount`
docker would not read, a named volume when docker does not answer, or one
of another driver or with options other than a bind's, a size's or a
tmpfs's, a volume a sandbox mounts that docker no longer has, a daemon that
answers but cannot say),
it refuses too, until it can.

`caboose status` has an `app` line (attached, with its process and version,
or not running), and `caboose doctor` a note.

## The icon and the menu

The icon is caboose's railcar, outlined while no sandbox runs and filled
while one does, whatever needs you. A count beside it is how many things
need you (a proposal, a resource warning, an image out of date, an
update), running or not; `!` means the daemon is not answering, and the
railcar is then outlined, since what runs cannot be told. VoiceOver reads
both, as counts: "caboose: 1 running; 3 things need you". The menu has, from the top:

- the daemon's state when the app cannot reach it;
- **Needs you**: what those things are, one line each;
- **Requests (N)**: each pending request, newest first, with its answers;
- **N proposals…**, which opens the page's proposals;
- one section per environment: whether its sandbox runs, its sessions and
  memory, a `localhost:PORT` item for each forwarded port, **Open in web
  page**, and **Stop…** and **Restart…** while it runs;
- **Open web UI**, **Open log** (the daemon's log, in Console), **Open at
  login**, and **Quit**.

A menu that changes while it is open is redrawn only once it closes, so an
item never moves under the pointer.

## Answering a request

A request's submenu offers exactly the answers the host offers it: Allow
once, Allow for some hours, Allow always…, and Deny. Every answer, Deny
included, asks for confirmation first, in an alert that shows what the
answer does as the host sees it, and only after it what the sandbox said,
marked "not checked". The alert's default is Cancel: Return and a stray
click cancel it, and the button that answers has no key equivalent and is
disabled for its first second. There is no "allow all".

**Allow always…** writes `config.toml`, so it opens the web page, which
shows the exact change and asks you to confirm it.

## Opening a URL

When a sandbox asks to open a URL that `open_urls` says to ask about, the
app asks in an alert of its own: the URL as the host checked it, then what
the sandbox said. Cancel is the default; **Open** opens it once. If the
app quits with the alert up, the request is denied ("the app went away;
ask again").

## Notifications

While the app runs, caboose's notifications come from it. A notification
from caboose itself (a link that failed, a checkpoint, a restart done)
carries its own title. One a sandbox sent (`caboose-agent notify`) is
always titled "From the ENV sandbox", with what the sandbox wrote below it,
so a sandbox cannot post one that looks like caboose's. At most three are
posted in any five minutes; past that, one "N things need you" sums up the
rest. Clicking
one opens the web page where it belongs.

macOS asks once whether Caboose may post notifications. If you say no, or
the app cannot ask, notifications go through `osascript` as before.

## Stop and restart

**Stop…** and **Restart…** ask first, naming the sessions they will end, as
`caboose stop` and `caboose restart` do. The daemon then runs them in the
background (a restart may rebuild the image first), and a notification says
when it is done or what failed. A CLI `restart` at the same moment races as
two CLI restarts do.

## Open at login

**Open at login** registers the app with macOS's login items, so it starts
at login (and with it, the daemon). macOS may ask you to allow it in System
Settings; the menu then says so, and its item opens the pane. Before it
turns the item on, the app checks its own place again, as at its start:
when a sandbox reaches it now, the item stays off and the menu says why.

## Building it from a checkout

On a Mac, in a checkout:

```
make app
```

builds `./caboose` if need be, assembles `Caboose.app` around a copy of it
in `CABOOSE_HOME/app-dev/Caboose.app` (`~/.caboose/app-dev/Caboose.app` by
default; the command prints the path), and signs it ad hoc. That place is
fixed, outside the checkout and outside everything a sandbox mounts: a
sandbox can write a checkout, and the host runs the bundle, at every login
once it is a login item. The bundle uses a checkout's identifier and a
checkout's daemon (the one in `CABOOSE_HOME/daemon-dev`, beside the
release's). Since it holds a copy of the binary, run `make app` again after
rebuilding `./caboose`. `make app` runs on a Mac only, and never inside the
sandbox.

`~/.caboose/app-dev/Caboose.app/Contents/MacOS/caboose app --selftest` checks every class and
selector the app uses against your macOS, and exits.
