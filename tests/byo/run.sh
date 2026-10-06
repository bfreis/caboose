#!/usr/bin/env bash
# Bring-your-own-image integration tests: an [image] base on a glibc and
# a musl base, one after the other against the SAME data dir, and stock
# images the image check has to refuse.
#
#   tests/byo/run.sh
#   CABOOSE_BIN=path/to/caboose tests/byo/run.sh
#                         test another launcher build (relative to the repo)
#   BYO_DEBIAN=debian:13.7-slim BYO_ALPINE=alpine:3.24 tests/byo/run.sh
#                         other stock tags (the test bases are built on them)
#   BYO_READY_TIMEOUT=1200 tests/byo/run.sh
#                         seconds a launch waits for Claude Code's first install
#
# Run it through `make test-byo`, which rebuilds ./caboose first. It needs a
# real docker on the host, so like tests/run.sh it cannot run in the sandbox.
#
# Unlike tests/run.sh it never touches the real setup: every launcher call
# gets a throwaway CABOOSE_HOME, CABOOSE_DATA_DIR and root (one mktemp -d),
# and runs in an environment of its own, byo-test (and byo-test-refused), so
# the container and images are caboose-byo-test, caboose:byo-test and so on.
# Each call writes that environment's config.toml first (cc), which is where
# the base, the root and the timeout are set. A trap removes them, the test bases, every layer image it built and any
# stock image it had to pull. So it is safe beside live sessions: no CABOOSE_FORCE.
#
# The launcher is driven exactly as a user would, with no tty: `caboose
# claude --version` in a project under the root builds the layer when
# there is none, creates the container, waits for the entrypoint to install
# Claude Code and passes --version to claude. Everything is read back through
# status, version, check-image, docker inspect/exec/logs and
# the data dir itself.
# Check descriptions name container paths as ~/..., text that is printed and
# never meant to expand.
# shellcheck disable=SC2088
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
HERE="$ROOT/tests/byo"
CC="${CABOOSE_BIN:-$ROOT/caboose}"
case "$CC" in /*) ;; *) CC="$ROOT/$CC" ;; esac

die() { printf 'tests/byo: %s\n' "$*" >&2; exit 1; }
[ -x "$CC" ] || die "no launcher at $CC (run this through 'make test-byo')"
command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
docker version >/dev/null 2>&1 || die "the docker engine does not answer"

# Stock images: checked as they are (they must be refused), and the test
# bases' FROM. Pinned so that what "stock" lacks cannot drift under the
# assertions below; BYO_* overrides them.
DEBIAN="${BYO_DEBIAN:-debian:13.7-slim}"
ALPINE="${BYO_ALPINE:-alpine:3.24}"

# Everything this suite creates is named from here, so a leftover from an
# aborted run is recognisably ours -- and is removed below, before starting.
PREFIX=caboose-byo-test
TEST_ENV=byo-test                  # the environment the suite runs in
REFUSED_ENV=byo-test-refused       # the one a refused build must not create anything for
IMAGE="caboose:$TEST_ENV"          # the layer the launcher builds
CONTAINER="caboose-$TEST_ENV"
DEB_BASE="$PREFIX-debian-base"     # tests/byo/debian.Dockerfile
ALP_BASE="$PREFIX-alpine-base"     # tests/byo/alpine.Dockerfile
REFUSED_IMAGE="caboose:$REFUSED_ENV"
REFUSED_CONTAINER="caboose-$REFUSED_ENV"
DIR_BASE="caboose-base:$TEST_ENV"  # the base built from an environment's image/ dir

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n'   "$1"; fail=$((fail + 1)); }
check() { # check <description> <expected> <actual>
    if [ "$2" = "$3" ]; then ok "$1"; else bad "$1"; printf '        expected: %s\n        actual:   %s\n' "$2" "$3"; fi
}
group() { printf '\n\033[1m%s\033[0m\n' "$1"; }
note()  { printf '  \033[2m%s\033[0m\n' "$1"; }
exists() { if [ -e "$1" ]; then echo 0; else echo 1; fi; }

# --- the throwaway setup --------------------------------------------------

# Physical paths (pwd -P): the launcher resolves the root with
# EvalSymlinks before mounting it, and the cwd guard compares
# physical paths too -- on macOS $TMPDIR is under /var, a symlink
# to /private/var. The data dir is mounted as given, so it is made physical
# as well, for the paths status prints to compare equal to it.
WORK="$(mktemp -d "${TMPDIR:-/tmp}/caboose-byo.XXXXXX")" || die "mktemp failed"
WORK="$(cd "$WORK" && pwd -P)"
DATA="$WORK/data"
REPO_ROOT="$WORK/root"
PROJ="$REPO_ROOT/proj"     # /work/root/proj in the container
OUT="$WORK/out"; ERR="$WORK/err"
mkdir -p "$DATA" "$PROJ"

# Only these settings, and nothing from the caller's own caboose: no
# CABOOSE_* of theirs, and a CABOOSE_HOME of the suite's own, so no
# config.toml of theirs is read either.
for v in $(compgen -e); do
    case "$v" in CABOOSE_*) unset "$v" ;; esac
done
export CABOOSE_HOME="$WORK/caboose-home"
export CABOOSE_DATA_DIR="$DATA" CABOOSE_ENV="$TEST_ENV"
# The first launch on each platform waits for a ~224MB download; the
# default of 600s is tight on a slow line.
READY_TIMEOUT="${BYO_READY_TIMEOUT:-1200}"

# Stock images are removed at the end only if this run pulled them.
pulled=()
for img in "$DEBIAN" "$ALPINE"; do
    docker image inspect "$img" >/dev/null 2>&1 || pulled+=("$img")
done
# Every layer image ID the launcher built: rebuilding under the same tag
# leaves the previous one untagged, still used by the container until the
# restart, so removing the tag alone would leave it behind.
built_ids=()
record_image() {
    local id; id="$(docker image inspect -f '{{.Id}}' "$1" 2>/dev/null)"
    [ -n "$id" ] && built_ids+=("$id")
    return 0
}

remove_ours() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1
    docker rm -f "$REFUSED_CONTAINER" >/dev/null 2>&1
    docker image rm -f "$IMAGE" >/dev/null 2>&1
    docker image rm -f "$REFUSED_IMAGE" >/dev/null 2>&1
    return 0
}
cleanup() {
    remove_ours
    local id
    for id in ${built_ids[@]+"${built_ids[@]}"}; do docker image rm -f "$id" >/dev/null 2>&1; done
    docker image rm -f "$DEB_BASE" "$ALP_BASE" "$DIR_BASE" >/dev/null 2>&1
    # Everything in the data dir was written by the host user or by the
    # container's agent user, which the layer gives the host UID, so a plain
    # rm works. Where docker maps UIDs (rootless docker, podman) it may not:
    # then root in a container -- the host user, under that mapping -- does it.
    if [ -n "${WORK:-}" ] && [ -d "$WORK" ] && ! rm -rf "$WORK" 2>/dev/null; then
        docker run --rm --user 0:0 -v "$WORK:/w" --entrypoint /bin/sh "$ALPINE" \
            -c 'rm -rf /w/data /w/root' >/dev/null 2>&1
        rm -rf "$WORK" 2>/dev/null \
            || printf '\n\033[31mWARNING\033[0m could not remove %s; delete it by hand.\n' "$WORK" >&2
    fi
    local img
    for img in ${pulled[@]+"${pulled[@]}"}; do docker image rm "$img" >/dev/null 2>&1; done
    return 0
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# A leftover from an aborted run would make "the launch builds the layer"
# vacuous (the image would already be there), so it goes first.
remove_ours
docker image rm -f "$DEB_BASE" "$ALP_BASE" "$DIR_BASE" >/dev/null 2>&1

# --- driving the launcher -------------------------------------------------

# cc [NAME=value...] [launcher args...]: run the launcher the way a script
# would, from a project under the root with no tty -- stdin from
# /dev/null, stdout and stderr to $OUT and $ERR -- and leave its exit status
# in $rc. The NAME=value words are for this call only: BYO_BASE is the
# environment's [image] base (none when absent), BYO_ENV the environment
# (byo-test), which cc writes a config.toml for first -- the root, the boot
# timeout and the base -- and CABOOSE_FORCE and the like are variables of the
# launcher's. Launcher args never look like NAME=value, so they cannot be
# mistaken for one.
#
# With no tty on stdin or stdout, `caboose claude --version` goes through
# launcher.Attach: enterProjectDir,
# the root guard, ensureRunning(true) -- which on an absent container
# runs createContainer, whose ensureImage builds a missing image -- or
# rebuilds one built on another base than [image] base names -- with
# the same build as build ("TTY or not", its log on stderr), then
# waitUntilReady, polling `docker exec CONTAINER test -f /tmp/.caboose-ready`
# for up to ready_timeout seconds while the entrypoint installs
# Claude Code -- and finally, because !tty.IsTerminal(stdin/stdout),
# syscall.Exec of `docker exec -i ... CONTAINER caboose-entrypoint --version`,
# which execs ~/.local/bin/claude --version. Its exit status is claude's.
rc=0
cc() {
    local envs=() base="" benv="$TEST_ENV"
    while [ $# -gt 0 ]; do
        case "$1" in
            BYO_BASE=*) base="${1#*=}"; shift ;;
            BYO_ENV=*) benv="${1#*=}"; shift ;;
            [A-Z]*=*) envs+=("$1"); shift ;;
            *) break ;;
        esac
    done
    mkdir -p "$CABOOSE_HOME/envs/$benv"
    {
        printf 'format = 1\n\n[roots]\nroot = "%s"\n\n[session]\nready_timeout = %s\n' "$REPO_ROOT" "$READY_TIMEOUT"
        [ -z "$base" ] || printf '\n[image]\nbase = "%s"\n' "$base"
    } > "$CABOOSE_HOME/envs/$benv/config.toml"
    (cd "$PROJ" && env CABOOSE_ENV="$benv" ${envs[@]+"${envs[@]}"} "$CC" "$@") </dev/null >"$OUT" 2>"$ERR"
    rc=$?
}
DEB="BYO_BASE=$DEB_BASE"
ALP="BYO_BASE=$ALP_BASE"

# check_rc <description> <expected>: check $rc, and on a mismatch show the
# end of the launcher's stderr, which is where a build or boot says why.
check_rc() {
    check "$1" "$2" "$rc"
    if [ "$2" != "$rc" ]; then
        printf '        stderr (last lines):\n'
        tail -n 25 "$ERR" | sed 's/^/          /'
    fi
}
# field <label>: the value of a "label : value" line in $OUT, the format of
# status and version (both "%-10s: %s").
field() { sed -n "s/^$1 *: //p" "$OUT" | head -1; }
# irow <label>: the value of check-image's row for label ("  MARK LABEL
# VALUE"), whatever its mark; one sed expression a mark, as a bracket of
# multibyte marks would need a UTF-8 locale.
irow() {
    sed -n -e "s/^  ✓ $1  *//p" -e "s/^  ! $1  *//p" -e "s/^  ✗ $1  *//p" "$OUT" | head -1
}
# verdict: check-image's verdict, "Usable" or "Not usable".
verdict() { sed -n -e 's/^  ✓ \(Usable\):.*/\1/p' -e 's/^  ✗ \(Not usable\):.*/\1/p' "$OUT"; }
first() { cut -d' ' -f1; }
# has <file> <text>: 1 when the file contains the text (fixed string), else 0.
has() { if grep -qF -- "$2" "$1" 2>/dev/null; then echo 1; else echo 0; fi; }

