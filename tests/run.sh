#!/usr/bin/env bash
# Integration tests for the caboose sandbox.
#
# These drive the REAL container and data dir: they recreate the container and
# plant decoy version files. That kills running sessions, so the suite refuses
# to start while any tmux session exists unless FORCE=1.
#
#   tests/run.sh          run everything
#   FORCE=1 tests/run.sh  run even with live sessions
#   ONLY=vm tests/run.sh  run the isolation vm group alone (a Mac; `make
#                         test-vm`), which uses a throwaway environment and
#                         leaves the real one alone; VM_TEST_BUILD_TIMEOUT,
#                         VM_TEST_BOOT_TIMEOUT and VM_TEST_CMD_TIMEOUT (seconds)
#                         bound its steps
#   CABOOSE_BIN=path/to/caboose tests/run.sh
#                         test another launcher build (relative to the repo)
#
# The launcher is a build product, so run this through `make test`, which
# rebuilds ./caboose and the image first.
# Check descriptions name container paths as ~/..., text that is printed and
# never meant to expand.
# shellcheck disable=SC2088
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
CC="${CABOOSE_BIN:-$ROOT/caboose}"
case "$CC" in /*) ;; *) CC="$ROOT/$CC" ;; esac
pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n'   "$1"; fail=$((fail + 1)); }
check() { # check <description> <expected> <actual>
    if [ "$2" = "$3" ]; then ok "$1"; else bad "$1"; printf '        expected: %s\n        actual:   %s\n' "$2" "$3"; fi
}
group() { printf '\n\033[1m%s\033[0m\n' "$1"; }
# `[ -e X ]; echo $?` reads naturally but trips SC2319: that $? is a
# condition's status rather than a command's, and is easy to clobber by
# accident. Same 0/1 answer, stated outright.
exists() { if [ -e "$1" ]; then echo 0; else echo 1; fi; }

# --- isolation = vm ---------------------------------------------------------
# Defined up here and run last, or alone (ONLY=vm): unlike every other group
# it never touches the real environment. Each launcher call gets a throwaway
# CABOOSE_HOME, data dir and repo root (one mktemp -d under /tmp, short
# enough for the VM's socket path), and a VM and image named
# caboose-vm-test, with none of the caller's CABOOSE_* settings. The image is
# built from scratch in the builder guest, so this group takes a while.
#
# A VM boot can hang where a container start cannot, so every launcher call
# here is bounded in time (bounded), each kind by its own VM_TEST_*_TIMEOUT,
# and the teardown stops whatever vmm and link the run left behind.
VM_NAME=caboose-vm-test
VM_T_BUILD="${VM_TEST_BUILD_TIMEOUT:-3600}"  # the image, base and layer, in the builder
VM_T_BOOT="${VM_TEST_BOOT_TIMEOUT:-1500}"    # create and boot; the first waits for Claude Code
VM_T_CMD="${VM_TEST_CMD_TIMEOUT:-180}"       # a command in a running VM, stop, sync, prune
# The caller's CABOOSE_HOME, read before any of this changes it: where the
# kernel and the builder disk may be, besides the checkout's vm-dist/.
VM_REAL_HOME="${CABOOSE_HOME:-$HOME/.caboose}"
VM_WORK="" VM_PROJ="" VM_DATA=""
VM_UNSET=() VM_ENV=()

# bounded SECS WHAT CMD...: CMD, stopped after SECS seconds with status 124
# and a line on the terminal saying so, naming it WHAT. No timeout(1): a Mac
# has none. CMD runs in a process group of its own (set -m), which the
# watchdog stops whole: a child left running would hold CMD's output open,
# and a $(...) would wait for it. The watchdog polls, so nothing of it
# outlives CMD by more than a second, and it holds none of that output.
bounded() {
    local secs=$1 what=$2 flag pid w s; shift 2
    flag="$(mktemp "${TMPDIR:-/tmp}/caboose-bounded.XXXXXX")" && rm -f "$flag"
    set -m
    "$@" </dev/null &
    pid=$!
    set +m
    (
        n=0
        while [ "$n" -lt "$secs" ]; do
            sleep 1; kill -0 "$pid" 2>/dev/null || exit 0; n=$((n + 1))
        done
        : > "$flag"; kill -TERM -- "-$pid"; sleep 10; kill -KILL -- "-$pid"
    ) </dev/null >/dev/null 2>&1 &
    w=$!
    wait "$pid"; s=$?
    kill "$w" 2>/dev/null; wait "$w" 2>/dev/null
    if [ -e "$flag" ]; then
        rm -f "$flag"
        printf '  \033[31mTIMEOUT\033[0m after %ss: %s\n' "$secs" "$what" >&9
        return 124
    fi
    return "$s"
}

# vcc SECS [NAME=VALUE...] ARGS...: the launcher in the vm group's throwaway
# environment, from its project, with NAME=VALUE added, bounded by SECS.
vcc() {
    local secs=$1; shift
    local extra=()
    while [ $# -gt 0 ] && [[ $1 =~ ^[A-Z_][A-Z0-9_]*= ]]; do extra+=("$1"); shift; done
    (cd "$VM_PROJ" && bounded "$secs" "caboose $*" env ${VM_UNSET[@]+"${VM_UNSET[@]}"} ${VM_ENV[@]+"${VM_ENV[@]}"} ${extra[@]+"${extra[@]}"} \
        "$CC" "$@" </dev/null)
}
# vsh CMD: CMD run by bash in the VM, its output as it printed it.
vsh() { vcc "$VM_T_CMD" shell -c "$1" 2>/dev/null | tr -d '\r'; }
# vstate: the VM's state as status reports it (running, exited, absent).
vstate() { vcc "$VM_T_CMD" status 2>/dev/null | sed -n 's/^VM *: .*(\(.*\))$/\1/p'; }

# vm_unavailable: why the vm group cannot run here, nothing when it can.
# The launcher says as much, but only once it is asked to start a VM, and a
# skip has to say why without trying.
vm_unavailable() {
    if [ "$(uname -s)" != Darwin ]; then
        echo "not a Mac: isolation vm runs on macOS only"; return
    fi
    if [ "$(uname -m)" != arm64 ]; then
        echo "not Apple silicon ($(uname -m)): isolation vm has no builder for it yet"; return
    fi
    # caboose-vmm sits beside the launcher, as the launcher finds it: after
    # its symlinks.
    local exe="$CC" t
    while [ -L "$exe" ]; do
        t="$(readlink "$exe")"
        case "$t" in /*) exe="$t" ;; *) exe="$(dirname "$exe")/$t" ;; esac
    done
    if [ ! -f "$(dirname "$exe")/caboose-vmm" ]; then
        echo "no caboose-vmm beside $exe ('make vmm' builds and signs it)"; return
    fi
    local f
    for f in kernel-arm64:vm-kernel builder-arm64.img:vm-builder; do
        if [ ! -f "$ROOT/vm-dist/${f%%:*}" ] && [ ! -f "$VM_REAL_HOME/vm/arm64/${f%%:*}" ]; then
            echo "no ${f%%:*} in $ROOT/vm-dist or $VM_REAL_HOME/vm/arm64 ('make ${f#*:}' writes it)"; return
        fi
    done
}

vm_setup() {
    VM_WORK="$(mktemp -d /tmp/caboose-vm.XXXXXX)" || return 1
    # Physical, as the launcher resolves the root: on a Mac /tmp is a
    # symlink to /private/tmp.
    VM_WORK="$(cd "$VM_WORK" && pwd -P)"
    VM_HOME="$VM_WORK/home"; VM_DATA="$VM_WORK/data"; VM_ROOT="$VM_WORK/root"; VM_PROJ="$VM_ROOT/proj"
    mkdir -p "$VM_HOME/vm/arm64" "$VM_DATA" "$VM_PROJ" || return 1
    # The kernel and the builder disk, when the caller's CABOOSE_HOME has
    # them: the launcher looks in CABOOSE_HOME/vm/<arch>, then the checkout.
    local f
    for f in kernel-arm64 builder-arm64.img; do
        [ -f "$VM_REAL_HOME/vm/arm64/$f" ] && ln -s "$VM_REAL_HOME/vm/arm64/$f" "$VM_HOME/vm/arm64/$f"
    done
    # None of the caller's settings: no CABOOSE_* of theirs, no FORCE, and
    # a CABOOSE_HOME of the group's own, so no config.toml of theirs either.
    VM_UNSET=(-u FORCE)
    local v
    for v in $(compgen -e); do
        case "$v" in CABOOSE_*) VM_UNSET+=(-u "$v") ;; esac
    done
    VM_ENV=(CABOOSE_HOME="$VM_HOME" CABOOSE_DATA_DIR="$VM_DATA" CABOOSE_REPO_ROOT="$VM_ROOT"
            CABOOSE_IMAGE="$VM_NAME" CABOOSE_CONTAINER="$VM_NAME" CABOOSE_ISOLATION=vm
            CABOOSE_READY_TIMEOUT="$VM_T_BOOT")
}

# vm_kill PID: ends a caboose process (vmm, the link) the run left behind.
# Only one whose name says caboose: a pid file can outlive its process, and
# its number be reused.
vm_kill() {
    local pid=$1
    case "$pid" in ''|*[!0-9]*) return 0 ;; esac
    [ "$pid" -gt 1 ] || return 0
    case "$(ps -p "$pid" -o comm= 2>/dev/null)" in *caboose*) ;; *) return 0 ;; esac
    kill -TERM "$pid" 2>/dev/null
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        kill -0 "$pid" 2>/dev/null || return 0
        sleep 1
    done
    kill -KILL "$pid" 2>/dev/null
    return 0
}
# vm_pids: the pids the VMs' and the link's files name.
vm_pids() {
    local f
    for f in "$VM_DATA"/vm/*/vmm.pid "$VM_DATA"/link.json; do
        [ -f "$f" ] && sed -n -e 's/.*"pid": *\([0-9][0-9]*\).*/\1/p' -e '/^ *[0-9][0-9]* *$/p' "$f" | head -1
    done
    return 0
}
# vm_teardown: stops the VM (bounded), then ends any vmm or link still
# running, and removes the throwaway dir. Called at the end of the group and
# again at exit, for a run cut short; a no-op once done.
vm_teardown() {
    [ -n "$VM_WORK" ] && [ -d "$VM_WORK" ] || return 0
    vcc "$VM_T_CMD" FORCE=1 stop >/dev/null 2>&1
    local pid
    for pid in $(vm_pids); do vm_kill "$pid"; done
    rm -rf "$VM_WORK" 2>/dev/null \
        || printf '\n\033[31mWARNING\033[0m could not remove %s; delete it by hand.\n' "$VM_WORK" >&2
    VM_WORK=""
    return 0
}
# vm_show FILE: the end of a log a failed step left, for the reader.
vm_show() { [ -s "$1" ] && tail -n 15 "$1" | sed 's/^/        | /'; return 0; }

