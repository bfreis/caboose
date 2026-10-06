#!/bin/sh
# Installs caboose, then runs `caboose setup`:
#
#   curl -fsSL https://github.com/bfreis/caboose/releases/latest/download/install.sh | sh
#
# It downloads the latest release for this OS and architecture, checks it
# against the release's checksums.txt, and installs it the way caboose
# updates itself afterwards (internal/selfupdate, whose layout this must
# match): the binary in ~/.caboose/versions/<tag>/caboose, and
# ~/.local/bin/caboose a symlink to it, the one thing outside ~/.caboose.
# Nothing needs root. Run again, it installs the latest release next to the
# one there, keeping one before it.
#
#   CABOOSE_HOME=DIR         install under DIR instead of ~/.caboose (the
#                            same variable caboose itself reads)
#   CABOOSE_VERSION=v1.2.3   install that release instead of the latest
#   CABOOSE_NO_SETUP=1       install only; do not run caboose setup after
#   CABOOSE_RELEASES_URL     where releases are (tests)
#
# POSIX sh: it runs before anything else is known about the machine.

set -eu

releases="${CABOOSE_RELEASES_URL:-https://github.com/bfreis/caboose/releases}"
releases="${releases%/}"

say() { printf 'caboose: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "this needs $1, which is not installed"; }

need curl
need tar
need uname
need mktemp
[ -n "${HOME:-}" ] || die "HOME is not set: nowhere to install to"

case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) die "caboose runs on macOS and Linux, not on $(uname -s)" ;;
esac
case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "caboose has no build for $(uname -m)" ;;
esac
# A shell under Rosetta says x86_64 on Apple Silicon: install the native one.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
    [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
    arch=arm64
fi

if command -v sha256sum >/dev/null 2>&1; then
    sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
    sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
    die "this needs sha256sum or shasum, to check the download"
fi

# The release: CABOOSE_VERSION, or where releases/latest redirects to --
# no API, so no rate limit.
tag="${CABOOSE_VERSION:-}"
if [ -z "$tag" ]; then
    location="$(curl -fsS -o /dev/null -w '%{redirect_url}' "$releases/latest")" ||
        die "cannot reach $releases/latest"
    tag="${location##*/}"
    # Only a caboose version, vMAJOR.MINOR.PATCH[-PRERELEASE] (roughly
    # internal/selfupdate's Valid, which the updater checks): another
    # release of the repository's, as the vm kernel's source
    # (kernel-VERSION), is never caboose.
    case "$location" in
        */tag/v[0-9]*.[0-9]*.[0-9]*) ;;
        *) tag= ;;
    esac
    case "$tag" in
        '' | *[!0-9A-Za-z.-]*) die "cannot tell the latest release: $releases/latest redirects to '$location', which is no caboose version; set CABOOSE_VERSION=vX.Y.Z to install one" ;;
    esac
fi
case "$tag" in
    v*) ;;
    *) tag="v$tag" ;;
esac

asset="caboose_${tag#v}_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading caboose $tag for $os/$arch"
curl -fsSL -o "$tmp/$asset" "$releases/download/$tag/$asset" ||
    die "cannot download $releases/download/$tag/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$releases/download/$tag/checksums.txt" ||
    die "cannot download $releases/download/$tag/checksums.txt"
want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt of $tag lists no $asset"
got="$(sha256 "$tmp/$asset")"
[ "$got" = "$want" ] || die "$asset does not match its checksum (checksums.txt says $want, the download is $got); nothing was installed"

mkdir "$tmp/x"
tar -xzf "$tmp/$asset" -C "$tmp/x" caboose || die "$asset has no caboose in it"
# caboose-vmm, which runs a vm isolation's VM, is in a macOS archive only.
tar -xzf "$tmp/$asset" -C "$tmp/x" caboose-vmm 2>/dev/null || true

bin="$HOME/.local/bin"
versions="${CABOOSE_HOME:-$HOME/.caboose}/versions"
link="$bin/caboose"
mkdir -p "$bin" "$versions"

# Whole into a dir of its own, then renamed into place.
dest="$versions/$tag"
part="$versions/.$tag.tmp-$$"
rm -rf "$part"
mkdir "$part"
cp "$tmp/x/caboose" "$part/caboose"
chmod 755 "$part" "$part/caboose"
if [ -f "$tmp/x/caboose-vmm" ]; then
    cp "$tmp/x/caboose-vmm" "$part/caboose-vmm"
    chmod 755 "$part/caboose-vmm"
fi
rm -rf "$dest"
mv "$part" "$dest"

# The version the link pointed at before stays, for going back.
before=""
if [ -L "$link" ]; then
    target="$(readlink "$link")"
    case "$target" in
        "$versions"/*/caboose) before="${target#"$versions"/}"; before="${before%/caboose}" ;;
    esac
elif [ -e "$link" ]; then
    say "replacing $link, which is not an install of this script's"
fi
ln -s "$dest/caboose" "$bin/.caboose.new"
mv -f "$bin/.caboose.new" "$link"

for d in "$versions"/v*; do
    [ -d "$d" ] || continue
    case "${d##*/}" in
        "$tag" | "$before") ;;
        *) rm -rf "$d" ;;
    esac
done

say "installed caboose $tag in $dest"
say "it keeps itself up to date (CABOOSE_NO_AUTO_UPDATE=1 turns that off)"

case ":${PATH:-}:" in
    *":$bin:"*) ;;
    *)
        say "$bin is not on your PATH; add it, in your shell's profile:"
        # shellcheck disable=SC2016 # printed for the profile, as written
        say '  export PATH="$HOME/.local/bin:$PATH"'
        ;;
esac

if ! command -v docker >/dev/null 2>&1; then
    say "caboose needs Docker (OrbStack, Docker Desktop or colima), and there is no docker here yet"
elif ! docker info >/dev/null 2>&1; then
    say "docker is installed, but its engine does not answer: start it before using caboose"
fi

# Setup asks questions, so it needs the terminal: under curl | sh, stdin is
# the pipe, so it reads /dev/tty -- when there is one.
if [ -n "${CABOOSE_NO_SETUP:-}" ]; then
    say "next: caboose setup"
elif (exec </dev/tty) 2>/dev/null; then
    say "running caboose setup"
    "$link" setup </dev/tty || say "caboose setup did not finish; run it again with: caboose setup"
else
    say "no terminal to run caboose setup on; run it yourself: caboose setup"
fi
