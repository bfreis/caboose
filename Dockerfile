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
#
# No download may hang the build. apt runs with retries and timeouts, and
# every RUN that downloads defines its own fetch (a section cannot lean on
# another, nor leave a helper behind in the image): fetch SECONDS URL
# [CURL_ARGS...] gives up on a connection after 15s, on a transfer that
# moves under 10 kB/s for a minute, and on one still going after SECONDS;
# tries 3 more times, whatever the error (a connection cut halfway, too);
# and then says which URL it could not get. So a network the build cannot
# reach (a VPN's route, a proxy) fails in about a minute instead of never.
# A dead transfer is the stall check's to catch: SECONDS only backs it up,
# generous enough for a large file on a slow link (75 MB at 100 kB/s is 13
# minutes), which a fixed cap of a few minutes cut short.
FROM ubuntu:26.04

# git comes from the archive on purpose: 2.53.0 is two releases off upstream
# and Ubuntu patches it for CVEs, which a source build would make our problem.
# universe is enabled in the stock ubuntu image, which is where ripgrep lives.
RUN apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update \
    && apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -y --no-install-recommends \
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
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    fetch 1800 "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${target}.tar.xz" \
      -o /tmp/node.tar.xz; \
    tar -xJf /tmp/node.tar.xz --strip-components=1 -C /usr/local \
        --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE'; \
    rm /tmp/node.tar.xz; \
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
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    if [ "$BUN_VERSION" = latest ]; then \
      url="https://github.com/oven-sh/bun/releases/latest/download/bun-linux-${target}.zip"; \
    else \
      url="https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-${target}.zip"; \
    fi; \
    fetch 1800 "$url" -o /tmp/bun.zip; \
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
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    fetch 1800 "https://go.dev/dl/go${GO_VERSION}.linux-${target}.tar.gz" -o /tmp/go.tar.gz; \
    tar -xzf /tmp/go.tar.gz -C /usr/local; \
    rm /tmp/go.tar.gz; \
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
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    if [ "$GH_VERSION" = latest ]; then \
      latest="$(fetch 30 https://github.com/cli/cli/releases/latest -o /dev/null -w '%{url_effective}')"; \
      version="${latest##*/tag/v}"; \
    else \
      version="$GH_VERSION"; \
    fi; \
    fetch 1800 "https://github.com/cli/cli/releases/download/v${version}/gh_${version}_linux_${target}.tar.gz" \
      -o /tmp/gh.tar.gz; \
    tar -xzf /tmp/gh.tar.gz --strip-components=2 -C /usr/local/bin "gh_${version}_linux_${target}/bin/gh"; \
    rm /tmp/gh.tar.gz; \
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
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    if [ "$JJ_VERSION" = latest ]; then \
      latest="$(fetch 30 https://github.com/jj-vcs/jj/releases/latest -o /dev/null -w '%{url_effective}')"; \
      version="${latest##*/tag/v}"; \
    else \
      version="$JJ_VERSION"; \
    fi; \
    url="https://github.com/jj-vcs/jj/releases/download/v${version}/jj-v${version}-${target}.tar.gz"; \
    fetch 1800 "$url" -o /tmp/jj.tar.gz; \
    tar -xzf /tmp/jj.tar.gz -C /usr/local/bin ./jj; \
    rm /tmp/jj.tar.gz; \
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
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    if [ "$DOCKER_CLI_VERSION" = latest ]; then \
      index="$(fetch 30 "https://download.docker.com/linux/static/stable/${target}/")"; \
      version="$(printf '%s\n' "$index" \
                 | grep -oE 'docker-[0-9]+\.[0-9]+\.[0-9]+\.tgz' \
                 | sed 's/docker-//; s/\.tgz//' | sort -V | tail -1)"; \
    else \
      version="$DOCKER_CLI_VERSION"; \
    fi; \
    fetch 1800 "https://download.docker.com/linux/static/stable/${target}/docker-${version}.tgz" \
      -o /tmp/docker.tgz; \
    tar -xzf /tmp/docker.tgz --strip-components=1 -C /usr/local/bin docker/docker; \
    rm /tmp/docker.tgz; \
    docker --version