vm_group() {
    group 'isolation = vm'
    local why; why="$(vm_unavailable)"
    if [ -n "$why" ]; then
        printf '  \033[33mSKIP\033[0m %s\n' "$why"
        return 0
    fi
    exec 9>&2  # bounded's TIMEOUT line, whatever a check does with stderr
    if ! vm_setup; then
        bad 'set up a throwaway environment'
        return 0
    fi
    local out rc st

    # Bring-up: the image, then a command, which creates and boots the VM.
    # Nothing after this means anything without them.
    vcc "$VM_T_BUILD" build >"$VM_WORK/build.log" 2>&1; rc=$?
    check 'caboose build writes the image in the builder guest' 0 "$rc"
    if [ "$rc" -ne 0 ]; then vm_show "$VM_WORK/build.log"; vm_teardown; return 0; fi
    out="$(vcc "$VM_T_BOOT" shell -c 'echo up' 2>"$VM_WORK/boot.log" | tr -d '\r')"
    check 'a shell command creates and boots the VM, and runs in it' up "$out"
    if [ "$out" != up ]; then
        vm_show "$VM_WORK/boot.log"; vm_show "$VM_DATA/vm/$VM_NAME/console.log"
        vm_teardown; return 0
    fi
    st="$(vcc "$VM_T_CMD" status 2>/dev/null)"
    check 'status says the VM runs' running "$(printf '%s\n' "$st" | sed -n 's/^VM *: .*(\(.*\))$/\1/p')"
    check 'and that its isolation is vm' vm "$(printf '%s\n' "$st" | sed -n 's/^isolation : //p')"
    vcc "$VM_T_CMD" doctor --offline >"$VM_WORK/doctor.log" 2>/dev/null
    check 'doctor finds no problem with the isolation' 0 "$(grep -c '^  ✗ isolation' "$VM_WORK/doctor.log")"
    grep -E '^(  ✗ |    )isolation ' "$VM_WORK/doctor.log" | sed 's/^/        | /'

    # In the VM: who runs, where, and the files the host shares with it.
    check 'it is a Linux guest' Linux "$(vsh 'uname -s')"
    check "it runs as root in the guest, whose shares show every file as root's" 0 "$(vsh 'id -u')"
    check 'a shell starts in the project, at its path under /work' /work/proj "$(vsh pwd)"
    check 'that user writes a 600 file in ~/.claude' ok \
        "$(vsh 'f=~/.claude/caboose-test-600; umask 077; echo x > "$f" && [ "$(cat "$f")" = x ] && rm -f "$f" && echo ok')"
    vsh 'echo from the VM > /work/proj/caboose-test-root' >/dev/null
    check 'a file the VM writes under the root is on the host' 'from the VM' \
        "$(cat "$VM_PROJ/caboose-test-root" 2>/dev/null)"
    echo 'from the host' > "$VM_PROJ/caboose-test-host"
    check 'and one the host writes there is in the VM' 'from the host' "$(vsh 'cat /work/proj/caboose-test-host')"
    vsh 'echo kept > ~/.claude/caboose-test-keep' >/dev/null
    check 'what the VM writes to a kept path is in the data dir' kept \
        "$(cat "$VM_DATA/home/.claude/caboose-test-keep" 2>/dev/null)"

    # caboose sync, against a bare repo under the root: the sync's git runs
    # in the VM, which sees it at /work/sync-remote.git. Another machine is
    # a clone on the host.
    local remote="$VM_ROOT/sync-remote.git" probe="$VM_DATA/home/.config/caboose/caboose-test-sync" path
    git init -q --bare "$remote"
    mkdir -p "$(dirname "$probe")"
    echo 'from this machine' > "$probe"
    vcc "$VM_T_CMD" sync --remote /work/sync-remote.git >"$VM_WORK/sync.log" 2>&1; rc=$?
    check 'caboose sync pushes to a remote the VM reaches' 0 "$rc"
    [ "$rc" -eq 0 ] || vm_show "$VM_WORK/sync.log"
    path="$(git --git-dir="$remote" ls-tree -r --name-only main 2>/dev/null | grep 'caboose-test-sync$' | head -1)"
    check 'the remote has what syncs, as it is' 'from this machine' \
        "$(git --git-dir="$remote" show "main:$path" 2>/dev/null)"
    if [ -n "$path" ] && git clone -q -b main "$remote" "$VM_WORK/other" 2>/dev/null; then
        echo 'from another machine' > "$VM_WORK/other/$path"
        git -C "$VM_WORK/other" -c user.name=caboose-test -c user.email=caboose-test@example.invalid \
            -c commit.gpgsign=false commit -qam 'Sync from another machine' \
            && git -C "$VM_WORK/other" push -q origin HEAD:main
    fi
    vcc "$VM_T_CMD" sync >"$VM_WORK/sync.log" 2>&1; rc=$?
    check 'a second sync, with the remote it set' 0 "$rc"
    [ "$rc" -eq 0 ] || vm_show "$VM_WORK/sync.log"
    check 'takes what another machine pushed into the data dir' 'from another machine' "$(cat "$probe" 2>/dev/null)"
    check 'which the VM sees' 'from another machine' "$(vsh 'cat ~/.config/caboose/caboose-test-sync')"

    # caboose prune: the versions the VM's ~/.local holds, in the data dir.
    # The suite's decoys, as in 'version pruning'; the data dir is a
    # throwaway, so nothing real is at stake.
    local local_dir versions reals nreals v
    local_dir="$(printf '%s\n' "$st" | sed -n 's/^local dir *: //p')"
    versions="$local_dir/share/claude/versions"
    vm_versions() { ls -1 "$versions" 2>/dev/null | sort -Vr | tr '\n' ' ' | sed 's/ $//'; }
    reals="$(vm_versions)"
    nreals="$(ls -1 "$versions" 2>/dev/null | wc -l | tr -d ' ')"
    check 'Claude Code is installed in the data dir' 1 "$([ -n "$local_dir" ] && [ "$nreals" -gt 0 ] && echo 1 || echo 0)"
    for v in 0.0.9 0.0.10 0.0.11; do printf 'decoy' > "$versions/$v"; done
    vcc "$VM_T_CMD" CABOOSE_KEEP_VERSIONS=$((nreals + 2)) prune >/dev/null 2>&1; rc=$?
    check 'caboose prune runs in the VM' 0 "$rc"
    check 'and keeps the newest N by version order' "$reals 0.0.11 0.0.10" "$(vm_versions)"
    check 'claude still runs after it' 0 "$(vsh 'claude --version >/dev/null 2>&1; echo $?')"

    # caboose prune --docker: the VM's dockerd keeps everything on a disk of
    # its own in the data dir, and this deletes it for an empty one.
    local disk="$VM_DATA/vm/volumes/docker.img"
    local wait_dockerd='for i in {1..60}; do docker info >/dev/null 2>&1 && break; sleep 1; done'
    if [ "$(vsh 'command -v dockerd >/dev/null && echo yes')" = yes ]; then
        check 'dockerd in the VM builds an image' built \
            "$(vsh "$wait_dockerd; printf 'FROM scratch\nLABEL caboose.fixture=vm\n' | docker build -q -t caboose-vm-fixture - >/dev/null 2>&1 && docker image inspect caboose-vm-fixture >/dev/null 2>&1 && echo built")"
        check 'its disk is in the data dir' 0 "$(exists "$disk")"
        out="$(vcc "$VM_T_CMD" prune --docker 2>&1)"
        check 'prune --docker refuses to delete it non-interactively' 1 \
            "$(printf '%s\n' "$out" | grep -c 'refusing to delete it non-interactively')"
        check 'and leaves the VM running' running "$(vstate)"
        vcc "$VM_T_CMD" FORCE=1 prune --docker >"$VM_WORK/prune.log" 2>&1; rc=$?
        check 'FORCE=1 prune --docker deletes it' 0 "$rc"
        [ "$rc" -eq 0 ] || vm_show "$VM_WORK/prune.log"
        check 'stopping the VM first' exited "$(vstate)"
        check 'and making an empty disk in its place' 0 "$(exists "$disk")"
        check 'the next start has dockerd on the empty disk' gone \
            "$(vcc "$VM_T_BOOT" shell -c "$wait_dockerd; docker info >/dev/null 2>&1 || exit 1; docker image inspect caboose-vm-fixture >/dev/null 2>&1 && echo kept || echo gone" 2>/dev/null | tr -d '\r')"
    else
        printf '  \033[33mSKIP\033[0m the image has no dockerd: prune --docker only, with nothing to delete\n'
        vcc "$VM_T_CMD" FORCE=1 prune --docker >/dev/null 2>&1; rc=$?
        check 'FORCE=1 prune --docker succeeds' 0 "$rc"
    fi
    out="$(vcc "$VM_T_CMD" CABOOSE_ISOLATION=docker prune --docker 2>&1)"
    check 'prune --docker is refused under another isolation' 1 \
        "$(printf '%s\n' "$out" | grep -c 'prune --docker is for isolation vm')"

    # Stop, start, restart. A marker in the VM's directory tells a restart,
    # which recreates the VM and so its directory, from a stop and start,
    # which keep it.
    [ "$(vstate)" = running ] || vsh true >/dev/null
    : > "$VM_DATA/vm/$VM_NAME/caboose-test-marker"
    vcc "$VM_T_CMD" stop >/dev/null 2>&1; rc=$?
    check 'caboose stop stops the VM' "0 exited" "$rc $(vstate)"
    check 'a command starts it again' up "$(vcc "$VM_T_BOOT" shell -c 'echo up' 2>/dev/null | tr -d '\r')"
    check 'the same VM' 0 "$(exists "$VM_DATA/vm/$VM_NAME/caboose-test-marker")"
    vcc "$VM_T_BOOT" FORCE=1 restart >"$VM_WORK/restart.log" 2>&1; rc=$?
    check 'caboose restart recreates the VM, running' "0 running" "$rc $(vstate)"
    [ "$rc" -eq 0 ] || vm_show "$VM_WORK/restart.log"
    check 'a new one' 1 "$(exists "$VM_DATA/vm/$VM_NAME/caboose-test-marker")"
    check 'which still has what was kept' kept "$(vsh 'cat ~/.claude/caboose-test-keep')"

    # Teardown: the VM stops, and no vmm or link of this run is left.
    vcc "$VM_T_CMD" stop >/dev/null 2>&1; rc=$?
    check 'the VM stops at the end' "0 exited" "$rc $(vstate)"
    local pid left=0
    for pid in $(vm_pids); do
        case "$(ps -p "$pid" -o comm= 2>/dev/null)" in *caboose-vmm*) left=$((left + 1)) ;; esac
    done
    check 'and no vmm is left running' 0 "$left"
    vm_teardown
}

