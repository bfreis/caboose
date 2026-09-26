# A glibc test base for tests/byo/run.sh: stock Debian plus exactly what
# "Requirements for your own image" in docs/images.md asks for, and nothing
# else -- no toolchains, none of what the embedded Dockerfile adds. It is the
# smallest image caboose has to accept, so a requirement the probe forgets to
# check shows up here as a failed boot or a claude that will not run.
#
# Built with no context (docker build - < this file); nothing is COPYed.
# FROM_IMAGE is passed by run.sh, which also checks the stock image
# unmodified; the default here is the same pin, for building by hand.
ARG FROM_IMAGE=debian:13.7-slim
FROM ${FROM_IMAGE}

# Everything else the probe (imagecheck.sh) looks for is already in the
# stock image: /bin/sh (dash), /usr/bin/env, readlink -f, mktemp, rm, rmdir,
# sleep, test, uname, mkdir, chmod, cut, tr, head, sha256sum and chown
# (coreutils), find -delete (findutils), sed, grep, and glibc -- all
# Essential packages. So is bash, which is listed anyway because it is a
# requirement, not an accident of Debian. tic (ncurses-bin, optional) is
# Essential too, so this image covers the tic-present path; the Alpine one
# covers its absence.
#
# What stock Debian lacks, and check-image must name: curl, tmux, git
# (sync runs it in the container) and the CA certificates.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash curl ca-certificates tmux git \
 && rm -rf /var/lib/apt/lists/*