label() { docker image inspect --format "{{index .Config.Labels \"$2\"}}" "$1" 2>/dev/null; }
cexec() { docker exec "$CONTAINER" "$@" 2>/dev/null | tr -d '\r'; }
# The entrypoint's first-install line, from ensure_claude_installed in
# entrypoint.sh; its log goes to the container's stderr, so docker logs.
INSTALL_LINE='installing the native build'
installs_logged() { docker logs "$CONTAINER" 2>&1 | grep -cF "$INSTALL_LINE"; }
# check_installs <description> <expected>: check installs_logged, and on a
# mismatch show why. More installs than expected means a boot died and
# --restart unless-stopped brought the container back (the entrypoint runs
# under set -e, so a failed install exits PID 1); the log says what failed,
# and the container is gone by the time anyone could ask it.
check_installs() {
    local n
    n="$(installs_logged)"
    check "$1" "$2" "$n"
    if [ "$2" != "$n" ]; then
        printf '        restarts: %s\n        container log (last lines):\n' \
            "$(docker inspect --type=container -f '{{.RestartCount}}' "$CONTAINER" 2>/dev/null)"
        docker logs "$CONTAINER" 2>&1 | tr -d '\r' | tail -n 40 | sed 's/^/          /'
    fi
}

# The libc a Claude Code binary is built for, from its ELF program
# interpreter (/lib/ld-musl-<arch>.so.1 or ld-linux-*), which sits in the
# first few KB -- the same thing datadir.ELFPlatform reads. grep -c rather
# than -q: -q would stop reading early and SIGPIPE head, which pipefail
# turns into a failure.
binary_libc() {
    if [ "$(head -c 8192 "$1" 2>/dev/null | LC_ALL=C grep -ac 'ld-musl')" -gt 0 ]; then
        echo musl
    elif [ "$(head -c 8192 "$1" 2>/dev/null | LC_ALL=C grep -ac 'ld-linux')" -gt 0 ]; then
        echo glibc
    else
        echo unknown
    fi
}
# installed <platform>: the host path of the version ~/.local/bin/claude
# points at in that platform dir. The installer makes it a symlink to the
# CONTAINER path /home/agent/.local/share/claude/versions/<v> (see
# datadir.HasInstall), so only its last component is used.
installed() {
    local link; link="$(readlink "$DATA/local/$1/bin/claude" 2>/dev/null)" || return 0
    [ -n "$link" ] && printf '%s\n' "$DATA/local/$1/share/claude/versions/${link##*/}"
}
# snapshot <platform>: what a reinstall or an update would change -- the
# versions (names, sizes, mtimes) and where the launcher symlink points.
snapshot() {
    LC_ALL=C ls -l "$DATA/local/$1/share/claude/versions" 2>&1
    readlink "$DATA/local/$1/bin/claude" 2>&1
}