# ONLY=vm runs the vm group alone, before anything of the real environment
# is read: nothing else of the suite runs, so live sessions do not matter.
case "${ONLY:-}" in
    '') ;;
    vm)
        trap vm_teardown EXIT
        trap 'exit 130' INT TERM
        vm_group
        printf '\n%s passed, %s failed\n' "$pass" "$fail"
        [ "$fail" -eq 0 ]; exit
        ;;
    *) printf 'tests/run.sh: no group ONLY=%s; the one it can run alone is vm\n' "$ONLY" >&2; exit 2 ;;
esac

# Must track the launcher's own choices, which involve more than a default:
# a CABOOSE_* override, the environment's config.toml, the environment. Ask
# the launcher rather than restating all of that here; status only reads.
# Restated defaults are the fallback if it can't. Asked again after the
# bring-up restart, which may have mounted another platform's dir.
read_paths() {
    local status_now
    status_now="$("$CC" status 2>/dev/null || true)"
    DATA_DIR="$(printf '%s\n' "$status_now" | sed -n 's/^data dir *: //p')"
    CONTAINER="$(printf '%s\n' "$status_now" | sed -n 's/^container *: \(.*\) (.*)$/\1/p')"
    IMAGE="$(printf '%s\n' "$status_now" | sed -n 's/^image *: //p')"
    DATA_DIR="${DATA_DIR:-${CABOOSE_DATA_DIR:-$HOME/.caboose/envs/default/data}}"
    CONTAINER="${CONTAINER:-${CABOOSE_CONTAINER:-caboose}}"
    # The dir the container has mounted as ~/.local: local/<platform>.
    LOCAL_DIR="$(printf '%s\n' "$status_now" | sed -n 's/^local dir *: //p')"
    LOCAL_DIR="${LOCAL_DIR:-$DATA_DIR/local}"
    VERSIONS="$LOCAL_DIR/share/claude/versions"
    BIN="$LOCAL_DIR/bin"
    # What the sandbox keeps of its home, at its path under ~.
    HOME_DIR="$DATA_DIR/home"
    GIT_CFG="$HOME_DIR/.config/git/config"
    JJ_CFG="$HOME_DIR/.config/jj/config.toml"
    GH_DIR="$HOME_DIR/.config/gh"
}
read_paths


# Record the real symlink target so a mid-suite abort can't leave the install
# pointing at a decoy. Same for the tracked CLAUDE.md, which one test edits.
ORIGINAL_LINK="$(readlink "$BIN/claude" 2>/dev/null || true)"
CLAUDE_MD_BACKUP=""
restore() {
    if [ -n "$CLAUDE_MD_BACKUP" ] && [ -f "$CLAUDE_MD_BACKUP" ]; then
        cat "$CLAUDE_MD_BACKUP" > "$ROOT/sandbox/CLAUDE.md"
        rm -f "$CLAUDE_MD_BACKUP"
        CLAUDE_MD_BACKUP=""
    fi
    [ -n "$ORIGINAL_LINK" ] && ln -sfn "$ORIGINAL_LINK" "$BIN/claude"
    find "$VERSIONS" -maxdepth 1 -name '0.0.*' -delete 2>/dev/null
    if [ -n "$ORIGINAL_LINK" ] && [ ! -e "$VERSIONS/$(basename "$ORIGINAL_LINK")" ]; then
        printf '\n\033[31mWARNING\033[0m the real version is gone; next start will re-download it.\n' >&2
    fi
    return 0
}
# The config probes are cleaned up at exit, not in restore(): 'version pruning'
# calls restore() mid-suite, which sits between the 'tool config' group that
# writes the probes and the restart in 'stale runtime state' that checks they
# survived -- cleaning them there made those checks fail every time.
trap 'restore; cleanup_config_probes; vm_teardown' EXIT

# Throwaway keys the 'tool config' group writes through the container, removed
# here on the host so an aborted run cannot leave them behind. The jj key is
# top-level on purpose: unsetting a key inside a table leaves the empty table.
cleanup_config_probes() {
    git config --file "$GIT_CFG" --remove-section caboose-test >/dev/null 2>&1
    if [ -f "$JJ_CFG" ] && grep -q '^caboose-test-probe = ' "$JJ_CFG"; then
        docker exec "$CONTAINER" jj config unset --user caboose-test-probe >/dev/null 2>&1 \
            || sed -i.bak '/^caboose-test-probe = /d' "$JJ_CFG"
        rm -f "$JJ_CFG.bak"
    fi
    rm -f "$GH_DIR/caboose-test-probe"
    rm -f "$DATA_DIR"/home/.config/caboose/start.d/caboose-test-* \
          "$DATA_DIR"/home/.config/caboose/shell.d/caboose-test.*
    return 0
}

live_sessions="$(docker exec "$CONTAINER" tmux list-sessions 2>/dev/null | wc -l | tr -d ' ')"
if [ "${live_sessions:-0}" -gt 0 ] && [ -z "${FORCE:-}" ]; then
    printf 'refusing to run: %s live tmux session(s) would be killed.\n' "$live_sessions" >&2
    printf 'finish them, or re-run with FORCE=1.\n' >&2
    exit 1
fi

versions_now() { ls -1 "$VERSIONS" 2>/dev/null | sort -Vr | tr '\n' ' ' | sed 's/ $//'; }

group 'bring-up'
"$CC" restart >/dev/null 2>&1
read_paths
check 'container is running' running \
    "$(docker inspect --type=container -f '{{.State.Status}}' "$CONTAINER" 2>/dev/null)"
check 'claude resolves through the exec path' 0 \
    "$(docker exec "$CONTAINER" /usr/local/bin/caboose-entrypoint --version >/dev/null 2>&1; echo $?)"
check 'caboose-agent is in the image, for this architecture' 0 \
    "$(docker exec "$CONTAINER" /usr/local/bin/caboose-agent --help >/dev/null 2>&1; echo $?)"

group 'sandbox instructions'
# The launcher -- not the entrypoint, and not a bind mount -- installs the
# tracked CLAUDE.md, substituting the one placeholder in it. A stale copy or a
# surviving @@...@@ both point the agent at a path that does not exist.
installed="$HOME_DIR/.claude/CLAUDE.md"
check 'the tracked CLAUDE.md was installed' 0 "$(exists "$installed")"
check 'the placeholder was substituted' 0 \
    "$(grep -c '@@CABOOSE_DIR@@' "$installed" 2>/dev/null || true)"
# Where the container has this checkout: its place under whichever root
# status lists as holding it.
checkout_in_container=""
while IFS= read -r line; do
    host="${line#repo root : }"; ctr="${host##* -> }"; host="${host% -> *}"
    case "$ROOT/" in "${host%/}/"*) checkout_in_container="$ctr${ROOT#"${host%/}"}" ;; esac
