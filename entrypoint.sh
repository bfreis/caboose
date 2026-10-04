#!/usr/bin/env bash
# caboose container entrypoint. Two roles:
#
#   caboose-entrypoint --cc-supervise
#       PID 1 of the long-lived container. Bootstraps Claude Code into the
#       bind-mounted ~/.local if it isn't there, clears runtime state left
#       behind by a previous container, signals readiness, then idles so the
#       container outlives any individual session.
#
#   caboose-entrypoint --cc-start
#       Runs ~/.config/caboose/start.d, as the supervisor does once it is
#       ready (run_start_scripts). In the foreground, for the tests.
#
#   caboose-entrypoint [claude args...]
#       What `docker exec` runs: hand off to the persisted claude binary.
set -euo pipefail

READY_FILE=/tmp/.caboose-ready
START_DIR="$HOME/.config/caboose/start.d"
CLAUDE_BIN="$HOME/.local/bin/claude"
VERSIONS_DIR="$HOME/.local/share/claude/versions"
# Each version is a ~224MB self-contained binary and the updater never removes
# the one it replaced. Keep the active one plus enough history to roll back.
KEEP_VERSIONS="${CABOOSE_KEEP_VERSIONS:-2}"

log() { printf 'caboose: %s\n' "$*" >&2; }

# Under isolation vm with egress_proxy on, HTTPS_PROXY names caboose-agent's
# outbound proxy on the VM's loopback, which serves only while the host's
# link is up -- and a launch links the VM as it boots, so it may come up
# just after this. A download before the sandbox is ready (the first
# install below) waits for it, a minute at most, rather than fail on a
# refused connection. The agent is caboose's own, in the layer.
AGENT=/usr/local/bin/caboose-agent
wait_for_proxy() {
    [ "${CABOOSE_ISOLATION:-}" = vm ] || return 0
    case "${HTTPS_PROXY:-}" in http://127.0.0.1:*) ;; *) return 0 ;; esac
    [ -x "$AGENT" ] || return 0
    "$AGENT" wait-proxy 60 >/dev/null 2>&1 && return 0
    log "the outbound proxy ($HTTPS_PROXY) is not serving: no host linked yet? trying anyway"
}

# Anthropic's native installer drops the binary in ~/.local/share/claude/
# versions/<v> and points ~/.local/bin/claude at it. Both are bind-mounted from
# the data dir's local/<platform>, so this runs once per platform for the
# life of the data dir — not once per image rebuild.
ensure_claude_installed() {
    if [ -e "$CLAUDE_BIN" ]; then
        return 0
    fi
    log "no Claude Code in the persistent data dir — installing the native build"
    log "(one-time, ~220MB; it self-updates in place from here on)"
    # Fetch to a file rather than `curl ... | bash </dev/null`: the redirect
    # would apply to bash, replacing the piped script with an empty stdin and
    # killing curl with SIGPIPE. The installer still needs stdin closed so it
    # never waits on a prompt, hence the redirect on the file invocation.
    wait_for_proxy
    local installer
    installer="$(mktemp)"
    if ! curl -fsSL https://claude.ai/install.sh -o "$installer"; then
        rm -f "$installer"
        log "ERROR: could not download the installer"
        return 1
    fi
    bash "$installer" </dev/null || { rm -f "$installer"; return 1; }
    rm -f "$installer"
    if [ ! -e "$CLAUDE_BIN" ]; then
        log "ERROR: installer finished but $CLAUDE_BIN is missing"
        return 1
    fi
}