printf 'caboose BYO image suite: %s\n' "$CC"
printf 'This takes several minutes: it builds two test bases and installs Claude Code\n'
printf 'twice (glibc and musl, ~224MB each) into a throwaway data dir, %s.\n' "$DATA"

# --- 1. stock images are refused --------------------------------------------

group "stock images are refused, naming what is missing"
# check-image IMAGE: launcher.CheckImage pulls a named image that is not
# local (progress on stderr), runs the probe, prints imagecheck.Checklist as
# "  MARK LABEL  VALUE" rows plus a verdict on stdout, the Problems as
# "caboose: LABEL: missing ... -- why" on stderr (stderr is no terminal here),
# and exits 1 (checkUnmet) when any requirement is unmet, 2 when it could
# not check.
cc check-image "$DEBIAN"
check_rc "check-image $DEBIAN exits 1" 1
check 'it says not usable' 'Not usable' "$(verdict)"
check 'curl is missing'            missing "$(irow curl | first)"
check 'tmux is missing'            missing "$(irow tmux | first)"
check 'git is missing'             missing "$(irow git | first)"
check 'the CA certificates are missing' missing "$(irow ca-certs | first)"
# bash is an Essential package in Debian, so the probe must not invent a gap.
check 'bash is there (Essential in Debian)' ok "$(irow bash | first)"
check 'the libc is glibc'          glibc "$(irow libc)"
check 'and each is named, with why, on stderr' '1 1 1 1' \
    "$(has "$ERR" 'caboose: curl: missing') $(has "$ERR" 'caboose: tmux: missing') $(has "$ERR" 'caboose: git: missing') $(has "$ERR" 'caboose: CA certificates: missing')"

