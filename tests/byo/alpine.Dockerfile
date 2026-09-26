# A musl test base for tests/byo/run.sh: stock Alpine plus exactly what
# "Requirements for your own image" in docs/images.md asks for on musl, and
# nothing else. A session on this image running `claude --version` is what
# proves Claude Code's musl build runs under caboose.
#
# Built with no context (docker build - < this file); nothing is COPYed.
# FROM_IMAGE is passed by run.sh, which also checks the stock image
# unmodified; the default here is the same pin, for building by hand.
ARG FROM_IMAGE=alpine:3.24
FROM ${FROM_IMAGE}

# No coreutils, findutils or GNU sed/grep: BusyBox already provides every
# tool the probe (imagecheck.sh) checks for, with the behaviour it tests for
# real -- /usr/bin/env, readlink -f, mktemp -d, find -mindepth 1 -delete, rm,
# rmdir, sleep, /usr/bin/test, uname, mkdir, chmod, cut, sed, tr, grep, head,
# sha256sum and chown are all BusyBox applets in Alpine 3.24's minirootfs
# (checked by running its busybox on exactly those invocations). Adding
# coreutils would only hide a probe that stopped accepting BusyBox.
#
# Nor ncurses: tic is optional, and this image is the one that covers its
# absence (the launcher falls back to TERM=xterm-256color, which tmux's
# ncurses-terminfo-base dependency has).
#
# ca-certificates-bundle is already in the stock image (it is how the probe
# finds /etc/ssl/certs/ca-certificates.crt there); it is named here because
# the CA certificates are a requirement, and curl depends on it anyway.
#
# What stock Alpine lacks, and check-image must name: bash, curl, tmux,
# git (sync runs it in the container), and the musl build's runtime needs -- libgcc (libgcc_s.so.1), libstdc++
# (libstdc++.so.6) and ripgrep (the system rg, which USE_BUILTIN_RIPGREP=0
# points Claude Code at).
RUN apk add --no-cache bash curl ca-certificates-bundle tmux git libgcc libstdc++ ripgrep
