<p align="center">
  <img src="docs/assets/logo.svg" alt="caboose" width="220">
</p>

<h1 align="center">caboose</h1>

<p align="center">
  A Claude Code sandbox in Docker with its own account, settings, and plugins,<br>
  fully separate from the Claude Code installed on the host.
</p>

<p align="center">
  <a href="https://github.com/bfreis/caboose/actions/workflows/ci.yml"><img src="https://github.com/bfreis/caboose/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/bfreis/caboose/releases/latest"><img src="https://img.shields.io/github/v/release/bfreis/caboose" alt="Latest release"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/bfreis/caboose" alt="Go version"></a>
  <img src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey" alt="macOS | Linux">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/bfreis/caboose" alt="MIT license"></a>
</p>

A caboose rides on someone else's train: this is a long-lived place for
Claude Code to live that hitches onto your machine, with your kit carried in
and out through bind mounts.

## Install

You need a running Docker engine: Docker Desktop, OrbStack or colima.

```sh
curl -fsSL https://github.com/bfreis/caboose/releases/latest/download/install.sh | sh
cd ~/dev/some-project && caboose
```

The installer puts caboose in `~/.local/bin` without root, checks it
against the release's checksums, and runs `caboose setup`. After that,
caboose keeps itself up to date. The first launch builds the image, which
takes a few minutes once, and asks you to log in to Claude. See
[Getting started](docs/getting-started.md) for the details, and for
building from a checkout.

## What you get

- **One long-lived container for every session.** Each project gets a tmux
  session inside it, so closing the terminal detaches instead of killing
  the work, background agents keep running, and sessions see each other in
  the fleet roster.
- **Its own Claude Code.** It has its own login, settings, memories and
  plugins, kept in a data dir on the host (`~/.caboose`) and never in your
  checkouts. Claude Code updates itself in place, and the container can be
  recreated at any time without losing anything.
- **Only what you mount.** The sandbox sees your repo roots (default
  `~/dev`, at `/work` inside) and nothing else on your disk. Your SSH agent
  is forwarded, so git pushes and signs commits with keys that never enter
  the container.
- **The same paths on every machine.** Projects live at `/work/...`
  whatever the host path is, so `caboose sync` can carry memories, settings
  and skills between machines through a private git repo.
- **Your image, if you want.** The default image is Ubuntu with git, gh,
  jj, Node, Bun, Go and the docker CLI. You can trim it, edit its
  Dockerfile, or build on any glibc or musl image that passes
  `caboose check-image`.
- **Environments.** Run several fully separate cabooses side by side, for
  example to keep a work subscription apart from a personal one.

## Usage

```sh
caboose                    # start or attach the session for this project
caboose claude --resume    # pass arguments to claude
caboose setup              # roots, image, isolation, git identity, sync
caboose apply              # review and apply the changes sessions proposed
caboose doctor             # what is wrong, and the command that fixes it
caboose status             # container, sessions, disk use
caboose help               # every command
```

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | install, first run, updates |
| [Commands](docs/commands.md) | every command, and `caboose doctor` |
| [Configuration](docs/configuration.md) | `config.toml`, repo roots, environments, `caboose setup`, git identity |
| [How it works](docs/how-it-works.md) | the container, persistent state, tmux, terminal rendering |
| [Start-up scripts and shell config](docs/startup.md) | `start.d` run at container start, `shell.d` read by every bash |
| [Proposals](docs/proposals.md) | sessions asking for a tool, a kept directory or a root; `caboose apply` |
| [Use your own image](docs/images.md) | building on another image, and what it must contain |
| [The sandbox config](docs/sandbox-config.md) | what the sandbox keeps of its home, and what of that syncs |
| [Syncing between machines](docs/sync.md) | `caboose sync`: what syncs and how conflicts merge |
| [The host link](docs/host-link.md) | ports forwarded to your localhost, URLs and notifications from the sandbox |
| [SSH agent and commit signing](docs/ssh.md) | agent forwarding on macOS and Linux, 1Password, signing |
| [Caveats](docs/caveats.md) | what to know before you rely on it, including the docker socket |
| [Contributing](CONTRIBUTING.md) | building, testing and releasing caboose |

## License

MIT; see [LICENSE](LICENSE).
