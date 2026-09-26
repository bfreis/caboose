#!/bin/sh
# caboose layer user setup: what layer.Dockerfile runs, as root, to give the
# image an `agent` user with the host's UID and GID, and its home:
#
#     layer-user.sh UID GID [ROOT]
#
# The image is the user's (see docs/images.md, "Requirements for your own
# image"), so this depends on nothing the image check does not guarantee:
# plain POSIX sh, mkdir and chown. No useradd, adduser or groupadd, which
# some images have in one flavour, some in the other and some not at all:
# /etc/passwd, /etc/group, /etc/shadow and /etc/gshadow are edited directly,
# with sh builtins. Each is rewritten in place rather than replaced, so it
# keeps its mode and owner (shadow is root:shadow 0640 on Debian).
#
# UID and GID 0 are refused (caboose must not run as root), as is a
# /home/agent that is a symlink. The rules, in the order they apply:
#
#   - passwd: the entry holding UID is taken over and renamed agent, its home
#     /home/agent, its shell the image's bash, its primary group the one
#     below; its password field and GECOS stay. That is the ubuntu or node
#     user of stock images, which is why the default image evicted by UID. An
#     entry already named agent with UID is preferred over any other holder.
#     Any other entry named agent, holding some other UID, is dropped, so the
#     name stays unique. With no holder, a new entry is appended.
#   - shadow, when there is one: the taken-over entry is renamed with it,
#     hash and ageing included; other agent entries are dropped; and if agent
#     then has none, it gets a locked one (`agent:!:...`), as useradd writes.
#     su, sudo and sshd go through PAM's pam_unix, whose account check fails
#     for a user whose passwd says "x" but who has no shadow entry -- so a
#     `sudo` the image's author configured would break without one. With no
#     shadow file, a new entry's password field is "!" instead of "x".
#   - group: a group already holding GID is the primary group as it is, name
#     and all -- it may be a system group (a macOS host's GID 20 is dialout
#     on Debian), and renaming that would break whatever depends on it. A
#     group named agent at another GID is then dropped if nothing uses it (no
#     members, nobody's primary group), in gshadow too. Otherwise any group
#     named agent is dropped and `agent` added with GID, in gshadow too when
#     there is one.
#   - New entries go in before the first NIS compat line (`+...` or `-...`),
#     if a file has one, rather than after it, where NIS would answer first.
#   - Supplementary groups are membership lists of names, and are left
#     alone: a taken-over user's memberships stay with its old name, which no
#     longer exists (so, like userdel, it loses them), and lists naming agent
#     now mean this agent.
#
# Running it again changes nothing, and a file without a trailing newline
# comes out with one. ROOT, for tests only, prefixes every path it touches;
# commands are found on PATH, so tests can stub chown.

uid=${1:-}
gid=${2:-}
R=${3:-}
home=/home/agent

die() {
    printf 'caboose layer: %s\n' "$*" >&2
    exit 1
}

case $uid in '' | *[!0-9]*) die "UID must be a number, got '$uid'" ;; esac
case $gid in '' | *[!0-9]*) die "GID must be a number, got '$gid'" ;; esac
# The IDs are the host user's. Root's would take over the image's root user
# (renamed agent, its home and shell changed), and root's group would become
# agent's: the image check refuses both, and so does this.
[ "$uid" -ne 0 ] || die "UID 0: caboose must not run as root: the layer would take over the image's root user; run it as a regular user"
[ "$gid" -ne 0 ] || die "GID 0: caboose must not run with group root: the layer would make root agent's group; run it as a regular user"
# A symlinked home would take the mountpoints wherever it points, and chown
# -R does not follow it there: refused, as the image check does.
[ ! -L "$R$home" ] || die "$home is a symlink: the layer makes it a directory of agent's own, so the image must not have one there"

nl='
'

# The login shell: /bin/bash where the image has it, as every distro lists
# it in /etc/shells, else the first bash on PATH (the check found one).
shell=""
if [ -x "$R/bin/bash" ]; then
    shell=/bin/bash
else
    _ifs=$IFS
    IFS=:
    set -f
    for _d in $PATH; do
        if [ -n "$_d" ] && [ -f "$R$_d/bash" ] && [ -x "$R$_d/bash" ]; then
            shell=$_d/bash
            break
        fi
    done
    IFS=$_ifs
    set +f
fi
[ -n "$shell" ] || die "no bash in the image: the image check should have refused it"

passwd=$R/etc/passwd
group=$R/etc/group
shadow=$R/etc/shadow
gshadow=$R/etc/gshadow
[ -f "$passwd" ] || die "no /etc/passwd"
[ -f "$group" ] || die "no /etc/group"

# field N LINE: the Nth colon-separated field of LINE, into $f. Parameter
# expansion rather than IFS splitting, which drops a trailing empty field.
field() {
    f=$2
    _k=1
    while [ "$_k" -lt "$1" ]; do
        case $f in *:*) f=${f#*:} ;; *) f=""; return ;; esac
        _k=$((_k + 1))
    done
    f=${f%%:*}
}

# write FILE CONTENT, in place.
write() {
    printf '%s' "$2" > "$1" || die "cannot write ${1#"$R"}"
}