cc check-image "$ALPINE"
check_rc "check-image $ALPINE exits 1" 1
check 'it says not usable' 'Not usable' "$(verdict)"
check 'bash is missing'            missing "$(irow bash | first)"
check 'curl is missing'            missing "$(irow curl | first)"
check 'tmux is missing'            missing "$(irow tmux | first)"
check 'git is missing'             missing "$(irow git | first)"
check 'the libc is musl'           musl "$(irow libc)"
check 'libgcc is missing'          missing "$(irow libgcc | first)"
check 'libstdc++ is missing'       missing "$(irow 'libstdc++' | first)"
check 'ripgrep is missing'         missing "$(irow ripgrep | first)"
# BusyBox covers every small tool the probe checks: no coreutils needed.
check 'BusyBox covers the tools'   ok "$(irow tools)"
check 'and each is named on stderr' '1 1 1 1 1 1 1' \
    "$(for l in bash curl tmux git libgcc 'libstdc++' ripgrep; do printf '%s ' "$(has "$ERR" "caboose: $l: missing")"; done | sed 's/ $//')"

# --- 2. the test bases pass -------------------------------------------------

group "the test bases pass the check"
build_base() { # build_base TAG DOCKERFILE FROM: no context, nothing is COPYed
    if docker build -q -t "$1" --build-arg FROM_IMAGE="$3" - <"$2" >"$WORK/build.log" 2>&1; then
        ok "docker build $1 (FROM $3)"
    else
        bad "docker build $1 (FROM $3)"
        tail -n 25 "$WORK/build.log" | sed 's/^/          /'
        die "cannot go on without the test bases"
    fi
}
build_base "$DEB_BASE" "$HERE/debian.Dockerfile" "$DEBIAN"
build_base "$ALP_BASE" "$HERE/alpine.Dockerfile" "$ALPINE"