done < <("$CC" status 2>/dev/null | grep '^repo root : ')
check 'the checkout is under a root, below /work' 0 \
    "$(case "$checkout_in_container" in /work|/work/*) echo 0 ;; *) echo 1 ;; esac)"
check 'it names this checkout at its container path' 0 \
    "$(grep -qF "$checkout_in_container" "$installed" 2>/dev/null; echo $?)"
check 'which is where the container has it' 0 \
    "$(docker exec "$CONTAINER" test -f "$checkout_in_container/tests/run.sh"; echo $?)"

# Installing per launch rather than per container start is the point: an edit
# has to reach the next session without a restart. shell is a launch
# path that needs no tty to have already done the work (it fails at the final
# docker exec here, which is fine -- the sync happens before that).
CLAUDE_MD_BACKUP="$(mktemp)"
cat "$ROOT/sandbox/CLAUDE.md" > "$CLAUDE_MD_BACKUP"
printf '\n<!-- canary -->\n' >> "$ROOT/sandbox/CLAUDE.md"
(cd "$ROOT" && "$CC" shell -c true) >/dev/null 2>&1
check 'an edit is installed on the next launch, without a restart' 0 \
    "$(grep -qF '<!-- canary -->' "$installed" 2>/dev/null; echo $?)"
cat "$CLAUDE_MD_BACKUP" > "$ROOT/sandbox/CLAUDE.md"
rm -f "$CLAUDE_MD_BACKUP"; CLAUDE_MD_BACKUP=""
(cd "$ROOT" && "$CC" shell -c true) >/dev/null 2>&1
check 'and reverting it is picked up just as fast' 1 \
    "$(grep -qF '<!-- canary -->' "$installed" 2>/dev/null; echo $?)"

group 'tool config lives in the data dir'
# git, jj and gh save their config by writing a temp file and renaming it
# over the original. That rename fails with EBUSY on a single-file bind mount
# (`gh auth login`: "could not write config file /home/agent/.gitconfig:
# Device or resource busy"), so these are mounted as
# directories at their XDG paths, and every write here has to succeed from
# inside AND land in the data dir.
cexec() { docker exec "$CONTAINER" "$@" 2>/dev/null | tr -d '\r'; }
check 'git config --global succeeds inside' 0 \
    "$(docker exec "$CONTAINER" git config --global caboose-test.probe yes >/dev/null 2>&1; echo $?)"
check 'and lands in the data dir' yes \
    "$(git config --file "$GIT_CFG" --get caboose-test.probe 2>/dev/null || true)"
# git writes ~/.config/git/config only while ~/.gitconfig is absent. One
# appearing -- from the image, or from the file not existing to begin with --
# would silently send every later write outside the mounts.
check 'no ~/.gitconfig shadows it' 1 \
    "$(docker exec "$CONTAINER" sh -c 'test -e "$HOME/.gitconfig"' 2>/dev/null; echo $?)"
check 'jj config set --user succeeds inside' 0 \
    "$(docker exec "$CONTAINER" jj config set --user caboose-test-probe yes >/dev/null 2>&1; echo $?)"
check 'and lands in the data dir' 1 \
    "$(grep -cx 'caboose-test-probe = "yes"' "$JJ_CFG" 2>/dev/null || true)"
# A ~/.jjconfig.toml would win: jj writes to it rather than the XDG file.
check 'jj writes nowhere but ~/.config/jj' "$(cexec sh -c 'echo "$HOME/.config/jj/config.toml"')" \
    "$(cexec jj config path --user)"
# gh has no keyring in here, so ~/.config/gh holds the token itself.
check '~/.config/gh is the data dir, from inside' 0 \
    "$(docker exec "$CONTAINER" sh -c 'touch "$HOME/.config/gh/caboose-test-probe"' >/dev/null 2>&1 \
       && exists "$GH_DIR/caboose-test-probe")"
check 'and only its owner can read it' drwx------ \
    "$(ls -ld "$GH_DIR" 2>/dev/null | cut -c1-10)"
check '~/.config itself belongs to the agent, not root' agent \
    "$(cexec stat -c %U /home/agent/.config)"
# sync runs git in here, on the data dir's sync/ mounted at ~/.caboose-sync.
check '~/.caboose-sync is the data dir'"'"'s sync/, from inside' 0 \
    "$(docker exec "$CONTAINER" sh -c 'touch "$HOME/.caboose-sync/caboose-test-probe"' >/dev/null 2>&1 \
       && exists "$DATA_DIR/sync/caboose-test-probe")"
rm -f "$DATA_DIR/sync/caboose-test-probe"
# Sessions write proposals into the data dir's proposals/, and read what the
# host last wrote into current/.
check '~/.caboose-proposals is the data dir'"'"'s proposals/, from inside' 0 \
    "$(docker exec "$CONTAINER" sh -c 'touch "$HOME/.caboose-proposals/.caboose-test-probe"' >/dev/null 2>&1 \
       && exists "$DATA_DIR/proposals/.caboose-test-probe")"
rm -f "$DATA_DIR/proposals/.caboose-test-probe"
check 'a launch tells sessions what a proposal is made against' 0 \
    "$(docker exec "$CONTAINER" test -f /home/agent/.caboose-proposals/current/state.toml >/dev/null 2>&1; echo $?)"
check 'git runs in the container (sync needs it)' 0 \
    "$(docker exec "$CONTAINER" git --version >/dev/null 2>&1; echo $?)"
# A Claude Code run outside tmux (CABOOSE_NO_TMUX, caboose claude -p) is seen, from
# docker top, as writing the data dir -- which is what keeps a sync away.
# A sleep named as the launcher symlink stands in for one.
docker exec -d "$CONTAINER" bash -c 'echo $$ > /tmp/caboose-test-claude.pid; exec -a /home/agent/.local/bin/claude sleep 60'
sleep 1
check 'a Claude Code run outside tmux is seen' 1 \
    "$("$CC" status 2>/dev/null | grep -c '^Claude Code outside tmux: [1-9]')"
docker exec "$CONTAINER" bash -c 'kill "$(cat /tmp/caboose-test-claude.pid)"; rm -f /tmp/caboose-test-claude.pid'

group 'version pruning'
# Decoys are 0.0.* -- deliberately BELOW every installed version, and KEEP is
# counted from however many real ones there are (the updater leaves the
# previous one behind, so there is often more than one), so no real 224MB
# binary is ever a prune candidate and a logic bug cannot delete one.
#
# The trap: 0.0.9 vs 0.0.10. Keeping the reals plus two, a correct version
# sort keeps {reals, 0.0.11, 0.0.10}; a lexical sort would rank 0.0.9 above
# both and keep {reals, 0.0.11, 0.0.9} instead.
real="$(basename "$ORIGINAL_LINK")"
find "$VERSIONS" -maxdepth 1 -name '0.0.*' -delete 2>/dev/null  # an aborted run's decoys
reals="$(versions_now)"
nreals="$(ls -1 "$VERSIONS" 2>/dev/null | wc -l | tr -d ' ')"
for v in 0.0.9 0.0.10 0.0.11; do printf 'decoy' > "$VERSIONS/$v"; done
CABOOSE_KEEP_VERSIONS=$((nreals + 2)) "$CC" prune >/dev/null 2>&1
check 'keeps newest N by version order, not lexically' "$reals 0.0.11 0.0.10" "$(versions_now)"

find "$VERSIONS" -maxdepth 1 -name '0.0.*' -delete 2>/dev/null
for v in 0.0.1 0.0.2; do printf 'decoy' > "$VERSIONS/$v"; done
ln -sfn "/home/agent/.local/share/claude/versions/0.0.1" "$BIN/claude"
CABOOSE_KEEP_VERSIONS=$nreals "$CC" prune >/dev/null 2>&1
check 'protects the active version even when it is oldest' "$reals 0.0.1" "$(versions_now)"
restore
check 'real version survived the suite' 0 "$(exists "$VERSIONS/$real")"
check 'and so did every other real one' "$reals" "$(versions_now)"

group 'start.d and shell.d'
# Written on the host, into the data dir, as a user would: two start-up
# scripts that must run one after the other at the restart below, in name
# order, and a shell.d pair that every bash must read, the .sh before the
# .bash. Checked after that restart, in this group's second half.
START_D="$HOME_DIR/.config/caboose/start.d"
SHELL_D="$HOME_DIR/.config/caboose/shell.d"
mkdir -p "$START_D" "$SHELL_D"
printf '#!/bin/sh\nsleep 1; echo one >> /tmp/caboose-test-start\n' > "$START_D/caboose-test-1"
printf '#!/bin/sh\necho two >> /tmp/caboose-test-start\n' > "$START_D/caboose-test-2"
printf '#!/bin/sh\necho never >> /tmp/caboose-test-start\n' > "$START_D/caboose-test-3"
chmod +x "$START_D/caboose-test-1" "$START_D/caboose-test-2"
printf 'caboose_test_shelld=sh\n' > "$SHELL_D/caboose-test.sh"
printf 'caboose_test_shelld="$caboose_test_shelld bash"\n' > "$SHELL_D/caboose-test.bash"

