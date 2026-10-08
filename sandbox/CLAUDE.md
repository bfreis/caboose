# Running inside a caboose sandbox

> Written by caboose (@@CABOOSE_VERSION@@) at every launch, and read-only in
> here. `~/.claude/CLAUDE.md` is the user's own; caboose never writes it.

This Claude Code runs inside caboose's sandbox for the environment
`@@CABOOSE_ENV@@`, not on the user's machine. One sandbox holds every session
of the environment: each terminal running `caboose` in a project attaches to a
session of its own, all in the same sandbox, which is why the fleet roster
sees them all. With tmux on (the default), sessions are tmux sessions, so a
closed terminal detaches and background work goes on.

@@IF isolation=container@@
## This sandbox: a Docker container

- You run as the user `agent`, with no sudo.
- What you write outside the kept paths (below) survives a stop and start, but
  not `caboose@@CABOOSE_ENV_FLAG@@ restart`, which recreates the container.
- There is no Docker daemon in here. A docker CLI has no socket unless the
  profile sets `engine_socket = true`, which hands the sandbox the host's
  engine, root-equivalent on the host: never suggest it in passing.
@@END@@
@@IF isolation=gvisor@@
## This sandbox: a container under gVisor

- gVisor (`runsc`) gives the container a kernel of its own, in user space.
- You run as `agent`, or as root where the engine requires it (`id` says).
  That root exists only inside gVisor, and its home is `/home/agent` too.
- What you write outside the kept paths (below) may not survive a stop, and
  `caboose@@CABOOSE_ENV_FLAG@@ restart` always loses it.