# caboose:end

# caboose:section dockerd the Docker engine (dockerd, run in the VM under isolation vm)
# dockerd, containerd and runc from Docker's static release, with the buildx
# and compose plugins, and iptables for its bridge. Only isolation vm starts
# it (the entrypoint): there the sandbox is a VM of its own, level 3, and
# its dockerd runs in the guest, its images on a disk kept across restarts.
# Under docker and gvisor it stays unused: never the host's socket. It
# brings the docker CLI too, so it builds without the docker section.
ARG DOCKER_ENGINE_VERSION=latest
RUN set -eux; \
    apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update; \
    apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -y --no-install-recommends iptables; \
    rm -rf /var/lib/apt/lists/*; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=x86_64; plugin=amd64; compose=x86_64 ;; \
      arm64) target=aarch64; plugin=arm64; compose=aarch64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    if [ "$DOCKER_ENGINE_VERSION" = latest ]; then \
      index="$(fetch 30 "https://download.docker.com/linux/static/stable/${target}/")"; \
      version="$(printf '%s\n' "$index" \
                 | grep -oE 'docker-[0-9]+\.[0-9]+\.[0-9]+\.tgz' \
                 | sed 's/docker-//; s/\.tgz//' | sort -V | tail -1)"; \
    else \
      version="$DOCKER_ENGINE_VERSION"; \
    fi; \
    fetch 1800 "https://download.docker.com/linux/static/stable/${target}/docker-${version}.tgz" \
      -o /tmp/docker.tgz; \
    tar -xzf /tmp/docker.tgz --strip-components=1 -C /usr/local/bin; \
    rm /tmp/docker.tgz; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    buildx="$(fetch 30 https://github.com/docker/buildx/releases/latest -I -o /dev/null -w '%{url_effective}')"; \
    buildx="${buildx##*/}"; \
    fetch 1800 "https://github.com/docker/buildx/releases/download/${buildx}/buildx-${buildx}.linux-${plugin}" \
      -o /usr/local/lib/docker/cli-plugins/docker-buildx; \
    fetch 1800 "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-${compose}" \
      -o /usr/local/lib/docker/cli-plugins/docker-compose; \
    chmod 755 /usr/local/lib/docker/cli-plugins/*; \
    dockerd --version; \
    docker buildx version; \
    docker compose version
# caboose:end

# caboose:section sudo sudo
# sudo, for the tools and scripts that call it. Only where the sandbox is
# root (isolation vm, and gvisor where the agent user cannot write its
# mounts) does it do anything: the agent user gets no sudoers entry.
RUN apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update \
    && apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -y --no-install-recommends sudo \
    && rm -rf /var/lib/apt/lists/*
# caboose:end

# caboose:section rust off Rust (rustup, with the stable toolchain)
# # Rust through rustup, into /usr/local: RUSTUP_HOME and CARGO_HOME are
# # made writable by everyone, as the official rust image does, since the
# # agent user (not root) fetches crates into CARGO_HOME's registry. It
# # takes rustup-init itself, as that image does, not sh.rustup.rs: that
# # script downloads it with a curl of its own, which has no timeout.
# ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH
# RUN set -eux; \
#     case "$(dpkg --print-architecture)" in \
#       amd64) target=x86_64-unknown-linux-gnu ;; \
#       arm64) target=aarch64-unknown-linux-gnu ;; \
#       *) echo "unsupported arch" >&2; exit 1 ;; \
#     esac; \
#     fetch() { \
#       t="$1"; u="$2"; shift 2; \
#       curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
#         || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
#     }; \
#     fetch 1800 "https://static.rust-lang.org/rustup/dist/${target}/rustup-init" -o /tmp/rustup-init; \
#     chmod 755 /tmp/rustup-init; \
#     /tmp/rustup-init -y --no-modify-path --profile minimal --default-toolchain stable; \
#     rm /tmp/rustup-init; \
#     chmod -R a+w "$RUSTUP_HOME" "$CARGO_HOME"; \
#     rustc --version; \
#     cargo --version
# caboose:end
