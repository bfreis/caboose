#!/bin/sh
# caboose image probe: what `caboose check-image` runs inside a
# throwaway container of the image being checked, as
#
#     docker run --rm --init [--platform P] --user 0:0 -e CABOOSE_PROBE_ROOT= \
#         --entrypoint /bin/sh IMAGE -c "$script" caboose-probe UID GID
#
# It reports facts and leaves the verdict to the launcher (internal/imagecheck),
# which holds the requirements table: which checks are required, which only on
# musl, which optional, and why each one matters.
#
# Plain POSIX sh on purpose. It runs on images that may have no bash -- saying
# so is part of its job -- and uses nothing but sh builtins except for the
# tools it is checking, so a missing tool shows up as a "missing" line rather
# than breaking the probe. It never exits non-zero and writes nothing to
# stderr: docker's exit status and stderr belong to docker, which is how the
# launcher tells an image with no /bin/sh from one the probe ran in.
#
# Output, one line per check, fields separated by single spaces:
#
#     probe 1                 first line: the format version
#     ok NAME DETAIL          a check passed; DETAIL is usually where
#     missing NAME [DETAIL]   it failed; DETAIL, if any, says how
#     warn NAME DETAIL        worth knowing, never a failure (reachability)
#     info KEY VALUE...       a fact: libc, arch, platform, uid/gid holders
#                             (and the uid holder's home and shell)
#     end                     last line, so truncated output is detectable
#
# CABOOSE_PROBE_ROOT, for tests only, prefixes every file the probe looks at
# (the launcher clears it, in case an image sets it); commands are found on
# PATH, so tests simulate a missing tool with a PATH of stubs.

R="${CABOOSE_PROBE_ROOT:-}"
host_uid="${1:-}"
host_gid="${2:-}"

echo "probe 1"
# Being here at all is the check.
echo "ok sh /bin/sh"

# lookpath NAME prints the first executable NAME on PATH. Not `command -v`:
# for a shell builtin such as test that prints the bare name, and the
# launcher's `docker exec CONTAINER test ...` needs a real file.
lookpath() {
    _old_ifs=$IFS
    IFS=:
    set -f
    for _dir in $PATH; do
        [ -n "$_dir" ] || _dir=.
        if [ -f "$_dir/$1" ] && [ -x "$_dir/$1" ]; then
            IFS=$_old_ifs
            set +f
            printf '%s\n' "$_dir/$1"
            return 0
        fi
    done
    IFS=$_old_ifs
    set +f
    return 1
}

# presence NAME [LABEL]: ok/missing for a command on PATH, reported as LABEL.
presence() {
    if _p=$(lookpath "$1"); then
        echo "ok ${2:-$1} $_p"
    else
        echo "missing ${2:-$1}"
    fi
}

presence bash
presence curl
presence tmux

# git: `caboose sync` runs every git command in the container. 2.28 is
# the first with `git init -b`. Run, not just found, so a git that cannot
# start on this image counts as missing.
if _p=$(lookpath git); then
    _v=$("$_p" --version 2>/dev/null)
    _v=${_v#git version }
    _v=${_v%% *}
    _maj=${_v%%.*}
    _min=${_v#*.}
    _min=${_min%%.*}
    case "$_maj.$_min" in
        .* | *. | *[!0-9.]*)
            echo "missing git $_p does not report a version" ;;
        *)
            if [ "$_maj" -gt 2 ] || { [ "$_maj" -eq 2 ] && [ "$_min" -ge 28 ]; }; then
                echo "ok git $_p $_v"
            else
                echo "missing git $_v is older than 2.28"
            fi ;;
    esac
else
    echo "missing git"
fi

presence tic

# --- The tools entrypoint.sh, the installer and the launcher call ----------
#
# entrypoint.sh's shebang is `#!/usr/bin/env bash`, so env has to be at that
# exact path, not merely on PATH.
if [ -x "$R/usr/bin/env" ]; then
    echo "ok tool:env /usr/bin/env"
else
    echo "missing tool:env not at /usr/bin/env"
fi

# entrypoint.sh resolves the active version with readlink -f.
if _p=$(lookpath readlink); then
    if [ "$(readlink -f / 2>/dev/null)" = / ]; then
        echo "ok tool:readlink $_p"
    else
        echo "missing tool:readlink no -f"
    fi
else
    echo "missing tool:readlink"
fi

# entrypoint.sh downloads the installer to a mktemp file, and prunes version
# directories with find -mindepth 1 -delete: both tried for real.
_tmp=""
if _p=$(lookpath mktemp); then
    _tmp=$(mktemp -d 2>/dev/null) || _tmp=""
    if [ -n "$_tmp" ] && [ -d "$_tmp" ]; then
        echo "ok tool:mktemp $_p"
    else
        _tmp=""
        echo "missing tool:mktemp mktemp -d failed"
    fi
else
    echo "missing tool:mktemp"
fi
if _p=$(lookpath find); then
    if [ -n "$_tmp" ] && : > "$_tmp/probe"; then
        find "$_tmp" -mindepth 1 -delete >/dev/null 2>&1
        if [ -e "$_tmp/probe" ]; then
            echo "missing tool:find no -mindepth/-delete"
        else
            echo "ok tool:find $_p"
        fi
    else
        echo "ok tool:find $_p (-delete not verified: no temp dir)"
    fi