group 'stale runtime state'
mkdir -p "$HOME_DIR/.claude/sessions" "$HOME_DIR/.claude/daemon"
echo '{"pid":99999}' > "$HOME_DIR/.claude/daemon.lock"
echo '{"pid":99999}' > "$HOME_DIR/.claude/sessions/99999.json"
echo 'k'             > "$HOME_DIR/.claude/sessions/99999.abc.key"
echo '{}'            > "$HOME_DIR/.claude/daemon/roster.json"
"$CC" restart >/dev/null 2>&1
check 'PID-keyed session files cleared on restart' "" \
    "$(ls -1 "$HOME_DIR/.claude/sessions" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
check 'stale daemon.lock cleared' 1 "$(exists "$HOME_DIR/.claude/daemon.lock")"
check 'stale roster.json cleared'  1 "$(exists "$HOME_DIR/.claude/daemon/roster.json")"

group 'start.d and shell.d, after the restart'
for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ "$(cexec cat /tmp/caboose-test-start | wc -l | tr -d ' ')" -ge 2 ] && break
    sleep 1
done
check 'start.d ran its executables one at a time, in name order' 'one two' \
    "$(cexec cat /tmp/caboose-test-start | tr '\n' ' ' | sed 's/ $//')"
check 'and said so in the container log' 1 \
    "$(docker logs "$CONTAINER" 2>&1 | grep -c 'caboose: start.d/caboose-test-2: running')"
check 'tini signals the whole process group on stop' TINI_KILL_PROCESS_GROUP=1 \
    "$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$CONTAINER" | grep '^TINI_KILL_PROCESS_GROUP=')"
check 'an interactive bash reads shell.d, the .sh first' 'sh bash' \
    "$(cexec bash -ic 'echo "caboose-test:$caboose_test_shelld"' | sed -n 's/^caboose-test://p')"
check 'and so does a login bash (a tmux window)' 'sh bash' \
    "$(cexec bash -lc 'echo "caboose-test:$caboose_test_shelld"' | sed -n 's/^caboose-test://p')"
rm -f "$START_D"/caboose-test-* "$SHELL_D"/caboose-test.*
# The probes the 'tool config' group wrote from inside, read from inside a
# fresh container: what `gh auth login` and `git config --global` save has to
# outlive a recreation.
probes_before="$fail"
check 'git config survived the restart' yes \
    "$(docker exec "$CONTAINER" git config --global --get caboose-test.probe 2>/dev/null | tr -d '\r')"
check 'jj config survived the restart' yes \
    "$(docker exec "$CONTAINER" jj config get caboose-test-probe 2>/dev/null | tr -d '\r')"
check '~/.config/gh survived the restart' 0 \
    "$(docker exec "$CONTAINER" sh -c 'test -e "$HOME/.config/gh/caboose-test-probe"' 2>/dev/null; echo $?)"
if [ "$fail" -ne "$probes_before" ]; then
    # Tells a probe lost on the host from one the container cannot see.
    printf '        host side:\n'
    ls -la "$HOME_DIR/.config" "$GH_DIR" 2>&1 | sed 's/^/          /'
    grep -H caboose-test "$GIT_CFG" "$JJ_CFG" 2>&1 | sed 's/^/          /'
    printf '        container side:\n'
    docker exec "$CONTAINER" sh -c 'ls -la "$HOME/.config" "$HOME/.config/gh"; \
        grep -H caboose-test "$HOME/.config/git/config" "$HOME/.config/jj/config.toml"' \
        2>&1 | sed 's/^/          /'
fi
cleanup_config_probes

group 'tmux transparency'
# tmux must not own a single keystroke: C-b is the default prefix and is also
# the most-pressed readline key (backward-char), so interception is invisible
# until someone tries to move the cursor.
tmux_q() { docker exec "$CONTAINER" tmux -L test_chk "$@" 2>/dev/null | tr -d '\r'; }

# /etc/tmux.conf must load with zero complaints. A bad directive is only
# reported to an interactive client -- `new-session -d` stays silent and
# tmux carries on -- so the whole file has to be re-sourced into a server with
# default tables to surface it. Note the exit status is 0 even on error, so
# this asserts on OUTPUT, not on $?. It catches the likes of
# `unbind -a -T prefix2` (there is no prefix2 table).
conf_errors="$(docker exec "$CONTAINER" bash -c '
    tmux -L test_conf kill-server 2>/dev/null
    tmux -L test_conf -f /dev/null new-session -d -s s "sleep 20" >/dev/null 2>&1
    tmux -L test_conf source-file /etc/tmux.conf 2>&1
    tmux -L test_conf kill-server 2>/dev/null' | tr -d '\r')"
check '/etc/tmux.conf loads with no errors' "" "$conf_errors"
docker exec "$CONTAINER" tmux -L test_chk kill-server >/dev/null 2>&1
docker exec "$CONTAINER" tmux -L test_chk -u new-session -d -s s 'sleep 120' >/dev/null 2>&1
check 'no prefix key'                None "$(tmux_q show -gv prefix)"
check 'no secondary prefix key'      None "$(tmux_q show -gv prefix2)"
# tmux deletes a key table once it is fully emptied, so the expected end
# state is that `prefix` does not exist at all; either way, zero bindings.
check 'prefix key table has no bindings' 0 \
    "$(tmux_q list-keys -T prefix | wc -l | tr -d ' ')"
check 'prefix key table is gone entirely' "table prefix doesn't exist" \
    "$(docker exec "$CONTAINER" tmux -L test_chk list-keys -T prefix 2>&1 | tr -d '\r')"
# The root table fires without a prefix. Mouse entries are fine (and inert
# while mouse is off); a KEYBOARD entry there would silently eat a keystroke.
check 'root table has no keyboard bindings' 0 \
    "$(tmux_q list-keys -T root | grep -vcE 'Mouse|Wheel|Click' | tr -d ' ')"
check 'mouse off so events pass to the app' off "$(tmux_q show -gv mouse)"
check 'extended keys enabled'        on   "$(tmux_q show -sv extended-keys)"
check 'status bar off'               off   "$(tmux_q show -gv status)"
# Synchronized output: declared in tmux.conf so it does not depend on the
# container having a terminfo entry for the host's terminal.
check 'synchronized output declared' 1 \
    "$(tmux_q show -gv terminal-features | grep -c 'sync')"
docker exec "$CONTAINER" tmux -L test_chk kill-server >/dev/null 2>&1

group 'image hygiene'
check 'container locale is UTF-8, not POSIX' C.UTF-8 \
    "$(docker exec "$CONTAINER" printenv LANG 2>/dev/null | tr -d '\r')"