# The platform the bases should get, from docker rather than from the
# launcher: the installer's names are linux-x64 and linux-arm64 (-musl).
case "$(docker image inspect -f '{{.Architecture}}' "$DEB_BASE" 2>/dev/null)" in
    amd64|x86_64)  ARCH=x64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) die "the test base is for an architecture Claude Code has no build for" ;;
esac
GLIBC="linux-$ARCH"
MUSL="linux-$ARCH-musl"

cc check-image "$DEB_BASE"
check_rc "check-image passes the Debian test base" 0
check 'verdict ok'                 'Usable' "$(verdict)"
check 'libc glibc'                 glibc "$(irow libc)"
check "platform $GLIBC"            "$GLIBC" "$(irow platform)"

cc check-image "$ALP_BASE"
check_rc "check-image passes the Alpine test base" 0
check 'verdict ok'                 'Usable' "$(verdict)"
check 'libc musl'                  musl "$(irow libc)"
check "platform $MUSL"             "$MUSL" "$(irow platform)"
check 'the musl libraries are found' 'ok ok ok' \
    "$(irow libgcc | first) $(irow 'libstdc++' | first) $(irow ripgrep | first)"

# --- 3. a session on the Debian base ----------------------------------------

group "a session on the Debian base"
note "first launch: builds the layer, creates the container, installs Claude Code ($GLIBC)"
cc "$DEB" claude --version
check_rc 'a no-tty launch builds, starts and reaches claude' 0
check 'claude --version answers through the pass-through' 1 "$(has "$OUT" '(Claude Code)')"
DEB_VERSION="$(tr -d '\r' <"$OUT")"
# ensureImage: "no image '%s' yet — %s", buildNote naming base in [image];
# build: "checking base image '%s' (%s) ..." before buildLayer.
check 'the launch built the missing image itself' 1 "$(has "$ERR" "no image '$IMAGE' yet")"
check 'on the [image] base' 1 "$(has "$ERR" "building the caboose layer on '$DEB_BASE'")"
check 'after checking that base' 1 "$(has "$ERR" "checking base image '$DEB_BASE'")"
# noteInstall found no bin/claude in the platform dir before the container
# started, so waitUntilReady says, on its first poll, that it installs.
check "it said it installs into local/$GLIBC" 1 \
    "$(has "$ERR" "first run on $GLIBC — installing Claude Code into $DATA/local/$GLIBC")"
record_image "$IMAGE"
check 'the container is running' running \
    "$(docker inspect --type=container -f '{{.State.Status}}' "$CONTAINER" 2>/dev/null)"
check "the image's platform label is $GLIBC" "$GLIBC" "$(label "$IMAGE" dev.bfreis.caboose.platform)"
check "its base-name label is the Debian base" "$DEB_BASE" "$(label "$IMAGE" dev.bfreis.caboose.base-name)"
check "its base-id label is that base's ID" \
    "$(docker image inspect -f '{{.Id}}' "$DEB_BASE" 2>/dev/null)" "$(label "$IMAGE" dev.bfreis.caboose.base-id)"
check 'no base-hash label: the embedded Dockerfile played no part' '' \
    "$(label "$IMAGE" dev.bfreis.caboose.base-hash)"
