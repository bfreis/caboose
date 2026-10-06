# check=skip=InvalidDefaultArgInFrom,UndefinedVar
# The caboose layer: what turns a base image into the sandbox. The base is
# either the embedded Dockerfile's image (built first, and tagged
# caboose-base:<env>) or the `base` of config.toml's [image], the user's own; the launcher
# builds this FROM it, and only after the image check has passed.
#
# It installs nothing. The base is the user's to fill (see the README's
# "Requirements for your own image"), so all this adds is what cannot be part
# of a generic image: the agent user with the host's UID/GID and its home,
# the entrypoint, and tmux.conf. Every step runs with what the check
# guarantees -- /bin/sh, mkdir, chown -- and in exec form, so a SHELL the base
# sets does not apply to it.
#
# The IDs are CABOOSE_UID and CABOOSE_GID, not UID and GID: the RUN below
# goes through /bin/sh, which on some images is bash, and bash sets UID
# itself -- to 0 during a build -- whatever the environment says.
#
# The check= directive on line 1 (a parser directive, so it has to come before
# any comment) silences two build warnings that are this file working as
# meant: BASE has no default because the launcher always passes it, naming
# the base it has just checked; and LANG, below, is read precisely in case the
# base leaves it unset.
ARG BASE
FROM ${BASE}

ARG CABOOSE_UID
ARG CABOOSE_GID

# The base may leave any user in place; the setup edits /etc as root.
USER 0:0

# tmux wraps Claude Code's full-screen TUI; without this it adds a status bar
# and clamps the palette. System-wide so it needs no state in the data dir.
COPY tmux.conf /etc/tmux.conf

# The launcher execs this path (and the Makefile recognises the sandbox by
# it). COPY keeps the mode the launcher wrote into the context, 0755.
COPY entrypoint.sh /usr/local/bin/caboose-entrypoint

# The agent user and its home: layer-user.sh has the rules, including what
# happens to an entry that already holds the UID or GID. It stays in the
# image, as a record of how the user was made.
COPY layer-user.sh /usr/local/lib/caboose/layer-user.sh

# caboose-agent, the sandbox's end of the link to the host (caboose link):
# static, so it needs nothing of the base. TARGETARCH is the build's own
# architecture, which BuildKit sets; the context holds both builds.
ARG TARGETARCH
COPY agent-bin/caboose-agent-linux-${TARGETARCH} /usr/local/bin/caboose-agent

# The shell.d loader, which the ~/.bashrc layer-user.sh sets up sources.
COPY shellrc.bash /usr/local/lib/caboose/shellrc.bash
RUN ["/bin/sh", "-c", "exec /bin/sh /usr/local/lib/caboose/layer-user.sh \"$CABOOSE_UID\" \"$CABOOSE_GID\""]

# Bind-mounted from the host (see the launcher for the full list). Claude Code
# then persists across containers:
#   ~/.claude/                 settings.json, CLAUDE.md, skills, agents,
#                              plugins, transcripts, .credentials.json
#   ~/.claude.json             OAuth session, user MCP servers, project trust
#   ~/.local/share/claude/     the versioned native binaries (~220MB each)
#   ~/.local/bin/claude        launcher symlink the updater repoints
#   ~/.cache/claude/           update staging, kept beside the versions it
#                              feeds (a separate mount, so a move from it into
#                              the versions dir crosses mounts)
#   ~/.config/{git,jj,gh}/     git/jj/gh config and the gh token -- mounted as
#                              directories, because these tools save by rename
#                              and a single-file mount refuses that (EBUSY).
#                              Nothing here may create ~/.gitconfig: git only
#                              writes ~/.config/git/config while it is absent.
# The three Claude Code mounts come from the data dir's dot_local/<platform>,
# the platform being the image check's, recorded as a label on this image.
# Deliberately NOT persisted: ~/.local/state/claude/locks, which is per-boot
# lock state and should die with the container.
ENV HOME=/home/agent

# ~/.local/bin first, for the claude symlink. A base with no PATH of its own
# gets the usual one rather than a trailing ':', which would put the cwd on
# it.
ENV PATH=/home/agent/.local/bin:${PATH:-/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}

# Stock images tend to leave LANG unset, so LC_CTYPE is POSIX. The tmux path
# gets away with it because the launcher passes `tmux -u`, but
# `tmux = false` has no equivalent -- which would make the one path you
# reach for when rendering looks wrong the one path guaranteed to mangle
# box-drawing characters. It also leaves every non-Python subprocess in the C locale
# (Python 3 coerces itself out of it, per PEP 538). C.UTF-8 is built into
# glibc since 2.35 and shipped by Debian and Ubuntu before that, and musl is
# UTF-8 whatever LANG says. A base that sets LANG keeps its own: the
# substitution sees the base's environment.
ENV LANG=${LANG:-C.UTF-8}

USER agent
WORKDIR /work

# Setting ENTRYPOINT also clears any CMD the base had; the launcher passes the
# entrypoint's arguments itself.
ENTRYPOINT ["/usr/local/bin/caboose-entrypoint"]
