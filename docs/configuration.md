# Configuration

Each [environment](#environments) has a `config.toml` in its dir
(`~/.caboose/envs/default/config.toml` for the default one), and every
setting in it can be overridden for one shell by its variable. `caboose
setup` writes one with every setting commented out, when there is none, and
never rewrites one that is there; `caboose status` names the file it read.
A key the file does not know is an error, not ignored. What the sandbox
keeps of its home, and what of that syncs, is not here: it is the
[sandbox config](sandbox-config.md), the sandbox's own.

| `config.toml` | Variable | Default | |
|---|---|---|---|
| `repo_root` | `CABOOSE_REPO_ROOT` | `$HOME/dev` | host dir mounted into the container, at `/work` |
| `[roots]` | | unset | several host dirs instead, by name, each at `/work/<name>`; see [the repo root](#the-repo-root). `CABOOSE_REPO_ROOT` still wins |
| `format` | | `1` | the file's structure, which `caboose setup` writes; a file of a newer format than this caboose reads is refused, saying `caboose update` |
| `keep_versions` | `CABOOSE_KEEP_VERSIONS` | `2` | installed versions to retain (~224MB each) |
| `image`, `container` | `CABOOSE_IMAGE`, `CABOOSE_CONTAINER` | `caboose`; `caboose-<env>` for other environments | names |
| `base_image` | `CABOOSE_BASE_IMAGE` | unset | [your own image](images.md) to build the sandbox on; unset, the environment's `image/Dockerfile` when it has one, else the embedded Dockerfile, built and tagged `$CABOOSE_IMAGE-base` |
| `ready_timeout` | `CABOOSE_READY_TIMEOUT` | `600` | seconds to wait for first-run install |
| `no_auto_build` | `CABOOSE_NO_AUTO_BUILD` | unset | don't build a missing image, or rebuild a stale one when creating the container; say to run `caboose build` |
| `no_tmux` | `CABOOSE_NO_TMUX` | unset | skip tmux: native rendering, but no detach/reattach |
| `auto_sync` | `CABOOSE_AUTO_SYNC` | unset | [sync](sync.md) before a launch that finds nothing running in the container, once a sync remote is set |
| `tz` | `CABOOSE_TZ` | host's zone | timezone inside the container |
| `docker_sock` | `CABOOSE_DOCKER_SOCK` | unset | mount the host docker socket — **removes the isolation**, see caveats |
| `docker_run_args` | `CABOOSE_DOCKER_RUN_ARGS` | unset | more arguments for `docker run`, as the container is created; see [docker run arguments](#docker-run-arguments). The variable is split at whitespace |
| `forward_ports` | `CABOOSE_FORWARD_PORTS` | `3000-3999 5173 8000-8999` | ports listening in the sandbox that are forwarded to the same port on this machine's localhost, or `none`; see [the host link](host-link.md#ports) |
| `open_urls` | `CABOOSE_OPEN_URLS` | `ask` | whether the sandbox may open URLs in your browser: `ask`, `allow` or `off`; see [the host link](host-link.md#urls-and-notifications) |
| | `CABOOSE_ENV` | `default` | the environment; `--env` wins over it |
| | `CABOOSE_HOME` | `~/.caboose` | where environments live |
| | `CABOOSE_DATA_DIR` | `$CABOOSE_HOME/envs/<env>/data` | the data dir, named outright |
| | `CABOOSE_NO_AUTO_UPDATE` | unset | don't [update caboose](getting-started.md#updates) by itself; machine-wide, so no `config.toml` key |

A container defaults to UTC, so the launcher detects the host's zone name
(from `$TZ`, else `/etc/localtime`) and applies it two ways: container-wide
at creation, and per-session via `tmux new-session -e` at attach. The second
is what makes a new session correct without a restart — a tmux session
inherits the *server's* environment, not the attaching client's, so a server
started before the zone was known would otherwise hand every new session
UTC. Sessions already running keep the zone they were created with.

The roots and `CABOOSE_KEEP_VERSIONS` are both fixed when the
container is created (as bind mounts and a `docker run -e`), so changing
either needs `caboose restart`. `caboose prune` always uses the current
value of the second, and `caboose status` warns when the two disagree.

## docker run arguments

`docker_run_args` adds arguments of your own to the `docker run` that
creates the container: a capability, a device, a DNS server, a host entry,
a memory limit.

```toml
docker_run_args = ["--cap-add=NET_ADMIN", "--device=/dev/net/tun", "--dns=100.100.100.100"]
```

or, for one shell, `CABOOSE_DOCKER_RUN_ARGS="--cap-add=NET_ADMIN --device=/dev/net/tun"`,
split at whitespace, which wins over the file. Like the roots, they are
fixed when the container is created: `caboose restart` applies a change,
and until then a launch, `caboose status` and `caboose doctor` say the
container was created with others.

Each argument is one flag with its value, `--flag=value`. Only a few
boolean flags (`--privileged`, `--read-only`, ...) may stand alone, since
docker would take the next argument as another flag's value. Refused:

- short flags (`-e`): write the long one (`--env=...`);
- the flags caboose sets or depends on: `--name`, `--hostname`,
  `--user`, `--entrypoint`, `--init`, `--restart`, `--rm`, `--detach`,
  `--interactive`, `--tty`, `--attach`, `--platform`, `--pull`,
  `--cidfile`, and `--env-file` and `--label-file`, whose contents caboose
  cannot check;
- `--env` for a variable caboose sets (`HOME`, `PATH`, `TZ`,
  `SSH_AUTH_SOCK`, anything `CABOOSE_` or `TINI_`), and `--label` for
  caboose's own (`io.github.bfreis.caboose.*`);
- a `--volume`, `--mount` or `--tmpfs` at, inside or around one of
  caboose's mounts (the roots under `/work`, the kept parts of the home,
  Claude Code's install), or over the agent's home itself.

A refused argument stops the launch that would create the container, and
`caboose restart` before it removes the old one; `caboose doctor` names
it. Everything else goes to docker as written, and is yours to choose:
some arguments (`--privileged`, `--cap-add`, `--device`, `--volume`,
`--network=host`, ...) give the sandbox more of the host, which `caboose
doctor` notes. Only the host can set them: they are not in the sandbox's
own config, and a session cannot propose them.

## The repo root

The container mounts one directory tree from the host, the repo root, at
`/work`: `CABOOSE_REPO_ROOT` (or `repo_root` in
[`config.toml`](#configuration)), default `~/dev`. So `~/dev/you/project`
is `/work/you/project` inside, and every project you run `caboose` in has
to be under the root; running it anywhere else is an error that says so.

`caboose setup roots` asks for them and writes them into `config.toml`;
the rest of this section is what it writes. If your projects live
somewhere else, point it there (or, for one shell or in your profile, set
the variable, which wins over the file):

```sh
export CABOOSE_REPO_ROOT=~/src
```

If they live in more than one place, name each in a `[roots]` table in
`config.toml`, in place of `repo_root`; each is mounted at `/work/<name>`:

```toml
[roots]
dev = "~/dev"         # /work/dev
work = "~/work"       # /work/work
```

Roots cannot contain one another. Use the same names on every machine you
[sync](sync.md), so a project has the same path on each.

Bind mounts are fixed when the container is created, so changing the roots
once a container exists needs `caboose restart`, which kills running
sessions (it asks first); until then a launch warns that they differ.

Everything under the roots is readable and **writable** from inside the
container. That is the point, but it is also the whole of what the sandbox
can reach on your disk, so choose them accordingly.

## Environments

An environment is a whole caboose of its own: its own data dir — and so
its own Claude login, memories and settings —, container, image and
config. Use one to keep a work subscription apart from a personal one, or
a different repo root, or a different image:

```sh
caboose -e work setup          # asks to create ~/.caboose/envs/work, then sets it up
caboose --env work             # or: CABOOSE_ENV=work caboose
caboose env                    # list them, the current one marked
```

The first launch in a new environment builds its image and creates its
container, `caboose-work`, and asks you to log in to Claude, as a first
run ever does. Sessions in different environments are in different
containers, so they do not see each other in the fleet roster. Any other
command in an environment that was never created is refused: a mistyped
`--env` would otherwise start a new, empty sandbox. The environment is
caboose's own flag, before the command: `caboose setup --env work` is
refused, with the spelling that works.

Without `--env` or `CABOOSE_ENV` you are in `default`, whose container and
image are named plain `caboose`.

## Setup

```sh
caboose setup                  # every section: roots, image, git, sync, and the
                               # container and the login along the way
caboose setup git              # only the git identity and signing
```

asks its questions on the terminal, each showing what is there now as
its default (Enter keeps it), and writes only what an answer changed, so
a re-run is safe and hand edits to `config.toml` and the sandbox's git
config survive. With no terminal it refuses rather than guess. A section
writes when its questions are done: stopping halfway (^C, ^D) leaves the
sections before it written and nothing of its own. A completed run leaves
a `config.toml` in the environment, which is what a launch and `caboose
doctor` take as set up.

Run for an environment that does not exist yet (`caboose -e work setup`),
it asks before creating it.

`config.toml` is edited line by line, as you would: only the keys setup
asks about change, a key's commented-out template line is uncommented with
the new value, and comments and other settings stay where they are. The
result is read back before it is written; if it would not say what you
answered (the file uses TOML the editor cannot follow), nothing is written
and setup shows the lines to change by hand.

- **roots** shows the [repo roots](#the-repo-root) and edits them: change
  one's directory, add another (each of several needs a name, its
  directory under `/work`), or remove one. A change that moves projects to
  another path in the container — going from one root to several moves
  `/work/x` to `/work/<name>/x` — is listed and asks first: Claude Code
  keeps each project's memories and history under its path, and will not
  find them under the old one (nothing is moved or deleted). Removing one
  of several keeps the other's name, and so its paths. A container that
  exists keeps the roots it was created with until `caboose restart`.
- **image** keeps what the sandbox is built on, or changes it: the
  Dockerfile embedded in caboose (the default when there is no `image/`),
  an [environment's own](images.md) `image/Dockerfile` written
  from caboose's preset or from the parts you choose, or, given up, the
  `base_image` in `config.toml`. An `image/Dockerfile` that is there is
  kept by default; replacing it shows the difference and asks. After a
  change it offers to build; moving the container onto the new image is
  `caboose restart`, which it leaves to you.
- **git**: the identity and signing, [below](#git-identity-and-commit-signing).
- **the container**, in a whole run only, after the image: a running one
  is left alone, a stopped one started, and an absent one created when you
  say so (building the image first if there is none). git lists the
  forwarded agent's keys from it, and sync runs its git in it.
- **sync** asks for the [sync](sync.md) remote and, with
  one, for `auto_sync`. A new remote is set, and synced with right away,
  as `caboose sync --remote URL` does: that needs the container (it is
  started, never built) and no session running, and when it cannot run
  setup says why and what to run later, and goes on. A remote is not
  removed here; turning `auto_sync` off stops the automatic part.
- **the Claude login**, last in a whole run: whether the data dir has one
  (`.claude/.credentials.json`, only looked at, never read). Logging in is
  Claude Code's own first-run flow, which setup cannot drive from the
  host; with none yet, run from a project under a root, it offers to start
  a session there, which asks you to log in, and otherwise says how.

### Git identity and commit signing

`caboose setup git` asks for the sandbox's `user.name` and `user.email`
and how it signs commits, and writes them into its git config,
`<data>/home/.config/git/config` (`~/.config/git/config` inside). The
defaults are the sandbox's own values; where it has none, the host's
*default* identity is offered — an `[include]` is followed, an
`[includeIf]` is not, since the sandbox has one git config for every repo
and a per-repo override (a work address for work checkouts, say) must not
become it by accident. For signing it offers the key already set, the
host's SSH signing key, the keys the forwarded agent holds (listed when
the container is running), a pasted key or the path of a `.pub` file, or
none; then whether to sign every commit. See [SSH agent and commit signing](ssh.md)
for how signing works in the sandbox.

A launch never writes the identity, so a new environment never gets the
host's without a word. It says, in one line, while there is none, and
`caboose doctor` lists it as a problem. `git config --global` inside the
sandbox works as well as setup does.