check 'claude --version works by docker exec too' "$DEB_VERSION" "$(cexec claude --version)"
check "local/$GLIBC/bin/claude is the installer's symlink" 0 \
    "$(if [ -L "$DATA/local/$GLIBC/bin/claude" ]; then echo 0; else echo 1; fi)"
glibc_bin="$(installed "$GLIBC")"
check "and points at a version in local/$GLIBC" 0 "$(exists "${glibc_bin:-/nonexistent}")"
check 'which is a glibc build' glibc "$(binary_libc "${glibc_bin:-/nonexistent}")"
check 'no musl dir yet' 1 "$(exists "$DATA/local/$MUSL")"
check_installs 'the entrypoint installed it' 1
cc "$DEB" status
check "status: platform $GLIBC" "$GLIBC" "$(field platform)"
check "status: local dir is local/$GLIBC" "$DATA/local/$GLIBC" "$(field 'local dir')"
cc "$DEB" version
check 'version: the image matches' matches "$(field local | first)"
check 'version: the container is on it' "$CONTAINER (running, on the local image)" "$(field container)"
check 'version: the base is the [image] base' "$DEB_BASE (base in [image]," "$(field base | cut -d' ' -f1-4)"
check 'the container runs as the host UID' "$(id -u)" "$(cexec id -u)"
check 'with the host GID' "$(id -g)" "$(cexec id -g)"
check 'as agent' agent "$(cexec id -un)"
check 'USE_BUILTIN_RIPGREP is not set on glibc' '' "$(cexec printenv USE_BUILTIN_RIPGREP)"
glibc_before="$(snapshot "$GLIBC")"

# --- 4. the same data dir on the Alpine base --------------------------------

group "the same data dir on the Alpine base"
# The explicit way to switch: build first, while the container runs on,
# then restart, which finds the image current and builds nothing. (Step
# 5 switches back with restart alone.) restart needs CABOOSE_FORCE=1 only
# when tmux sessions are live (confirmSessionLoss), and there are none; it
# is passed anyway, as the suite must never stop at a prompt.
cc "$ALP" build
check_rc 'build builds the layer on the Alpine base' 0
record_image "$IMAGE"
check "the image's platform label is $MUSL" "$MUSL" "$(label "$IMAGE" dev.bfreis.caboose.platform)"
cc "$ALP" version
check 'version: the image matches the new base' matches "$(field local | first)"
check 'version: the container is still on the old image' \
    "$CONTAINER (running, on an older image than the local one)" "$(field container)"
note "restart: recreates the container, installs Claude Code ($MUSL)"
cc "$ALP" CABOOSE_FORCE=1 restart
check_rc 'restart moves the container onto it' 0
check 'without building again' 0 "$(has "$ERR" "building the caboose layer")"
check "saying it installs into local/$MUSL" 1 \
    "$(has "$ERR" "first run on $MUSL — installing Claude Code into $DATA/local/$MUSL")"
cc "$ALP" claude --version
check_rc 'claude --version runs on musl (the proof)' 0
check 'and answers as Claude Code' 1 "$(has "$OUT" '(Claude Code)')"
check 'the container really is Alpine' 0 \
    "$(docker exec "$CONTAINER" test -f /etc/alpine-release >/dev/null 2>&1; echo $?)"
check_installs 'the entrypoint installed a build for it' 1
musl_bin="$(installed "$MUSL")"
check "local/$MUSL holds the install" 0 "$(exists "${musl_bin:-/nonexistent}")"
check 'which is a musl build' musl "$(binary_libc "${musl_bin:-/nonexistent}")"
check "local/$GLIBC is untouched" "$glibc_before" "$(snapshot "$GLIBC")"
check 'USE_BUILTIN_RIPGREP=0 on musl' 0 "$(cexec printenv USE_BUILTIN_RIPGREP)"
check 'the image has the rg it points claude at' 0 \
    "$(docker exec "$CONTAINER" sh -c 'command -v rg' >/dev/null 2>&1; echo $?)"
cc "$ALP" status
check "status: platform $MUSL" "$MUSL" "$(field platform)"
check "status: local dir is local/$MUSL" "$DATA/local/$MUSL" "$(field 'local dir')"
cc "$ALP" version
check 'version: the container is on the local image' \
    "$CONTAINER (running, on the local image)" "$(field container)"