# The images are built from files embedded in the launcher, written to temp
# dirs, so only what embed.go lists can reach the daemon -- an allowlist that
# trades a silent credential leak for a loud build failure, but only if it
# keeps up with the COPYs. (go test ./internal/assets checks the other half,
# the BaseContext and LayerContext lists.)
# An embedded directory covers what is under it (agent-bin, whose COPY
# names the file by ${TARGETARCH}).
embedded="$(sed -n 's|^//go:embed ||p' "$ROOT/embed.go")"
missing=""
while IFS= read -r src; do
    found=""
    for e in $embedded; do
        case "$src" in "$e" | "$e"/*) found=1 ;; esac
    done
    [ -n "$found" ] || missing="$missing $src"
done < <(awk '$1 == "COPY" { print $2 }' "$ROOT/Dockerfile" "$ROOT/layer.Dockerfile")
check 'every COPY source is embedded in the launcher' "" "$missing"

# Persistent state must not sit inside the checkout, where only the tracked
# .gitignore would stand between the credential and the VCS -- and a checkout
# predating that file deletes it, un-ignores the state, and lets the next
# snapshot take it. Assert the default is outside $ROOT,
# reading it from the launcher itself rather than restating the path here.
data_default="$(cd "$ROOT" && env -u CABOOSE_DATA_DIR "$CC" status 2>/dev/null \
                | sed -n 's/^data dir *: //p')"
if [ -z "$data_default" ]; then
    data_where=unreported
else
    case "$data_default/" in
        "$ROOT"/*) data_where=inside ;;
        *)         data_where=outside ;;
    esac
fi
check 'the data dir defaults outside the checkout' outside "$data_where"
# Unless the suite runs on a data dir named outright, it is the default
# environment's.
if [ -z "${CABOOSE_DATA_DIR:-}" ]; then
    check 'the data dir is the default environment'"'"'s' "${CABOOSE_HOME:-$HOME/.caboose}/envs/default/data" "$DATA_DIR"
fi

group 'launcher version'
# version is what an upgrade is checked with, so it has to agree with the
# image the suite just built (make test runs build first): same image
# name as status, and the hashes the launcher embeds recorded on that image.
# Only the hashes are compared; the version label is informational.
ver_out="$(cd "$ROOT" && "$CC" version 2>/dev/null)"; ver_rc=$?
ver_field() { printf '%s\n' "$ver_out" | sed -n "s/^$1 *: //p"; }
check 'version succeeds' 0 "$ver_rc"
check 'version prints a version' 1 "$([ -n "$(ver_field version)" ] && echo 1 || echo 0)"
check 'status reports the same version' "$(ver_field version)" \
    "$(cd "$ROOT" && "$CC" status 2>/dev/null | sed -n 's/^version *: //p')"
check 'version names the image status does' "$IMAGE" "$(ver_field image)"
check 'the local image matches the launcher' matches "$(ver_field local | cut -d' ' -f1)"
check 'the container is on the local image' "$CONTAINER (running, on the local image)" \
    "$(ver_field container)"
label() { docker image inspect --format "{{index .Config.Labels \"$1\"}}" "$IMAGE" 2>/dev/null; }
check 'the image carries a version label' 1 \
    "$([ -n "$(label io.github.bfreis.caboose.version)" ] && echo 1 || echo 0)"
layer_label="$(label io.github.bfreis.caboose.layer-hash)"
base_label="$(label io.github.bfreis.caboose.base-hash)"
check 'the image carries a full layer hash' 64 "${#layer_label}"
# The environment may build its base from its own image/ dir instead of
# the embedded Dockerfile: the same kind of base to the suite, a full hash
# either way, and version says which.
base_kind=default
case "$(ver_field base)" in *"(built from "*) base_kind="env" ;; esac
check 'and, built on caboose'"'"'s base, a full base hash' 64 "${#base_label}"
check "and says outright it is on that base ($base_kind)" "$base_kind" "$(label io.github.bfreis.caboose.base-kind)"
check 'the image records the host user it was built for' "$(id -u):$(id -g)" \
    "$(label io.github.bfreis.caboose.uid):$(label io.github.bfreis.caboose.gid)"
# The suite runs on the default base, which is named <image>-base and
# checked and built on by ID: the labels say which.
BASE="$(ver_field base | cut -d' ' -f1)"
check 'version names the default base' "$IMAGE-base" "$BASE"
check 'its base-name label is that base' "$BASE" "$(label io.github.bfreis.caboose.base-name)"
check 'its base-id label is that base as it is now' \
    "$(docker image inspect --format '{{.Id}}' "$BASE" 2>/dev/null)" "$(label io.github.bfreis.caboose.base-id)"
check 'its platform label is the one the container mounts' \
    "$(cd "$ROOT" && "$CC" status 2>/dev/null | sed -n 's/^platform *: //p')" \
    "$(label io.github.bfreis.caboose.platform)"

group 'base and layer'
# The default base passes the check it is built on, and holds nothing of the
# layer's: no agent user, no entrypoint. The layer is where both come from.
check 'check-image passes the default base' 0 \
    "$(cd "$ROOT" && env -u CABOOSE_BASE_IMAGE "$CC" check-image >/dev/null 2>&1; echo $?)"
check 'the base has no entrypoint of ours' 1 \
    "$(docker run --rm --entrypoint /bin/sh "$BASE" -c 'test -e /usr/local/bin/caboose-entrypoint' >/dev/null 2>&1; echo $?)"
check 'the base has no agent user' 1 \
    "$(docker run --rm --entrypoint /bin/sh "$BASE" -c 'grep -q "^agent:" /etc/passwd' >/dev/null 2>&1; echo $?)"
# Files written into bind mounts must come out as the host user's, which is
# what the layer's UID/GID are for. Under gVisor on an engine that hides the
# mounts from the agent, the container runs as root instead (its user
# label says so), and the engine maps what root writes to the host user.
if [ "$(docker inspect --type=container -f '{{index .Config.Labels "io.github.bfreis.caboose.user"}}' "$CONTAINER")" = 0:0 ]; then
    check 'the container runs as root, as its label says' 0:0 "$(cexec id -u):$(cexec id -g)"
    check 'as root' root "$(cexec id -un)"
else
    check 'the container runs as the host UID' "$(id -u)" "$(cexec id -u)"
    check 'with the host GID as its group' "$(id -g)" "$(cexec id -g)"
    check 'as agent' agent "$(cexec id -un)"
fi
check "agent's home is /home/agent, and its shell bash" '/home/agent bash' \
    "$(cexec sh -c 'grep "^agent:" /etc/passwd | cut -d: -f6,7 | sed "s|:.*/| |"')"
check 'and it owns its home' "$(id -u)" "$(cexec stat -c %u /home/agent)"
# pam_unix (su, sudo) fails an account whose passwd says "x" but which has
# no shadow entry; the layer adds or renames one.
check 'the agent user has one shadow entry' 1 \
    "$(docker exec -u 0 "$CONTAINER" sh -c 'grep -c "^agent:" /etc/shadow' 2>/dev/null | tr -d '\r')"
check 'PATH starts with the agent'"'"'s .local/bin' /home/agent/.local/bin \
    "$(cexec sh -c 'printf %s "$PATH" | cut -d: -f1')"
check 'the glibc image is not told to use a system ripgrep' '' \
    "$(cexec printenv USE_BUILTIN_RIPGREP)"

group 'git identity'
# A launch never copies the host's git identity or signing into the
# sandbox: caboose setup asks for them, and a launch only says, in one
# line, that there is none. These drive the launcher with a throwaway data
# dir; logs is used because it reaches ensureRunning without touching the
# container, which bring-up has already started.
seed_tmp1="$(mktemp -d)"
seed_cfg=home/.config/git/config

# The host's global git config is a throwaway one, not yours, with an
# identity and SSH signing that a launch has to leave where they are.
seed_host="$(mktemp -d)"
seed_key='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICabooseTestKeyNotARealKeyAtAll0000000000 caboose-test'
printf '[user]\n\tname = Caboose Test\n\temail = caboose-test@example.invalid\n\tsigningkey = %s\n[gpg]\n\tformat = ssh\n[commit]\n\tgpgsign = true\n' \
    "$seed_key" > "$seed_host/gitconfig"
seed_old_global="${GIT_CONFIG_GLOBAL-}"
export GIT_CONFIG_GLOBAL="$seed_host/gitconfig"

out="$(cd "$ROOT" && CABOOSE_DATA_DIR="$seed_tmp1" "$CC" logs --tail 1 2>&1 >/dev/null)"
check 'a launch copies no identity or signing from the host' '' \
    "$(for k in user.name user.email user.signingkey commit.gpgsign; do git config --file "$seed_tmp1/$seed_cfg" --get "$k"; done 2>/dev/null)"
check 'and says, once, that the sandbox has none' 1 "$(printf '%s\n' "$out" | grep -c 'no git identity')"
check 'and creates no .gitconfig in the data dir' 1 "$(exists "$seed_tmp1/.gitconfig")"
rm -rf "$seed_tmp1" "$seed_host"
if [ -n "$seed_old_global" ]; then export GIT_CONFIG_GLOBAL="$seed_old_global"; else unset GIT_CONFIG_GLOBAL; fi

# Setup asks questions, so without a terminal it refuses before creating
# or writing anything -- here for an environment that does not exist,
# which it would otherwise offer to create.
setup_env=caboose-suite-nosuch
setup_home="${CABOOSE_HOME:-$HOME/.caboose}"
out="$(cd "$ROOT" && "$CC" -e "$setup_env" setup </dev/null 2>&1)"; rc=$?
check 'setup refuses without a terminal' 1 "$rc"
check 'and says why' 1 "$(printf '%s\n' "$out" | grep -c 'no terminal to ask them on')"
check 'and creates no environment' 1 "$(exists "$setup_home/envs/$setup_env")"

group 'ssh agent'
# The agent ssh on the host would use -- an IdentityAgent (1Password's
# documented setup) over $SSH_AUTH_SOCK -- has to be the one the sandbox
# sees: on a Mac that only works through the engine's forwarded socket.
host_agent="$(ssh -G github.com 2>/dev/null | sed -n 's/^identityagent //p' | tr -d '"')"
case "$host_agent" in
    '' | SSH_AUTH_SOCK | '$SSH_AUTH_SOCK') host_agent="${SSH_AUTH_SOCK:-}" ;;
    none) host_agent='' ;;
    '~/'*) host_agent="$HOME/${host_agent#\~/}" ;;
esac
host_keys="$(SSH_AUTH_SOCK="$host_agent" ssh-add -l 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ')"
if [ -n "$host_keys" ]; then
    check 'the sandbox sees the host agent'"'"'s keys' "$host_keys" \
        "$(docker exec "$CONTAINER" ssh-add -l 2>/dev/null | tr -d '\r' | awk '{print $2}' | sort | tr '\n' ' ')"
    check 'status says the agent is forwarded' forwarded \
        "$("$CC" status 2>/dev/null | sed -n 's/^ssh agent *: \([a-z]*\).*/\1/p')"
    if [ -n "$(docker exec "$CONTAINER" git config --global --get user.signingkey 2>/dev/null)" ]; then
        # A real signature through the forwarded agent. 1Password asks to
        # approve it on the Mac.
        printf '  \033[33mNOTE\033[0m signing a throwaway commit in the sandbox; approve it if your agent asks\n'
        check 'a commit made in the sandbox is signed' 1 \
            "$(docker exec "$CONTAINER" sh -c 'd=$(mktemp -d) && git -C "$d" init -q \
                && git -C "$d" commit -q -S --allow-empty -m caboose-probe >/dev/null 2>&1 \
                && git -C "$d" cat-file commit HEAD | grep -c "^gpgsig"; rm -rf "$d"' 2>/dev/null | tr -d '\r')"
    else
        printf '  \033[33mSKIP\033[0m the sandbox has no signing key to sign with\n'
    fi
else
    printf '  \033[33mSKIP\033[0m no agent with keys on the host\n'
fi

group 'timezone'
# Independent of how the launcher detects the zone: compare real clock offsets.
# A zone name the image lacks degrades silently to UTC, and this is what
# notices.
check 'container clock matches the host offset' "$(date +%z)" \
    "$(docker exec "$CONTAINER" date +%z 2>/dev/null | tr -d '\r')"
check 'container TZ is a zone the image actually has' 0 \
    "$(docker exec "$CONTAINER" sh -c 'test -z "$TZ" || test -f "/usr/share/zoneinfo/$TZ"' 2>/dev/null; echo $?)"