# The fleet/agents machinery registers sessions by PID and rendezvous socket:
#   ~/.claude/daemon.lock          the supervisor's PID
#   ~/.claude/daemon/roster.json   worker PIDs + /tmp socket paths
#   ~/.claude/sessions/<pid>.json  one file per live session
# ~/.claude is bind-mounted, so those files survive a container restart while
# the processes and /tmp sockets they name do not. Left in place, a stale
# daemon.lock convinces the new container that a supervisor is already running
# and PIDs get reused against unrelated processes. Everything here is runtime
# state that a live Claude Code regenerates; none of it is user data.
clear_stale_runtime_state() {
    local n=0
    for f in "$HOME/.claude/daemon.lock" \
             "$HOME/.claude/daemon.status.json" \
             "$HOME/.claude/daemon.json" \
             "$HOME/.claude/daemon/roster.json"; do
        if [ -e "$f" ]; then rm -f "$f" && n=$((n + 1)); fi
    done
    if [ -d "$HOME/.claude/sessions" ]; then
        # bash leaves an unmatched glob literal, and rm -f on a literal is a
        # no-op, so an empty sessions dir needs no guard.
        for f in "$HOME"/.claude/sessions/*.json "$HOME"/.claude/sessions/*.key; do
            if [ -e "$f" ]; then rm -f "$f" && n=$((n + 1)); fi
        done
    fi
    if [ "$n" -gt 0 ]; then
        log "cleared $n stale session/daemon file(s) from a previous container"
    fi
}

# version_newer A B: whether A is the later version, as `sort -V` orders the
# dotted versions the installer names its files by (2.1.9 < 2.1.10 < 2.2.0).
# Field by field: by the fields' leading digits, numerically (10#, so a
# leading zero is not octal), and when those tie, as strings (2.1.7 <
# 2.1.7b); when one runs out of fields first it is the earlier (2.1 < 2.1.0).
version_newer() {
    local -a a b
    local k x y xn yn
    IFS=. read -ra a <<<"$1"
    IFS=. read -ra b <<<"$2"
    for ((k = 0; k < ${#a[@]} || k < ${#b[@]}; k++)); do
        x="${a[k]-}" y="${b[k]-}"
        [ "$x" = "$y" ] && continue
        [ -n "$x" ] || return 1
        [ -n "$y" ] || return 0
        xn="${x%%[!0-9]*}" yn="${y%%[!0-9]*}"
        if [ -n "$xn" ] && [ -n "$yn" ] && ((10#$xn != 10#$yn)); then
            ((10#$xn > 10#$yn)) && return 0
            return 1
        fi
        [[ $x > $y ]] && return 0
        return 1
    done
    return 1
}

# Retain the newest $KEEP_VERSIONS versions, always including whichever one
# ~/.local/bin/claude currently resolves to (so a rollback target is never the
# thing we delete, even if the symlink points at something old).
#
# Called from the supervisor at container start, when no session exists yet, so
# nothing has one of these binaries open. On-demand pruning is also safe --
# unlinking a running executable on Linux keeps the inode alive for the running
# process -- but start-up is the moment with nothing to reason about.
prune_old_versions() {
    [ -d "$VERSIONS_DIR" ] || return 0
    if ! [[ "$KEEP_VERSIONS" =~ ^[0-9]+$ ]] || [ "$KEEP_VERSIONS" -lt 1 ]; then
        log "CABOOSE_KEEP_VERSIONS='$KEEP_VERSIONS' is not a positive integer; not pruning"
        return 0
    fi

    local active=""
    if [ -L "$CLAUDE_BIN" ]; then
        active="$(readlink -f "$CLAUDE_BIN" 2>/dev/null || true)"
        active="${active##*/}"
    fi

    # Newest first, by version and not lexically: 2.1.9 must rank below
    # 2.1.10. An insertion sort on version_newer rather than `ls | sort -V`,
    # because BusyBox's sort may lack -V and the image is the user's to pick.
    # The glob skips dotfiles; -L keeps a dangling symlink.
    local versions=() v i
    for v in "$VERSIONS_DIR"/*; do
        [ -e "$v" ] || [ -L "$v" ] || continue
        v="${v##*/}"
        i=${#versions[@]}
        while [ "$i" -gt 0 ] && version_newer "$v" "${versions[i-1]}"; do
            versions[i]="${versions[i-1]}"
            i=$((i - 1))
        done
        versions[i]="$v"
    done

    local kept=0 removed=0
    for v in "${versions[@]}"; do
        if [ "$v" = "$active" ] || [ "$kept" -lt "$KEEP_VERSIONS" ]; then
            kept=$((kept + 1))
            continue
        fi
        # Versions are plain files today; tolerate a directory layout anyway
        # without reaching for a recursive force-delete.
        if [ -d "$VERSIONS_DIR/$v" ]; then
            find "$VERSIONS_DIR/$v" -mindepth 1 -delete 2>/dev/null || true
            rmdir "$VERSIONS_DIR/$v" 2>/dev/null || true
        else
            rm -f -- "$VERSIONS_DIR/$v"
        fi
        [ -e "$VERSIONS_DIR/$v" ] || removed=$((removed + 1))
    done

    if [ "$removed" -gt 0 ]; then
        log "pruned $removed old version(s); kept $kept (active: ${active:-none})"
    fi
    return 0
}