check 'the container runs as the host UID' "$(id -u)" "$(cexec id -u)"
check 'with the host GID' "$(id -g)" "$(cexec id -g)"
musl_before="$(snapshot "$MUSL")"

# --- 5. back to Debian: nothing is reinstalled ------------------------------

group "back on the Debian base, the glibc install is reused"
cc "$DEB" version
# classifyImage: a BYO image built on another base name is stale, for a
# changed base (builtOn), which the next container creation rebuilds.
check 'version: the image is out of date (built on Alpine)' 'out of date' "$(field local | cut -c1-11)"
check 'and says which base it was built on' 1 "$(has "$OUT" "on '$ALP_BASE', not on the [image] base '$DEB_BASE'")"
check 'and that a restart rebuilds it' 1 \
    "$(has "$ERR" "the next launch that creates the container rebuilds it on the configured base")"
# No build this time: Restart's ensureImage sees an image built on
# another base than [image] base names, says so, and runs the build
# (check, then buildLayer, log on stderr) before it removes the container.
note "restart alone: rebuilds the layer on Debian, then recreates the container"
cc "$DEB" CABOOSE_FORCE=1 restart
check_rc 'restart rebuilds and moves the container back' 0
record_image "$IMAGE"
check 'saying why it rebuilds' 1 \
    "$(has "$ERR" "image '$IMAGE' was built on '$ALP_BASE'; base in [image] now names '$DEB_BASE' — rebuilding")"
check 'on the Debian base' 1 "$(has "$ERR" "building the caboose layer on '$DEB_BASE' as '$IMAGE'")"
check 'and that it built it' 1 "$(has "$ERR" "built image '$IMAGE'")"
check 'with no stale-image warning' 0 "$(has "$ERR" "is out of date")"
check "the image's base-name label is the Debian base again" "$DEB_BASE" \
    "$(label "$IMAGE" dev.bfreis.caboose.base-name)"
check "and its platform label $GLIBC" "$GLIBC" "$(label "$IMAGE" dev.bfreis.caboose.platform)"
check 'the new container runs that image' "$(docker image inspect -f '{{.Id}}' "$IMAGE" 2>/dev/null)" \
    "$(docker inspect --type=container -f '{{.Image}}' "$CONTAINER" 2>/dev/null)"
check 'the container really is Debian' 0 \
    "$(docker exec "$CONTAINER" test -f /etc/debian_version >/dev/null 2>&1; echo $?)"
check 'and not Alpine' 1 \
    "$(docker exec "$CONTAINER" test -f /etc/alpine-release >/dev/null 2>&1; echo $?)"
check 'the launcher announced no install' 0 "$(has "$ERR" "installing Claude Code")"
# A fresh container, so its log is this boot's only.
check_installs 'the entrypoint installed nothing' 0
check 'it came up ready' 1 "$(docker logs "$CONTAINER" 2>&1 | grep -cF 'caboose: ready')"
check "local/$GLIBC is as it was" "$glibc_before" "$(snapshot "$GLIBC")"
check "local/$MUSL is left as it was" "$musl_before" "$(snapshot "$MUSL")"
cc "$DEB" claude --version
check_rc 'claude --version works again' 0
check 'the same version as before' "$DEB_VERSION" "$(tr -d '\r' <"$OUT")"
cc "$DEB" status
check "status: platform $GLIBC" "$GLIBC" "$(field platform)"
check 'USE_BUILTIN_RIPGREP is gone again' '' "$(cexec printenv USE_BUILTIN_RIPGREP)"

# --- 6. a base that fails the check -----------------------------------------

group "a base that fails the check is refused before the layer"
# An environment of its own: the image above exists, and a launch on an
# existing image builds nothing.
container_id="$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"
cc BYO_ENV="$REFUSED_ENV" BYO_BASE="$ALPINE" build
# build: the check's Problems as notes, then Die (exit 1) before buildLayer.
check_rc "build on stock $ALPINE fails" 1
check 'saying the base does not meet the requirements' 1 \
    "$(has "$ERR" "base image '$ALPINE' does not meet caboose's requirements")"
check 'listing what is missing' '1 1 1' \
    "$(has "$ERR" 'bash: missing') $(has "$ERR" 'libgcc: missing') $(has "$ERR" 'ripgrep: missing')"