# A tmux session inherits the server's environment, not the attaching client's,
# so the launcher sets it per session with -e. Asia/Kolkata deliberately: it
# has had no DST since 1945, so +0530 cannot rot with the calendar.
docker exec "$CONTAINER" tmux kill-session -t tz-canary 2>/dev/null
docker exec "$CONTAINER" tmux new-session -d -e TZ=Asia/Kolkata -s tz-canary \
    'sh -c "date +%z > /tmp/tz-canary; sleep 30"' 2>/dev/null
sleep 1
check 'tmux new-session -e reaches the pane' "+0530" \
    "$(docker exec "$CONTAINER" cat /tmp/tz-canary 2>/dev/null | tr -d '\r')"
docker exec "$CONTAINER" tmux kill-session -t tz-canary 2>/dev/null
docker exec "$CONTAINER" rm -f /tmp/tz-canary 2>/dev/null

group 'docker client'
check 'docker CLI is present in the image' 0 \
    "$(docker exec "$CONTAINER" docker --version >/dev/null 2>&1; echo $?)"
# Security regression tests. If these fail, the sandbox boundary is gone, not
# merely weakened -- a reachable socket is root-equivalent access to the host.
if [ -z "${CABOOSE_DOCKER_SOCK:-}" ]; then
    check 'docker socket is NOT mounted by default' 1 \
        "$(docker exec "$CONTAINER" test -S /var/run/docker.sock 2>/dev/null; echo $?)"
    check 'and the CLI is therefore inert' 1 \
        "$(docker exec "$CONTAINER" docker ps >/dev/null 2>&1; echo $?)"
else
    printf '  \033[33mSKIP\033[0m CABOOSE_DOCKER_SOCK is set; the socket is expected here\n'
fi

group 'terminfo import'
# Ghostty/Kitty/WezTerm ship terminfo no distro packages, so the launcher
# compiles the host's description into the container rather than downgrading
# TERM forever. Renaming a universally-present entry makes this deterministic:
# it needs no particular terminal on the host, and the container genuinely
# lacks the name.
docker exec "$CONTAINER" infocmp caboose-probe >/dev/null 2>&1 \
    && bad 'probe entry should not exist yet' \
    || ok 'probe terminfo absent from the container to begin with'
{ infocmp -x xterm-256color 2>/dev/null || infocmp xterm-256color 2>/dev/null; } \
  | awk 'BEGIN{d=0} /^[^#]/ && !d { sub(/^[^|,]+/, "caboose-probe"); d=1 } {print}' \
  | docker exec -i "$CONTAINER" sh -c 'tic -x -o "$HOME/.terminfo" -' >/dev/null 2>&1
check 'host terminfo compiles into the container' 0 \
    "$(docker exec "$CONTAINER" infocmp caboose-probe >/dev/null 2>&1; echo $?)"
check 'and tmux will accept it as TERM' 0 \
    "$(docker exec "$CONTAINER" env TERM=caboose-probe tmux -L ti_probe -f /dev/null \
         new-session -d -s t 'sleep 5' >/dev/null 2>&1; echo $?)"
docker exec "$CONTAINER" tmux -L ti_probe kill-server 2>/dev/null
docker exec "$CONTAINER" sh -c 'rm -f "$HOME/.terminfo/c/caboose-probe"' 2>/dev/null

group 'launcher guards'
out="$(cd / && "$CC" claude --version </dev/null 2>&1)"
case "$out" in
    *"outside the mounted repo root"*) ok 'rejects a cwd outside the repo root' ;;
    *) bad "rejects a cwd outside the repo root (got: ${out:0:60})" ;;
esac
# Bind mounts are fixed at container creation, so a CABOOSE_REPO_ROOT that no
# longer matches the mount must be caught here -- otherwise it sails through
# and dies inside tmux on a container path the user never typed.
out="$(cd / && CABOOSE_REPO_ROOT=/ "$CC" claude --version </dev/null 2>&1)"
case "$out" in
    *"outside the root this container has mounted"*)
        ok 'detects CABOOSE_REPO_ROOT drifting from the real mount' ;;
    *) bad "detects CABOOSE_REPO_ROOT drifting from the real mount (got: ${out:0:60})" ;;
esac
# shell must not skip the guard and hand the user a raw "docker exec"
# error about a path that does not exist in the container.
out="$(cd / && "$CC" shell -c true </dev/null 2>&1)"
case "$out" in
    *"outside the"*) ok 'shell guards its cwd too' ;;
    *) bad "shell guards its cwd too (got: ${out:0:60})" ;;
esac
check 'non-tty invocation still reaches claude' 0 \
    "$(cd "$ROOT" && "$CC" claude --version </dev/null >/dev/null 2>&1; echo $?)"
# Only through 'caboose claude': anything else caboose does not know is
# refused, before docker or the configuration are asked anything.
check 'a bare claude flag is refused' 2 "$(cd "$ROOT" && "$CC" --resume </dev/null >/dev/null 2>&1; echo $?)"
check 'and so is an unknown command' 2 "$(cd "$ROOT" && "$CC" statusbar </dev/null >/dev/null 2>&1; echo $?)"
check 'caboose --help is caboose'"'"'s' 1 \
    "$(cd "$ROOT" && "$CC" --help </dev/null 2>/dev/null | grep -c '^Usage:$')"

group 'where the data dir is'
# A probe is status in a throwaway HOME, against a container name nothing
# uses: status only reads, so nothing is created, stopped or moved, and the
# real container and data dir are never in play. DOCKER_CONFIG keeps docker
# pointed at the same daemon despite the HOME.
probe="$(mktemp -d)"
(cd "$ROOT" && env -u CABOOSE_DATA_DIR -u CABOOSE_HOME -u CABOOSE_ENV HOME="$probe/home" \
    DOCKER_CONFIG="${DOCKER_CONFIG:-$HOME/.docker}" \
    CABOOSE_REPO_ROOT="$ROOT" CABOOSE_CONTAINER="caboose-test-absent-$$" \
    "$CC" status >"$probe/out" 2>/dev/null </dev/null)
check 'the default data dir is ~/.caboose/envs/default/data' "$probe/home/.caboose/envs/default/data" \
    "$(sed -n 's/^data dir *: //p' "$probe/out")"
check 'and a status call creates nothing' 1 "$(exists "$probe/home/.caboose/envs")"
rm -rf "$probe"

group 'independent sessions per terminal'
# detach names the session it resolved, so it doubles as a read-only probe
# for the naming rule without needing a tty. Two terminals in one project must
# land on the SAME name by default (that is detach/reattach) and on DIFFERENT
# ones once --session is given (that is the independent-session escape).
detach_name() { # detach_name [args...] -> the session name it resolved
    (cd "$ROOT" && "$CC" "$@" detach </dev/null 2>&1) \
        | sed -n "s/.*session '\([^']*\)'.*/\1/p" | head -1
}
plain="$(detach_name)"
named="$(detach_name --session two)"
check 'the project session name is stable' "$plain" "$(detach_name)"
check '--session gives a different session' 1 \
    "$([ -n "$named" ] && [ "$named" != "$plain" ] && echo 1 || echo 0)"
check '--session suffixes the project session' "$plain-two" "$named"
check '--session=NAME is the same' "$named" "$(detach_name --session=two)"
check 'CABOOSE_SESSION does it too' "$named" \
    "$(cd "$ROOT" && CABOOSE_SESSION=two "$CC" detach </dev/null 2>&1 \
        | sed -n "s/.*session '\([^']*\)'.*/\1/p" | head -1)"
out="$(cd "$ROOT" && "$CC" --session </dev/null 2>&1)"
case "$out" in
    *"needs a name"*) ok '--session with no name is rejected' ;;
    *) bad "--session with no name is rejected (got: ${out:0:60})" ;;
esac
# The flag must not be passed on to claude, and must not swallow what follows.
out="$(cd "$ROOT" && "$CC" --session probe claude --version </dev/null 2>&1)"
case "$out" in
    *"Claude Code"*) ok 'args after --session still reach claude' ;;
    *) bad "args after --session still reach claude (got: ${out:0:60})" ;;
esac

group 'a second terminal gets its own session'
# The name series itself -- busy names stepped over, orphaned ones reused --
# is only reachable on the tty attach path, so it is covered by
# TestFirstFreeSeries in internal/session instead.

# And the real half of it. tmux resolves a -t target exactly, then by prefix:
# without the '=' the idle base name would see its numbered sibling's client
# and every terminal would keep forking a new session forever.
probe='caboose-probe'
tmux_kill() { docker exec "$CONTAINER" tmux kill-session -t "=$1" 2>/dev/null; }
tmux_make() { docker exec "$CONTAINER" tmux new-session -d -s "$1" "${2:-sleep 120}" 2>/dev/null; }
clients_on_probe() {
    docker exec "$CONTAINER" tmux list-clients -t "=$1" 2>/dev/null | wc -l | tr -d ' '
}
tmux_make "$probe"
tmux_make "$probe-2"
# A pane running `tmux attach` is a real client. $TMUX has to go first or the
# inner tmux refuses to nest.
tmux_make "$probe-drv" "sh -c 'unset TMUX; exec tmux attach -t =$probe-2'"
sleep 1
check 'a session someone is attached to reads as busy' 1 "$(clients_on_probe "$probe-2")"
check 'its idle sibling reads as free'                 0 "$(clients_on_probe "$probe")"
tmux_kill "$probe-drv"; tmux_kill "$probe-2"; tmux_kill "$probe"

