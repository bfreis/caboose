#!/bin/sh
# macsign.sh: sign a darwin binary of caboose's, with rcodesign, on any OS.
#
#   macsign.sh [--adhoc] IDENTIFIER BINARY [ENTITLEMENTS]
#
# With a Developer ID configured (MACSIGN_P12, the .p12's path, and
# MACSIGN_P12_PASSWORD_FILE, a file holding its password), BINARY is signed
# with it, in place, with the hardened runtime (which notarization
# requires) and Apple's timestamp, and with ENTITLEMENTS when given. Then,
# when MACSIGN_API_KEY names an App Store Connect API key's JSON (rcodesign
# encode-app-store-connect-api-key), it is notarized: zipped, since Apple's
# notary takes no bare Mach-O, and submitted, waiting for the verdict. A
# bare binary cannot be stapled, so Gatekeeper looks the ticket up online.
#
# Without a Developer ID, --adhoc signs it ad-hoc (with ENTITLEMENTS),
# and without --adhoc it is left as it is. Any failure fails the script:
# a release whose secrets are set never falls back to ad-hoc.
#
# Run by goreleaser's post-build hooks (.goreleaser.yaml), with the files
# release.yml writes from its secrets, and by `make vmm` on a Mac with
# DEVID_P12 and its friends. It never prints what the files hold.
set -eu

adhoc=
if [ "${1-}" = --adhoc ]; then
	adhoc=1
	shift
fi
if [ $# -lt 2 ] || [ $# -gt 3 ]; then
	echo "usage: macsign.sh [--adhoc] IDENTIFIER BINARY [ENTITLEMENTS]" >&2
	exit 2
fi
id=$1 bin=$2 ent=${3-}

set -- --binary-identifier "$id"
if [ -n "$ent" ]; then
	set -- "$@" --entitlements-xml-file "$ent"
fi

if [ -z "${MACSIGN_P12-}" ]; then
	if [ -n "${MACSIGN_API_KEY-}" ]; then
		echo "macsign.sh: MACSIGN_API_KEY is set without MACSIGN_P12: only a Developer ID signature can be notarized" >&2
		exit 1
	fi
	if [ -n "$adhoc" ]; then
		rcodesign sign "$@" "$bin"
	fi
	exit 0
fi

for f in "$MACSIGN_P12" "${MACSIGN_P12_PASSWORD_FILE:?MACSIGN_P12 is set without MACSIGN_P12_PASSWORD_FILE}"; do
	[ -s "$f" ] || { echo "macsign.sh: $f is missing or empty" >&2; exit 1; }
done
rcodesign sign "$@" --code-signature-flags runtime \
	--p12-file "$MACSIGN_P12" --p12-password-file "$MACSIGN_P12_PASSWORD_FILE" "$bin"

if [ -z "${MACSIGN_API_KEY-}" ]; then
	echo "macsign.sh: $bin signed with a Developer ID, not notarized (no MACSIGN_API_KEY)" >&2
	exit 0
fi
[ -s "$MACSIGN_API_KEY" ] || { echo "macsign.sh: $MACSIGN_API_KEY is missing or empty" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# Named after the binary, so Apple's log says which one it was.
zip -qj "$tmp/$id.zip" "$bin"
rcodesign notary-submit --api-key-file "$MACSIGN_API_KEY" --wait --max-wait-seconds 1800 "$tmp/$id.zip"