check 'and that the layer was not built' 1 "$(has "$ERR" "not building the caboose layer on '$ALPINE'")"
# buildLayer's own note, "building the caboose layer on '%s' as '%s'" (the
# refusal's "not building the caboose layer on" contains the first half).
check 'no layer build ran' 0 "$(has "$ERR" "building the caboose layer on '$ALPINE' as '$REFUSED_IMAGE'")"
check "no image $REFUSED_IMAGE was created" 1 \
    "$(docker image inspect "$REFUSED_IMAGE" >/dev/null 2>&1; echo $?)"

# The first-run path: the same build, from ensureImage, whose error ends the
# launch before any container exists.
cc BYO_ENV="$REFUSED_ENV" BYO_BASE="$ALPINE" claude --version
check_rc "a launch on stock $ALPINE fails" 1
check 'with the same refusal' 1 "$(has "$ERR" "not building the caboose layer on '$ALPINE'")"
check 'and claude never ran' '' "$(cat "$OUT")"
check "no image $REFUSED_IMAGE" 1 "$(docker image inspect "$REFUSED_IMAGE" >/dev/null 2>&1; echo $?)"
check "no container $REFUSED_CONTAINER" 1 "$(docker inspect --type=container "$REFUSED_CONTAINER" >/dev/null 2>&1; echo $?)"
check 'the other container was left alone' "$container_id" \
    "$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"

group "an environment's own image dir"
# What caboose setup writes, built for real: the minimal core -- which no
# answer can leave out, and which must pass the image check -- with the
# opt-in Rust section, commented out in the embedded Dockerfile and
# uncommented here. The Dockerfile comes from the same code setup uses
# (tests/byo/preset), since there is no terminal here to answer setup.
IMG_DIR="$CABOOSE_HOME/envs/$TEST_ENV/image"
mkdir -p "$IMG_DIR"
if (cd "$ROOT" && go run ./tests/byo/preset rust) > "$IMG_DIR/Dockerfile"; then
    ok 'the preset for core + rust is written'
else
    bad 'the preset for core + rust is written'
fi
container_id="$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"
record_image "$IMAGE"
# Both a dir and an [image] base is a guess about what to build on: refused.
cc "$DEB" build
check_rc 'an image dir and an [image] base together are refused' 1
check 'saying to keep one' 1 "$(has "$ERR" 'Keep one')"
cc build
check_rc 'build from the image dir' 0
record_image "$IMAGE"
check 'the base is built from the dir' 1 "$(has "$ERR" "building the base image '$DIR_BASE' from $IMG_DIR/Dockerfile")"
check 'the layer says so' 'env' \
    "$(docker image inspect -f '{{index .Config.Labels "dev.bfreis.caboose.base-kind"}}' "$IMAGE" 2>/dev/null)"
check 'rust runs in it' 1 \
    "$(docker run --rm --entrypoint sh "$IMAGE" -c 'rustc --version' 2>/dev/null | grep -c '^rustc ')"
check 'and the agent user can write CARGO_HOME' 0 \
    "$(docker run --rm --entrypoint sh "$IMAGE" -c 'test -w "$CARGO_HOME"'; echo $?)"
# Found or not, not a status: dash's command -v exits 127 for a missing
# command, bash's 1.
check 'node, left out, is not in it' absent \
    "$(docker run --rm --entrypoint sh "$IMAGE" -c 'if command -v node >/dev/null; then echo found; else echo absent; fi' 2>/dev/null)"
cc version
check 'version finds it current' 1 "$(grep -c '^local     : matches' "$OUT")"

# An edit is a change the user made: stale, and rebuilt only when a
# container is next created -- never under a running one.
printf '# an edit\n' >> "$IMG_DIR/Dockerfile"
cc version
check 'after an edit, version finds it out of date' 1 "$(grep -c "^local     : out of date .*before an edit" "$OUT")"
image_before="$(docker image inspect -f '{{.Id}}' "$IMAGE" 2>/dev/null)"
cc claude --version
check_rc 'a launch still works' 0
check 'it says so' 1 "$(has "$ERR" "is out of date")"
check 'and builds nothing' "$image_before" "$(docker image inspect -f '{{.Id}}' "$IMAGE" 2>/dev/null)"
check 'nor touches the container' "$container_id" \
    "$(docker inspect --type=container -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"
rm -rf "$IMG_DIR"

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