else
    echo "missing tool:find"
fi
if [ -n "$_tmp" ]; then
    rm -f "$_tmp/probe" 2>/dev/null
    rmdir "$_tmp" 2>/dev/null
fi

# The rest only need to exist. test: the launcher's readiness check is
# `docker exec CONTAINER test -f ...`, which execs a file, not a builtin.
# uname, mkdir, chmod, cut, sed, tr, grep, head and sha256sum: what Claude
# Code's installer runs on its way to a verified binary. chown: what the
# derived layer's layer-user.sh gives the agent user its home with, the one
# tool it uses beyond sh builtins and mkdir.
for _t in rm rmdir sleep test uname mkdir chmod cut sed tr grep head sha256sum chown; do
    presence "$_t" "tool:$_t"
done

# --- libc and platform, exactly as https://claude.ai/install.sh decides -----
#
# The installer's test, verbatim but for grep: `[ -f /lib/libc.musl-x86_64.so.1 ]
# || [ -f /lib/libc.musl-aarch64.so.1 ] || ldd /bin/ls 2>&1 | grep -q musl`.
# A case match on ldd's output is the same test without depending on grep.
# Anything else the installer takes for glibc; the probe also looks for
# glibc itself, so an image with neither is reported rather than handed a
# glibc build it cannot run.
libc=""
libc_at=""
if [ -f "$R/lib/libc.musl-x86_64.so.1" ]; then
    libc=musl libc_at=/lib/libc.musl-x86_64.so.1
elif [ -f "$R/lib/libc.musl-aarch64.so.1" ]; then
    libc=musl libc_at=/lib/libc.musl-aarch64.so.1
else
    case "$(ldd "$R/bin/ls" 2>&1)" in
        *musl*) libc=musl libc_at="ldd /bin/ls" ;;
    esac
fi
if [ -z "$libc" ]; then
    for _f in "$R"/lib/libc.so.6 "$R"/lib64/libc.so.6 "$R"/lib/*/libc.so.6 \
              "$R"/usr/lib/libc.so.6 "$R"/usr/lib64/libc.so.6 "$R"/usr/lib/*/libc.so.6; do
        if [ -e "$_f" ]; then
            libc=glibc libc_at=${_f#"$R"}
            break
        fi
    done
fi
if [ -n "$libc" ]; then
    echo "info libc $libc"
    echo "ok libc $libc $libc_at"
else
    echo "info libc unknown"
    echo "missing libc neither glibc nor musl"
fi

# The installer's mapping of `uname -m`, and its platform name:
# linux-<x64|arm64>, with -musl appended on musl.
_m=$(uname -m 2>/dev/null)
echo "info arch ${_m:-unknown}"
case "$_m" in
    x86_64|amd64) _arch=x64 ;;
    arm64|aarch64) _arch=arm64 ;;
    *) _arch="" ;;
esac
if [ -n "$_arch" ]; then
    echo "ok arch $_m"
    if [ "$libc" = musl ]; then
        echo "info platform linux-$_arch-musl"
    else
        echo "info platform linux-$_arch"
    fi
else
    echo "missing arch ${_m:-unknown}"
fi

# What the musl build needs at run time (Claude Code's Alpine instructions):
# the GCC runtime libraries, and a system ripgrep in place of the bundled one.
if [ "$libc" = musl ]; then
    findlib() {
        for _d in /lib /usr/lib /usr/local/lib /lib64 /usr/lib64; do
            if [ -e "$R$_d/$1" ]; then
                echo "ok $2 $_d/$1"
                return 0
            fi
        done
        echo "missing $2 no $1"
    }
    findlib libgcc_s.so.1 libgcc
    findlib libstdc++.so.6 libstdc++
    presence rg ripgrep
fi

# --- The host UID and GID ------------------------------------------------
#
# Who holds them now, from the files themselves (getent may be absent). Not
# a failure: the layer takes that entry over for the agent user. An existing
# user or group called agent is reported too, since the layer adds its own.
# The `|| [ -n "$_n" ]` keeps a last line with no newline.
holder() { # FILE ID: the name on the first entry whose third field is ID
    [ -f "$1" ] && [ -r "$1" ] || return 1
    while IFS=: read -r _n _ _i _ || [ -n "$_n" ]; do
        if [ "$_i" = "$2" ]; then
            printf '%s\n' "$_n"
            return 0
        fi
        _n=""
    done < "$1"
    return 1
}
named() { # FILE NAME: the third field of the entry called NAME
    [ -f "$1" ] && [ -r "$1" ] || return 1
    while IFS=: read -r _n _ _i _ || [ -n "$_n" ]; do
        if [ "$_n" = "$2" ]; then
            printf '%s\n' "$_i"
            return 0
        fi
        _n=""
    done < "$1"
    return 1
}
# field FILE NAME N: the Nth field (5 = home, 6 = shell) of the passwd
# entry called NAME. Parameter expansion, not IFS, so an empty field stays.
field() {
    [ -f "$1" ] && [ -r "$1" ] || return 1
    while IFS= read -r _l || [ -n "$_l" ]; do
        if [ "${_l%%:*}" = "$2" ]; then
            _k=0
            while [ "$_k" -lt "$3" ]; do
                case $_l in *:*) _l=${_l#*:} ;; *) _l="" ;; esac
                _k=$((_k + 1))
            done
            printf '%s\n' "${_l%%:*}"
            return 0
        fi
    done < "$1"
    return 1
}
if [ -n "$host_uid" ]; then
    if _h=$(holder "$R/etc/passwd" "$host_uid"); then
        echo "info uid $host_uid $_h"
        # What the layer's takeover would change for that account: a
        # system one (a nologin shell, Debian's _apt at 100) is worth a note.
        if _v=$(field "$R/etc/passwd" "$_h" 5); then echo "info uid-home $_v"; fi
        if _v=$(field "$R/etc/passwd" "$_h" 6); then echo "info uid-shell $_v"; fi
    else
        echo "info uid $host_uid"
    fi
