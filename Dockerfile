# Isolated Claude Code sandbox: the default base image.
#
# This is only the base: OS packages and toolchains. The launcher builds it,
# tags it <CABOOSE_IMAGE>-base, runs the image check against it like any
# CABOOSE_BASE_IMAGE, and then builds layer.Dockerfile FROM it -- the agent
# user, its home, the entrypoint and tmux.conf all live there, so that a
# user's own image goes through exactly the same path. Nothing in here may
# assume the agent user exists, and nothing here takes the host's UID/GID.
#
# Claude Code itself is deliberately NOT installed into this image. It is
# installed on first run into /home/agent/.local, which the launcher bind
# mounts from ~/.caboose/dot_local/<platform> — so it persists across image
# rebuilds and, crucially, can update itself in place (an install in the
# image would be root-owned, and would need the autoupdater disabled).
#
# The base is a plain distro, not node:22-*. The node images are built on
# buildpack-deps, which cost ~381MB compressed and — worse — pinned the whole
# userland to whatever Node's base froze: bookworm's git was 2.39.5, four years
# stale. Ubuntu 26.04 LTS is 38.9MB and ships git 2.53.0 in main, so git needs
# no special handling here at all. Every language toolchain below is now an
# explicit, independently bumpable ARG rather than a side effect of the base.
#
# Rule of thumb for what goes in apt vs. a release tarball: apt when the
# distro's version is close enough to upstream and we want its security
# updates (git), a tarball when the distro is hopelessly behind (gh is 2.46 in
# Ubuntu vs 2.101 upstream; jj and the docker CLI are not packaged at all).
#
# Everything after the OS packages is in sections, between a
# "# caboose:section NAME [off] TITLE" line and "# caboose:end": what
# `caboose setup` offers to leave out of (or, for an "off" section, put
# into) an environment's own image/Dockerfile. An off section is commented
# out line by line, so this file builds without it. A section carries its
# own ARGs and ENVs, and must not depend on another one.
FROM ubuntu:26.04

# git comes from the archive on purpose: 2.53.0 is two releases off upstream
# and Ubuntu patches it for CVEs, which a source build would make our problem.
# universe is enabled in the stock ubuntu image, which is where ripgrep lives.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git openssh-client \
        ripgrep jq less vim procps unzip \
        build-essential python3 python3-venv \
        tmux zstd \
        ncurses-term \
    && rm -rf /var/lib/apt/lists/*

# caboose:section node Node.js, npm and corepack (for npx-launched MCP servers)
# Node. Not for Claude Code — that is a self-contained native binary — but for
# npx-launched MCP servers and for JS checkouts under the repo root, some of
# which build node-pty, hence build-essential and python3 above staying put.
# Bumping to the Node 24 line is a one-word ARG change; 22 is what the
# node:22-bookworm base provided, so this keeps JS behaviour identical.
ARG NODE_VERSION=22.23.1
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=x64 ;; \
      arm64) target=arm64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${target}.tar.xz" \
      | tar -xJ --strip-components=1 -C /usr/local \
            --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE'; \
    corepack enable; \
    node --version; \
    npm --version
# caboose:end

# caboose:section bun Bun
# Bun, alongside Node rather than instead of it. It is not a drop-in: JS
# checkouts under the repo root pin pnpm via corepack, call node/tsx/vitest
# directly, and build native N-API addons, which is where bun compatibility is
# thinnest. It is here because some of them pin bun as their packageManager.
ARG BUN_VERSION=latest
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=x64 ;; \
      arm64) target=aarch64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    if [ "$BUN_VERSION" = latest ]; then \
      url="https://github.com/oven-sh/bun/releases/latest/download/bun-linux-${target}.zip"; \
    else \
      url="https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-${target}.zip"; \
    fi; \
    curl -fsSL -o /tmp/bun.zip "$url"; \
    unzip -q /tmp/bun.zip -d /tmp; \
    mv "/tmp/bun-linux-${target}/bun" /usr/local/bin/bun; \
    rm -rf /tmp/bun.zip "/tmp/bun-linux-${target}"; \
    bun --version
# caboose:end

# caboose:section go Go
# Go. The image previously had no Go at all, and the Go modules under the repo
# root need a toolchain. 1.27.1 covers the lot: it matches the newest `go`
# directive across them, the rest being 1.26.x or older.
ARG GO_VERSION=1.27.1
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=amd64 ;; \
      arm64) target=arm64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${target}.tar.gz" \
      | tar -xz -C /usr/local; \
    /usr/local/go/bin/go version
# Go's own bin, and GOPATH's: GOPATH defaults to $HOME/go, and the layer
# fixes HOME at /home/agent. The layer prepends ~/.local/bin to this.
#
# Not bind-mounted (the layer lists what is), and worth fixing separately:
# Go's module cache lives in GOPATH (~/go/pkg/mod) and its build cache in
# ~/.cache/go-build, neither of which survives a container recreate, so
# `caboose restart` currently costs a full re-download of every
# dependency.
ENV PATH=/home/agent/go/bin:/usr/local/go/bin:$PATH
# caboose:end

# caboose:section gh the GitHub CLI (gh)
# gh. Ubuntu has 2.46.0 against 2.101.0 upstream, so this takes the tarball.
# Like hub before it, gh does not bundle git — it shells out to the git above.
ARG GH_VERSION=latest
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=amd64 ;; \
      arm64) target=arm64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    if [ "$GH_VERSION" = latest ]; then \
      version="$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/cli/cli/releases/latest \
                 | sed -n 's#.*/tag/v##p')"; \
    else \
      version="$GH_VERSION"; \
    fi; \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${version}/gh_${version}_linux_${target}.tar.gz" \
      | tar -xz --strip-components=2 -C /usr/local/bin "gh_${version}_linux_${target}/bin/gh"; \
    gh --version