# keep LINE: LINE, kept, into $out -- or into $tail from the first NIS compat
# line on (+ or -, which pull NIS entries in or mask them), so an entry added
# between the two goes in before NIS's, as a local one should: after a `+`,
# NIS would answer for the name first. Every loop below starts with both
# empty and writes "$out<new entries>$tail".
keep() {
    if [ -n "$tail" ]; then
        tail="$tail$1$nl"
    else
        case $1 in
            [+-]*) tail="$1$nl" ;;
            *) out="$out$1$nl" ;;
        esac
    fi
}

# drop FILE: FILE without its entries named agent, into $out and $tail.
drop() {
    out=""
    tail=""
    while IFS= read -r l || [ -n "$l" ]; do
        case $l in agent:*) continue ;; esac
        keep "$l"
    done < "$1"
}

# --- passwd ---------------------------------------------------------------

# Who holds UID, preferring an entry that is agent already.
holder=""
while IFS= read -r l || [ -n "$l" ]; do
    case $l in *:*:*:*:*:*:*) ;; *) continue ;; esac
    field 3 "$l"
    if [ "$f" = "$uid" ]; then
        if [ "${l%%:*}" = agent ]; then
            holder=agent
        elif [ -z "$holder" ]; then
            holder=${l%%:*}
        fi
    fi
done < "$passwd"

new_pw=x
[ -f "$shadow" ] || new_pw='!'

out=""
tail=""
taken=no
while IFS= read -r l || [ -n "$l" ]; do
    case $l in
        *:*:*:*:*:*:*)
            n=${l%%:*}
            field 3 "$l"
            if [ "$taken" = no ] && [ -n "$holder" ] && [ "$n" = "$holder" ] && [ "$f" = "$uid" ]; then
                field 2 "$l"
                pw=$f
                field 5 "$l"
                l="agent:$pw:$uid:$gid:$f:$home:$shell"
                taken=yes
            elif [ "$n" = agent ]; then
                continue
            fi
            ;;
    esac
    keep "$l"
done < "$passwd"
if [ "$taken" = no ]; then
    out="${out}agent:$new_pw:$uid:$gid::$home:$shell$nl"
fi
write "$passwd" "$out$tail"

# --- shadow ---------------------------------------------------------------

if [ -f "$shadow" ]; then
    out=""
    tail=""
    have=no
    while IFS= read -r l || [ -n "$l" ]; do
        case $l in
            *:*)
                n=${l%%:*}
                if [ "$have" = no ] && [ -n "$holder" ] && [ "$n" = "$holder" ]; then
                    l="agent:${l#*:}"
                    have=yes
                elif [ "$n" = agent ]; then
                    continue
                fi
                ;;
        esac
        keep "$l"
    done < "$shadow"
    if [ "$have" = no ]; then
        # name, password, then seven empty fields: last change, min, max,
        # warn, inactive, expire, reserved.
        out="${out}agent:!:::::::$nl"
    fi
    write "$shadow" "$out$tail"
fi

# --- group ----------------------------------------------------------------

held=no
while IFS= read -r l || [ -n "$l" ]; do
    case $l in *:*:*:*) ;; *) continue ;; esac
    field 3 "$l"
    if [ "$f" = "$gid" ]; then
        held=yes
        break
    fi
done < "$group"

if [ "$held" = no ]; then
    drop "$group"
    write "$group" "${out}agent:x:$gid:$nl$tail"
    if [ -f "$gshadow" ]; then
        drop "$gshadow"
        write "$gshadow" "${out}agent:!::$nl$tail"
    fi
else
    # A group named agent at another GID -- the image's own, left behind
    # when its agent user was dropped or taken over -- would only mislead:
    # `id` shows the other group, and anything chgrp'd to agent gets that
    # one. It goes, from gshadow too, when nothing uses it: no members, and
    # no user's primary group.
    stale=""
    while IFS= read -r l || [ -n "$l" ]; do
        case $l in agent:*:*:*) ;; *) continue ;; esac
        field 3 "$l"
        _g=$f
        field 4 "$l"
        if [ "$_g" != "$gid" ] && [ -z "$f" ]; then
            stale=$_g
        fi
        break
    done < "$group"
    if [ -n "$stale" ]; then
        while IFS= read -r l || [ -n "$l" ]; do
            case $l in *:*:*:*:*:*:*) ;; *) continue ;; esac
            field 4 "$l"
            if [ "$f" = "$stale" ]; then
                stale=""
                break
            fi
        done < "$passwd"
    fi
    if [ -n "$stale" ]; then
        drop "$group"
        write "$group" "$out$tail"
        if [ -f "$gshadow" ]; then
            drop "$gshadow"
            write "$gshadow" "$out$tail"
        fi
    fi
fi

# --- home -----------------------------------------------------------------

# Every directory a bind mount lands inside, owned by agent. Docker creates
# missing mountpoint parents itself, but as root -- which left ~/.local
# root-owned and made the installer fail on mkdir ~/.local/state.
# Directories that already exist keep their ownership when mounted into.
# ~/.config matters beyond its three mounts: left to Docker it would be
# root-owned, and every other tool that keeps config there would fail. The
# list is the launcher's mounts (internal/launcher/container.go).
for d in .claude .local/bin .local/share/claude .local/state .cache/claude \
         .config/git .config/jj .config/gh; do
    mkdir -p "$R$home/$d" || die "cannot create $home/$d"
done
chown -R "$uid:$gid" "$R$home" || die "cannot chown $home"
exit 0