# detach has to cover the numbered siblings too, or it frees a session
# the asking terminal is not the one stuck on.
tmux_make "$plain"
tmux_make "$plain-2"
tmux_make "$plain-drv" "sh -c 'unset TMUX; exec tmux attach -t =$plain-2'"
sleep 1
out="$(cd "$ROOT" && "$CC" detach </dev/null 2>&1)"
check 'detach reaches a numbered sibling' 1 \
    "$(printf '%s' "$out" | grep -c "detached clients from '$plain-2'")"
check 'and leaves the sessions running' 1 \
    "$(docker exec "$CONTAINER" tmux has-session -t "=$plain-2" 2>/dev/null && echo 1 || echo 0)"
out="$(cd "$ROOT" && "$CC" --session two detach </dev/null 2>&1)"
check 'an explicit name detaches only that session' 0 \
    "$(printf '%s' "$out" | grep -c 'detached clients')"
tmux_kill "$plain-drv"; tmux_kill "$plain-2"; tmux_kill "$plain"

group 'destructive commands ask first'
docker exec "$CONTAINER" tmux new-session -d -s confirm-canary 'sleep 120' 2>/dev/null
id_before="$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER")"
# env -u FORCE: the suite itself may have been started with FORCE=1, which is
# exactly what must NOT leak into this assertion.
out="$(cd "$ROOT" && env -u FORCE "$CC" restart </dev/null 2>&1)"
case "$out" in
    *"refusing to restart"*) ok 'refuses to restart non-interactively with live sessions' ;;
    *) bad "refuses to restart non-interactively with live sessions (got: ${out:0:60})" ;;
esac
check 'the named session is listed before refusing' 1 \
    "$(printf '%s' "$out" | grep -c 'confirm-canary')"
check 'container was NOT recreated' "$id_before" \
    "$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER")"
check 'canary survived the refusal' 1 \
    "$(docker exec "$CONTAINER" tmux has-session -t confirm-canary 2>/dev/null && echo 1 || echo 0)"
FORCE=1 "$CC" restart >/dev/null 2>&1
check 'FORCE=1 overrides the prompt' 1 \
    "$([ "$id_before" != "$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER")" ] && echo 1 || echo 0)"

group 'doctor'
# What only the real engine can prove: the container's answers as doctor
# reads them (the NUL-separated inside script, claude --version, ssh-add),
# and that it changes nothing. --offline: the data dir may have a sync
# remote, and the suite does not reach the network for it.
id_before="$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER")"
doc_out="$(cd "$ROOT" && "$CC" doctor --offline 2>/dev/null)"; doc_rc=$?
# A row is "  MARK LABEL  TEXT"; doc_row gives a label's rows as their
# level's word, then the text: "note: ...", "problem: ...".
doc_row() {
    printf '%s\n' "$doc_out" | sed -n -e "s/^  ✓ $1  *//p" -e "s/^  – $1  *//p" -e "s/^  ! $1  */note: /p" -e "s/^  ✗ $1  */problem: /p"
}
check 'doctor exits 1 exactly when it lists a problem' \
    "$(printf '%s\n' "$doc_out" | grep -q '^  ✗ [a-z]' && echo 1 || echo 0)" "$doc_rc"
check 'doctor finds the engine' 'the engine answers' "$(doc_row docker | grep -v '^note:')"
check 'doctor finds the image current' 1 \
    "$(doc_row image | grep -c "^$IMAGE, matching this launcher")"
check 'doctor finds the container running' "$CONTAINER, running" "$(doc_row container)"
check 'doctor reads the data dir status does' "$DATA_DIR" "$(doc_row 'data dir' | head -1)"
check 'doctor reads Claude Code'"'"'s version' \
    "$(docker exec "$CONTAINER" claude --version 2>/dev/null | tr -d '\r' | head -1)" "$(doc_row claude)"
# The inside script: the restart above realigned TZ and keep-versions, so
# neither is a note, and the socket note matches the socket.
check 'doctor sees no timezone drift after a restart' '' "$(doc_row timezone)"
check 'doctor sees no keep-versions drift after a restart' '' "$(doc_row versions)"
check 'doctor notes the docker socket exactly when it is mounted' \
    "$(docker exec "$CONTAINER" test -S /var/run/docker.sock 2>/dev/null && echo 1 || echo 0)" \
    "$(doc_row docker | grep -c 'socket is mounted')"
if [ -n "$host_keys" ]; then
    check 'doctor finds the agent forwarded' forwarded "$(doc_row 'ssh agent' | cut -d, -f1)"
    if [ -n "$(docker exec "$CONTAINER" git config --global --get user.signingkey 2>/dev/null)" ]; then
        check 'doctor finds the signing key in the agent' 1 "$(doc_row signing | grep -c 'which the agent holds$')"
    fi
fi
# Where Claude Code keeps its login is its own choice: doctor (and setup)
# look for .claude/.credentials.json. The suite's sandbox is logged in, so
# the file has to be there, and doctor has to say so.
if [ -s "$HOME_DIR/.claude/.credentials.json" ]; then
    check 'doctor finds the login where Claude Code keeps it' "Claude, in $HOME_DIR/.claude/.credentials.json" "$(doc_row login)"
else
    printf '  \033[33mNOTE\033[0m no %s: log in to Claude in the sandbox, or Claude Code keeps its login elsewhere now\n' \
        "$HOME_DIR/.claude/.credentials.json"
    check 'doctor says there is no login' 1 "$(doc_row login | grep -c '^note: not logged in')"
fi
check 'doctor changed nothing' "$id_before" "$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER")"

group 'image drift is non-destructive'
before="$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER")"
docker exec "$CONTAINER" tmux new-session -d -s drift-canary 'sleep 120' 2>/dev/null
# warn_if_image_drifted only fires when the comparison image resolves in the
# local *image store*. Naming the base image here is fragile: BuildKit keeps
# base layers in its build cache, so `docker inspect --type=image ubuntu:26.04`
# can fail even right after a successful build, leaving the warning silent and
# this check green-by-accident in the other direction. Build a throwaway image
# instead -- guaranteed present, guaranteed a different ID to caboose, and no
# network needed.
printf 'FROM scratch\nLABEL caboose.fixture=drift\n' \
  | docker build -q -t caboose-drift-fixture - >/dev/null
drift="$(cd "$ROOT" && CABOOSE_IMAGE=caboose-drift-fixture "$CC" status 2>&1 | grep -c 'older image')"
check 'drift is reported' 1 "$drift"
check 'doctor reports the drift as a problem, with restart for its fix' 1 \
    "$(cd "$ROOT" && CABOOSE_IMAGE=caboose-drift-fixture "$CC" doctor --offline 2>/dev/null \
        | grep -c '^    container  *caboose restart')"
check 'container was NOT recreated' "$before" \
    "$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER")"
check 'canary session survived' 1 \
    "$(docker exec "$CONTAINER" tmux has-session -t drift-canary 2>/dev/null && echo 1 || echo 0)"
docker exec "$CONTAINER" tmux kill-session -t drift-canary 2>/dev/null
docker image rm -f caboose-drift-fixture >/dev/null 2>&1 || true
check 'no drift reported against the real image' 0 \
    "$(cd "$ROOT" && "$CC" status 2>&1 | grep -c 'older image')"

group 'isolation = gvisor'
# Only where docker has runsc registered: registering it is not the
# launcher's. The user is the probe's to choose -- the agent where it can
# write its mounts under gVisor (a Linux host), root where it cannot
# (OrbStack) -- so what is checked is that the container runs as the label
# says, and that whoever that is can write the login's kind of file.
if docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q '"runsc"'; then
    CABOOSE_ISOLATION=gvisor FORCE=1 "$CC" restart >/dev/null 2>&1
    check 'the container runs under runsc' runsc \
        "$(docker inspect --type=container -f '{{.HostConfig.Runtime}}' "$CONTAINER")"
    check 'it is labelled gvisor' gvisor \
        "$(docker inspect --type=container -f '{{index .Config.Labels "io.github.bfreis.caboose.isolation"}}' "$CONTAINER")"
    user_label="$(docker inspect --type=container -f '{{index .Config.Labels "io.github.bfreis.caboose.user"}}' "$CONTAINER")"
    want_uid="$(label io.github.bfreis.caboose.uid)"
    [ "$user_label" = 0:0 ] && want_uid=0
    check 'it runs as the user its label says' "$want_uid" "$(cexec id -u)"
    check 'that user writes a 600 file in ~/.claude' ok \
        "$(cexec sh -c 'f=~/.claude/caboose-test-600; umask 077; echo x > "$f" && [ "$(cat "$f")" = x ] && rm -f "$f" && echo ok')"
    check 'doctor names the isolation' 'gvisor (runsc)' \
        "$(cd "$ROOT" && CABOOSE_ISOLATION=gvisor "$CC" doctor --offline 2>/dev/null | sed -n 's/^  ✓ isolation  *//p')"
    # Unless the environment's own config.toml says gvisor too.
    if [ "$(cd "$ROOT" && "$CC" status 2>/dev/null | sed -n 's/^isolation : //p')" = docker ]; then
        check 'a launch without it says the container is gvisor' 1 \
            "$(cd "$ROOT" && "$CC" status 2>&1 | grep -c 'created with isolation gvisor')"
    fi
    FORCE=1 "$CC" restart >/dev/null 2>&1
else
    printf '  \033[33mSKIP\033[0m docker has no runsc runtime\n'
fi

vm_group

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