# caboose:end

# caboose:section jj Jujutsu (jj)
# jj (Jujutsu) — not packaged for Ubuntu either, pull the release binary.
ARG JJ_VERSION=latest
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=x86_64-unknown-linux-musl ;; \
      arm64) target=aarch64-unknown-linux-musl ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    if [ "$JJ_VERSION" = latest ]; then \
      version="$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/jj-vcs/jj/releases/latest \
                 | sed -n 's#.*/tag/v##p')"; \
    else \
      version="$JJ_VERSION"; \
    fi; \
    url="https://github.com/jj-vcs/jj/releases/download/v${version}/jj-v${version}-${target}.tar.gz"; \
    curl -fsSL "$url" | tar -xz -C /usr/local/bin ./jj; \
    jj --version
# caboose:end

# caboose:section docker the Docker CLI (inert unless the host's socket is mounted)
# Docker CLI only -- no daemon, no containerd, just the client. It is inert
# unless CABOOSE_DOCKER_SOCK mounts a socket for it to talk to, which is off
# by default and deliberately so (see the launcher). Shipping the client
# regardless means turning the socket on later needs no image rebuild.
ARG DOCKER_CLI_VERSION=latest
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=x86_64 ;; \
      arm64) target=aarch64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    if [ "$DOCKER_CLI_VERSION" = latest ]; then \
      version="$(curl -fsSL "https://download.docker.com/linux/static/stable/${target}/" \
                 | grep -oE 'docker-[0-9]+\.[0-9]+\.[0-9]+\.tgz' \
                 | sed 's/docker-//; s/\.tgz//' | sort -V | tail -1)"; \
    else \
      version="$DOCKER_CLI_VERSION"; \
    fi; \
    curl -fsSL "https://download.docker.com/linux/static/stable/${target}/docker-${version}.tgz" \
      | tar -xz --strip-components=1 -C /usr/local/bin docker/docker; \
    docker --version
# caboose:end

# caboose:section rust off Rust (rustup, with the stable toolchain)
# # Rust through rustup, into /usr/local: RUSTUP_HOME and CARGO_HOME are
# # made writable by everyone, as the official rust image does, since the
# # agent user (not root) fetches crates into CARGO_HOME's registry.
# ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH
# RUN set -eux; \
#     curl -fsSL https://sh.rustup.rs \
#       | sh -s -- -y --no-modify-path --profile minimal --default-toolchain stable; \
#     chmod -R a+w "$RUSTUP_HOME" "$CARGO_HOME"; \
#     rustc --version; \
#     cargo --version
# caboose:end