fi
if [ -n "$host_gid" ]; then
    if _h=$(holder "$R/etc/group" "$host_gid"); then
        echo "info gid $host_gid $_h"
    else
        echo "info gid $host_gid"
    fi
fi
if _i=$(named "$R/etc/passwd" agent); then
    echo "info user agent $_i"
fi
if _i=$(named "$R/etc/group" agent); then
    echo "info group agent $_i"
fi

# --- What the layer's user setup edits and creates -------------------------
#
# layer-user.sh rewrites /etc/passwd and /etc/group in place, and makes
# /home/agent and the mountpoints in it, then chowns the lot. Whatever it
# would die on, the probe can see first: a missing (or non-regular) account
# file; a /home/agent that is a symlink, which chown -R would not follow into
# while the bind mounts land wherever it points; or anything on the way to a
# mountpoint that is there but is not a directory.
for _f in passwd group; do
    if [ -L "$R/etc/$_f" ]; then
        echo "missing etc:$_f /etc/$_f is a symlink"
    elif [ -f "$R/etc/$_f" ]; then
        echo "ok etc:$_f /etc/$_f"
    elif [ -e "$R/etc/$_f" ]; then
        echo "missing etc:$_f /etc/$_f is not a regular file"
    else
        echo "missing etc:$_f no /etc/$_f"
    fi
done
_bad=""
for _d in /home /home/agent /home/agent/.claude /home/agent/.local /home/agent/.local/bin \
          /home/agent/.local/share /home/agent/.local/share/claude /home/agent/.local/state \
          /home/agent/.cache /home/agent/.cache/claude /home/agent/.config \
          /home/agent/.config/git /home/agent/.config/jj /home/agent/.config/gh; do
    if [ "$_d" != /home ] && [ -L "$R$_d" ]; then
        _bad="$_d is a symlink"
        break
    elif [ -e "$R$_d" ] && [ ! -d "$R$_d" ]; then
        _bad="$_d is not a directory"
        break
    fi
done
if [ -n "$_bad" ]; then
    echo "missing home $_bad"
elif [ -d "$R/home/agent" ]; then
    echo "ok home /home/agent"
else
    echo "ok home /home/agent (created by the layer)"
fi

# --- CA certificates and reachability ------------------------------------
#
# Kept apart, so a build machine with no network does not read as an image
# with no CA certificates: the bundle is looked for at the paths the major
# distros' curl and OpenSSL use, and a trusted https answer counts too.
# Reachability is only ever a warning: a proxy may be configured later.
_ca=""
for _f in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt \
          /etc/ssl/ca-bundle.pem /etc/pki/tls/cacert.pem \
          /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/ssl/cert.pem; do
    if [ -s "$R$_f" ]; then
        _ca=$_f
        break
    fi
done
_verified=""
_untrusted=""
# The installer's first two requests: install.sh, then the release channel.
for _url in https://claude.ai https://downloads.claude.ai; do
    if ! lookpath curl >/dev/null; then
        echo "warn net $_url not checked: no curl"
        continue
    fi
    # Any HTTP answer is reachable: the question is TLS and routing, not
    # what the page says. -f would call a 403 from a CDN unreachable.
    curl -s -o /dev/null --connect-timeout 5 --max-time 10 "$_url" >/dev/null 2>&1
    _rc=$?
    case "$_rc" in
        0) echo "ok net $_url"; _verified=$_url ;;
        35|51|58|59|60|66|77|80|83|90|91)
            echo "warn net $_url TLS failed (curl exit $_rc)"; _untrusted=$_url ;;
        *) echo "warn net $_url unreachable (curl exit $_rc)" ;;
    esac
done
if [ -n "$_ca" ]; then
    echo "ok cacerts $_ca"
elif [ -n "$_verified" ]; then
    echo "ok cacerts no bundle at the usual paths, but $_verified verified"
elif [ -n "$_untrusted" ]; then
    echo "missing cacerts no bundle, and $_untrusted failed TLS"
else
    echo "missing cacerts no bundle at the usual paths"
fi

echo "end"
exit 0
