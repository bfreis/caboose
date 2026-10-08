# Isolated Claude Code sandbox: a base image built from a Dockerfile.
#
# This is only the base: OS packages and toolchains. It is the seed caboose
# setup writes into a dockerfile image profile's directory. The launcher
# builds that, tags it caboose-base:<env>, runs the image check against it
# like any other base, and then builds layer.Dockerfile FROM it -- the
# agent user, its home, the entrypoint and tmux.conf all live there, so
# that every base goes through exactly the same path. Nothing in here may
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
# stale. Ubuntu 26.04 LTS is 38.9MB, and its git (2.53.0) is only one release
# short of what the sandbox wants, which the Git Maintainers PPA covers below.
# Every language toolchain below is now an explicit, independently bumpable
# ARG rather than a side effect of the base.
#
# Rule of thumb for what goes in apt vs. a release tarball: apt when the
# distro's version is close enough to upstream and we want its security
# updates (git, through its PPA), a tarball when the distro is hopelessly
# behind (gh is 2.46 in Ubuntu vs 2.101 upstream; jj and the docker CLI are
# not packaged at all).
#
# Everything after the OS packages is in sections, between a
# "# caboose:section NAME [off] TITLE" line and "# caboose:end". A proposal
# from a session adds a section of its own the same way, or replaces one
# of the same name. An off section is commented out line by line, so this
# file builds without it. A section carries its own ARGs and ENVs, and
# must not depend on another one.
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

# git is the one package that comes from the Git Maintainers PPA
# (ppa:git-core/ppa) rather than the archive: the archive's 2.53.0 is behind
# what the sandbox needs (2.54 or later), and the PPA tracks upstream with
# Ubuntu's packaging and security fixes, which a source build would make our
# problem. The repo is added by hand, as a deb822 source signed by the PPA's
# key alone, pinned by fingerprint (Launchpad's API names it) and checked after
# the download, so nothing needs add-apt-repository. An apt preference keeps
# every other package on the archive and git and git-man on the PPA.
# gpg is installed to check the key, and removed again unless something
# installed after it depends on it (marked auto, then autoremoved, rather than
# purged, which would take any such package with it).
# universe is enabled in the stock ubuntu image, which is where ripgrep lives.
ARG GIT_PPA_KEY=F911AB184317630C59970973E363C90F8F1B6217
RUN set -eux; \
    apt='apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30'; \
    fetch() { \
      t="$1"; u="$2"; shift 2; \
      curl -fsSL --connect-timeout 15 --speed-limit 10000 --speed-time 60 --max-time "$t" --retry 3 --retry-delay 2 --retry-all-errors "$@" "$u" \
        || { echo "could not download $u: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
    }; \
    $apt update; \
    $apt install -y --no-install-recommends ca-certificates curl gpg; \
    fetch 120 "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x${GIT_PPA_KEY}" -o /tmp/git-ppa.asc; \
    got="$(gpg --batch --show-keys --with-colons /tmp/git-ppa.asc | awk -F: '/^pub:/{p=1;next} /^fpr:/&&p{print $10;exit}')"; \
    [ "$got" = "$GIT_PPA_KEY" ] || { echo "the git PPA key is $got, expected $GIT_PPA_KEY" >&2; exit 1; }; \
    install -d -m 0755 /etc/apt/keyrings; \
    install -m 0644 /tmp/git-ppa.asc /etc/apt/keyrings/git-core-ppa.asc; \
    rm /tmp/git-ppa.asc; \
    . /etc/os-release; \
    printf 'Types: deb\nURIs: https://ppa.launchpadcontent.net/git-core/ppa/ubuntu\nSuites: %s\nComponents: main\nSigned-By: /etc/apt/keyrings/git-core-ppa.asc\n' "$VERSION_CODENAME" \
      > /etc/apt/sources.list.d/git-core-ppa.sources; \
    printf 'Package: *\nPin: release o=LP-PPA-git-core\nPin-Priority: 1\n\nPackage: git git-man\nPin: release o=LP-PPA-git-core\nPin-Priority: 990\n' \
      > /etc/apt/preferences.d/git-core-ppa; \
    $apt update; \
    $apt install -y --no-install-recommends \
        git openssh-client \
        ripgrep jq less vim procps unzip \
        build-essential python3 python3-venv \
        tmux zstd \
        ncurses-term; \
    apt-mark auto gpg >/dev/null; \
    $apt autoremove --purge -y; \
    rm -rf /var/lib/apt/lists/*; \
    git --version

# caboose:section node Node.js, npm and corepack (for npx-launched MCP servers)
# Node. Not for Claude Code — that is a self-contained native binary — but for
# npx-launched MCP servers and for JS checkouts under a root, some of
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
# checkouts under a root pin pnpm via corepack, call node/tsx/vitest
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
# unless `engine_socket` mounts a socket for it to talk to, which is off
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