- There is no Docker daemon in here unless the profile sets
  `engine_socket = true` (the host's engine, root-equivalent on the host).
- The host's edits under the roots reach file watchers only as attribute
  changes, and a directory held open can list stale contents: `stat` a file
  by name before taking it for gone. See the caboose-troubleshoot skill.
@@END@@
@@IF isolation=vm@@
## This sandbox: a Linux VM on the user's Mac

- You run as root; the home is `/home/agent`. `CABOOSE_ISOLATION=vm` is set.
- **Every boot starts from a fresh root filesystem.** Whatever you write
  outside the mounts (packages, `/etc`, `/usr`, `/tmp`, an unkept path under
  `~`) is gone after a stop, a restart, a Mac reboot, or the VM ending for
  any reason. Only the roots and the kept paths (below) last.
- The VM runs its own `dockerd` when the image has one (`caboose@@CABOOSE_ENV_FLAG@@ status`
  says). Its images sit on a disk that is kept; its containers can
  bind-mount `/work` paths, and a port they publish is forwarded like any
  other.
- The Mac's edits under the roots reach file watchers only as attribute
  changes, and outbound traffic goes through the host for proxy-aware
  clients only (below). See the caboose-troubleshoot skill.
@@END@@

## Paths

The host directories that hold the projects (roots) are mounted at fixed
container paths: @@CABOOSE_ROOTS@@. These paths are the same on every
machine. A host path someone pastes in is not a path in here: translate it
through that mapping. Nothing is reachable at a second path. A changed root
takes `caboose@@CABOOSE_ENV_FLAG@@ restart`.

## What is kept

Claude Code is not in the image. It lives in `~/.local/bin`,
`~/.local/share/claude` and `~/.cache/claude` (kept, per platform), and
updates itself in place. Besides those, only what the sandbox config
`~/.config/caboose/sandbox.toml` keeps survives a restart: by default
`~/.claude`, `~/.claude.json`, `~/.config/caboose`, `~/.config/git`,
`~/.config/jj`, `~/.config/gh` and `~/.ssh` (no keys: they stay on the host,
reached through the forwarded SSH agent). Everything else under the home is
lost. `mount` shows what is kept.

You may edit the sandbox config, `start.d` (scripts run at each sandbox
start) and `shell.d` (interactive shell config) when asked, without a
proposal: they reach nothing outside the sandbox. Use the caboose-persist
skill for that.

## Reaching the host

- A server listening in here is forwarded to the same port on the host's
  `localhost` if `forward_ports` (in `[link]` of the host's `config.toml`)
  allows it; by default 3000-3999, 5173 and 8000-8999. Tell the user to open
  `http://localhost:PORT`. `caboose-agent ports` lists what listens and what
  is forwarded.
- `caboose-agent open URL` opens an http(s) URL in the user's browser
  (usually after a dialog); `caboose-agent notify TEXT` shows a notification.
@@IF hostexec=on@@
- Host commands are on: `caboose-agent host CMD ARGS` runs CMD on the host,
  as the user, in the host directory of the current one, which must be under
  a root (`-C DIR` names another). There is no shell
  (`caboose-agent host sh -c '...'` for shell syntax) and no terminal;
  stdin, stdout, stderr and the exit status come back. Run what has to run
  on the host this way instead of asking the user, but treat it as the
  user's own machine: nothing destructive without asking.
@@ELSE@@
- Host commands are off (`host_exec` in `[link]` of `config.toml`, the
  user's call). Ask the user to run what has to run on the host.
@@END@@
@@IF isolation=vm@@
- Outbound: by default proxy-aware clients (`HTTPS_PROXY` and the rest are
  `http://127.0.0.1:9128`) and, usually, ssh go out through the Mac, VPN
  included, on `egress_ports` only (default 22, 80, 443); private addresses
  are refused unless `egress_allow` names them. Everything else, DNS lookups
  included, uses the VM's own NAT, which reaches no VPN.
@@END@@
- All of this needs the host link, which every launch starts. "no host is
  linked" means it is not running: see the caboose-troubleshoot skill.

## Changing the sandbox

Nothing in here can change the image, `config.toml` or the roots. When the
user wants something to last:

- **A tool installed for good, or another host directory mounted:** write a
  proposal (the caboose-propose skill); the user reviews it with
  `caboose@@CABOOSE_ENV_FLAG@@ apply` on the host.
- **A tool's settings kept, a daemon started with the sandbox, an alias:** the
  sandbox config, `start.d`, `shell.d` (the caboose-persist skill).
- **Anything else**: a key in the host's `config.toml`, which the user
  edits. `[link]` keys (ports, URLs, host commands) apply within seconds;
  the isolation, its size, `run_args` and roots take a restart. The
  caboose-propose skill lists them.
@@IF image!=apko@@
@@IF isolation=vm,gvisor@@
- As root, a package manager the image has installs only for now: gone at
@@IF isolation=vm@@
  the next boot. Say so, and propose what should stay.
@@ELSE@@
  the next restart, or sooner. Say so, and propose what should stay.
@@END@@
@@END@@
@@END@@

The configured image is `@@CABOOSE_IMAGE@@`; when proposing,
`~/.caboose-proposals/current/state.toml` is the authority.

## caboose itself

The user runs `caboose@@CABOOSE_ENV_FLAG@@ COMMAND` on the host, never in
here: `setup`, `doctor`, `status`, `restart`, `stop`, `shell`, `logs`,
`build`, `check-image`, `apply`, `sync`, `link`, `version`, `update`,
`prune`, `detach`, `env`, `claude`, `help`. When the sandbox itself seems
wrong, point the user at `caboose@@CABOOSE_ENV_FLAG@@ doctor` first: it names
each problem and the command that fixes it.
`caboose@@CABOOSE_ENV_FLAG@@ restart` recreates the sandbox and ends every
session in it, this one included.
@@IF hostexec=on@@
You may run the read-only ones yourself, in full:
`caboose-agent host caboose@@CABOOSE_ENV_FLAG@@ status` (and `doctor`, `version`).
@@IF env!=default@@
Keep `-e @@CABOOSE_ENV@@` on each: a host command carries none of the
sandbox's variables.
@@END@@
Leave the rest to the user: `restart` and `stop` end this session; `apply`
and `setup` ask a person.
@@END@@

@@IF source=checkout@@
caboose's source is at `@@CABOOSE_SOURCE@@`, the checkout this launcher was
built from. A change to caboose itself (launcher, image layer, entrypoint,
these instructions) goes there; its own CLAUDE.md says how each takes effect,
and most need a rebuild and a restart run on the host. Its user docs are in
`@@CABOOSE_SOURCE@@/docs`.
@@END@@
@@IF source=clone@@
A clone of caboose's source is at `@@CABOOSE_SOURCE@@`, but this launcher is
an installed release (@@CABOOSE_VERSION@@): a change made there reaches this
sandbox only through a release, or the user building and installing it on
the host. Its user docs are in `@@CABOOSE_SOURCE@@/docs`.
@@END@@
@@IF source=outside@@
This launcher was built from a checkout at `@@CABOOSE_SOURCE@@` on the host,
which this sandbox cannot see: a change to caboose itself is made there, from
the host. User docs: @@CABOOSE_UPSTREAM@@/tree/main/docs
@@END@@
@@IF source=release@@
caboose is an installed release (@@CABOOSE_VERSION@@); its source, docs and
issue tracker are at @@CABOOSE_UPSTREAM@@. Nothing in here changes caboose
itself: for what the user's configuration cannot do, suggest an issue there.
@@END@@

## Skills

caboose-propose, caboose-persist and caboose-troubleshoot hold the
procedures. If one is not among your skills, read
`/etc/claude-code/.claude/skills/<name>/SKILL.md`.