# ~/.claude/downloads is the installer's staging area and is bind-mounted, so a
# failed install can leave a ~224MB binary parked there. The installer removes
# its own download on success; this only catches the wreckage.
clear_installer_downloads() {
    local dir="$HOME/.claude/downloads" f n=0
    [ -d "$dir" ] || return 0
    for f in "$dir"/*; do
        if [ -e "$f" ]; then rm -f -- "$f" && n=$((n + 1)); fi
    done
    [ "$n" -gt 0 ] && log "cleared $n stray installer download(s)"
    return 0
}

# The user's start-up scripts: ~/.config/caboose/start.d, mounted from the
# data dir's home/.config/caboose/start.d. One at a time, in name order (byte
# order, whatever the locale), like run-parts: each runs to its end before
# the next starts, so 20-b may count on what 10-a did, and a daemon is
# started in the background by its script (`foo &`). Every
# executable file runs; dotfiles, backups (*~) and anything not executable
# are skipped. Their output goes to the container's log (`caboose logs`)
# as it is, unprefixed: a pipe through a prefixer would stay open as long
# as a daemon holding it lives, and never end. A script that fails is
# logged, and the next one runs.
#
# The supervisor runs this in the background, after the ready file, so a
# slow script does not hold up a launch. What a script leaves running is in
# the supervisor's process group, which the launcher has tini signal as a
# whole (TINI_KILL_PROCESS_GROUP), so a daemon gets its TERM on docker stop.
run_start_scripts() {
    [ -d "$START_DIR" ] || return 0
    local f name rc files lc_all="${LC_ALL-}" had_lc_all="${LC_ALL+x}"
    # The glob sorts by the collation; C is byte order. LC_ALL, since it
    # would override LC_COLLATE, and put back before any script runs.
    LC_ALL=C
    files=("$START_DIR"/*)
    if [ -n "$had_lc_all" ]; then LC_ALL="$lc_all"; else unset LC_ALL; fi
    for f in "${files[@]}"; do
        name="${f##*/}"
        case "$name" in *~) continue ;; esac
        [ -f "$f" ] && [ -x "$f" ] || continue
        log "start.d/$name: running"
        rc=0
        (cd "$HOME" && exec "$f") </dev/null || rc=$?
        if [ "$rc" -ne 0 ]; then
            log "start.d/$name: exited $rc"
        fi
    done
    return 0
}

# Under isolation vm (level 3) the sandbox is root in a VM of its own, and
# runs its own dockerd, when the image has one (the default Dockerfile's
# dockerd section): no host socket, no Docker-in-Docker. Its storage is a
# disk the launcher mounts at /var/lib/docker, kept across restarts, since
# overlayfs cannot sit on the virtio-fs shares. It runs in this process
# group, so a stop reaches it. A dockerd that does not come up is said, and
# the sandbox goes on without it.
DOCKERD_LOG=/var/log/caboose-dockerd.log
start_dockerd() {
    [ "${CABOOSE_ISOLATION:-}" = vm ] && [ "$EUID" -eq 0 ] || return 0
    command -v dockerd >/dev/null 2>&1 || return 0
    if ! grep -qs ' /var/lib/docker ' /proc/mounts; then
        log "no disk at /var/lib/docker: not starting dockerd"
        return 0
    fi
    dockerd --data-root /var/lib/docker >>"$DOCKERD_LOG" 2>&1 &
    local i
    for ((i = 0; i < 300; i++)); do
        if [ -S /var/run/docker.sock ]; then
            log "dockerd is up (its log: $DOCKERD_LOG)"
            return 0
        fi
        kill -0 $! 2>/dev/null || break
        sleep 0.1
    done
    log "dockerd did not come up; its log, $DOCKERD_LOG, says why"
}

case "${1:-}" in
    --cc-prune)
        prune_old_versions
        clear_installer_downloads
        ;;
    --cc-start)
        run_start_scripts
        ;;
    --cc-supervise)
        ensure_claude_installed
        clear_stale_runtime_state
        prune_old_versions
        clear_installer_downloads
        start_dockerd
        : > "$READY_FILE"
        log "ready — $("$CLAUDE_BIN" --version 2>/dev/null || echo 'claude version unknown')"
        run_start_scripts &
        # No `exec`: a bare `sleep` as PID 1 ignores SIGTERM, making
        # `docker stop` wait out its full timeout before SIGKILL. Trap it.
        trap 'exit 0' TERM INT
        while :; do sleep 86400 & wait $! || true; done
        ;;
    *)
        # Normally a no-op: the supervisor installed it at container start.
        # Report the failure rather than letting exec report a bare
        # "no such file or directory" for a path nobody asked about.
        if ! ensure_claude_installed >/dev/null 2>&1; then
            log "ERROR: $CLAUDE_BIN is missing and installing it failed."
            log "       Check network access, then see 'caboose logs'."
            exit 1
        fi
        exec "$CLAUDE_BIN" "$@"
        ;;
esac
