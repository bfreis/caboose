# Start-up scripts and shell config

Two directories of your own configure the sandbox from the inside:

| | read | holds |
|---|---|---|
| `~/.config/caboose/start.d` | once, at container start | scripts that start daemons or set something up |
| `~/.config/caboose/shell.d` | by every interactive bash | aliases, functions, a prompt, environment variables |

Both live in the data dir, at `<data>/home/.config/caboose` (`<data>` is
`~/.caboose/envs/default/data` for the default environment), mounted at
`~/.config/caboose`. So they outlive `caboose restart`, and can be edited
from either side: on the host, or from inside the sandbox, where a session
can write one for you. Neither is a [proposal](proposals.md): what they run
runs as the sandbox's user, inside the container, and reaches nothing a
session could not already reach.

## start.d

When the container starts — the first launch, `caboose restart`, and the
Docker engine coming back up — the entrypoint runs every executable file in
`start.d`:

- **One at a time, in name order**, byte order whatever the locale, like
  `run-parts`: a script runs to its end before the next starts, so
  `20-b` can count on what `10-a` did. Number them to order them.
- **A daemon goes in the background, in its script.** A script that does
  not end holds up the ones after it (though never a launch: they run once
  the container is ready).
- **Skipped:** files that are not executable (`chmod -x` turns one off),
  dotfiles, backups ending in `~`, and directories.
- **In `~`, with no stdin**, as the sandbox's user, with the container's
  environment.
- **Their output goes to `caboose logs`**, with a line from caboose as
  each one starts and when one fails. A failure is logged, and the next
  script runs.
- **Stopped with the container.** On `docker stop` (and so `caboose
  restart`), what they left running gets a `TERM` before it is killed.

Nothing restarts a daemon that dies; a script that wants that can loop. To
try one without a restart, run it: `~/.config/caboose/start.d/10-foo`.
`caboose doctor` lists the scripts the next start will run, and notes a
file there that is not executable.

For example, a one-off setup step, then a daemon that relies on it:

```sh
#!/bin/sh
# ~/.config/caboose/start.d/10-cache
mkdir -p "$HOME/.cache/mydaemon"
```

```sh
#!/bin/sh
# ~/.config/caboose/start.d/20-mydaemon
mydaemon --cache "$HOME/.cache/mydaemon" >/dev/null 2>&1 &
```

`10-cache` has finished by the time `20-mydaemon` starts. The `&` is what
lets any scripts after `20-mydaemon` run, and without the redirect the
daemon's own log would go to `caboose logs` too. Remember `chmod +x` on
both.

## shell.d

Every interactive bash — `caboose shell`, a tmux window, a `!` command,
and the snapshot of the shell Claude Code's Bash tool takes — reads
`shell.d` through `~/.bashrc`:

- `*.sh` for any shell caboose may offer: bash today, zsh perhaps later.
  Keep them to what both read the same: `alias`, `export`, plain
  functions. `.sh` here means that, not POSIX `sh`.
- `*.bash` for bash alone: `shopt`, `PS1`, `bind`, completions.
- **One order across both**, by name in byte order, the `.sh` before the
  `.bash` of the same name: `10-a.sh 10-a.bash 20-b.sh 20-b.bash`. The
  bash-only file can build on the shared one.
- Anything else is skipped: dotfiles, a `README`, an editor's backup.

The Bash tool reads your aliases too. Something like `alias ll='ls -l'` is
harmless; `alias rm='rm -i'` or `alias grep=rg` changes what the agent's
commands do, so keep what you put here deliberate.

To use the aliases of another machine, copy the file in, or, if it lives in
a repo under a [root](configuration.md#roots), source it from one
line: `. /work/dev/dotfiles/aliases.sh`. caboose never copies the host's
shell config itself. Guard what is only for one system or shell:

```sh
case "$(uname)" in Darwin) alias o=open ;; Linux) alias o=xdg-open ;; esac
[ -n "$ZSH_VERSION" ] && setopt autocd
```

The loader is `/usr/local/lib/caboose/shellrc.bash` in the image, and the
layer adds the line that sources it to `~/.bashrc`, keeping whatever
`~/.bashrc` the image already has. It also adds a `~/.bash_profile` that
sources `~/.bashrc`, so a login bash reads it too, unless the image has a
`~/.bash_profile`, `~/.bash_login` or `~/.profile` of its own. A new
`shell.d` file is read by the next shell you open; no restart needed.

[`caboose sync`](sync.md) carries both, with the rest of `~/.config/caboose`,
by default: a script written on one machine runs on the others at their next
start. A script that needs a tool only one machine's image has fails, in
`caboose logs`, on the others; to keep a directory to one machine, stop it
syncing in the [sandbox config](sandbox-config.md).
