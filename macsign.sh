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
# notary takes no bare Mach-O, and submitted once (never resubmitted: a
# second submission is a second notarization of the same binary, and a slow
# one stays slow), then polled for the verdict. A poll that fails to reach
# Apple is retried after a pause, so one transient error never costs a
# release; only a verdict (Invalid and Rejected print Apple's log) or the
# deadline, MACSIGN_NOTARY_TIMEOUT seconds (default 7200), ends it. A bare
# binary cannot be stapled, so Gatekeeper looks the ticket up online.
# MACSIGN_NOTARY_RETRY_PAUSE (seconds, default 30) is the pause between
# polls and, growing up to fourfold, between failed ones; tests shorten it.
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
# rcodesign 0.29.0, from its source: notary-submit without --wait uploads
# and exits 0 having logged "created submission ID: <uuid>" (on stderr,
# with the rest of its log). notary-wait ID --max-wait-seconds N polls
# every 3 s, a fixed interval it has no option for, logging "poll state
# after Ns: <Status>", and exits 0 on any verdict, Invalid and Rejected
# included (unlike notary-submit --wait), having logged Apple's log; with
# the submission still in progress after N s it exits 1 with "Error:
# reached time limit waiting for notarization to complete"; any other
# failure, a request that did not get through included, is another
# "Error: ..." and exit 1. So one poll per call (N=0), paced here, and the
# verdict is read from the status line.
timeout=${MACSIGN_NOTARY_TIMEOUT:-7200}
pause=${MACSIGN_NOTARY_RETRY_PAUSE:-30}
key=$MACSIGN_API_KEY

if out=$(rcodesign notary-submit --api-key-file "$key" "$tmp/$id.zip" 2>&1); then
	rc=0
else
	rc=$?
fi
printf '%s\n' "$out" >&2
sub=$(printf '%s\n' "$out" | sed -n 's/.*created submission ID: \([0-9A-Za-z-]*\).*/\1/p' | tail -n 1)
if [ -z "$sub" ]; then
	echo "macsign.sh: $bin: notary-submit gave no submission ID (exit $rc): nothing was submitted" >&2
	exit 1
fi
if [ "$rc" -ne 0 ]; then
	echo "macsign.sh: $bin: notary-submit failed (exit $rc) after creating submission $sub; not resubmitting: check it with: rcodesign notary-log --api-key-file KEY $sub" >&2
	exit 1
fi
echo "macsign.sh: $bin: submitted as $sub; waiting for Apple's verdict (up to ${timeout}s)" >&2

start=$(date +%s)
fails=0
while :; do
	if out=$(rcodesign notary-wait --api-key-file "$key" --max-wait-seconds 0 "$sub" 2>&1); then
		rc=0
	else
		rc=$?
	fi
	printf '%s\n' "$out" >&2
	state=$(printf '%s\n' "$out" | sed -n 's/.*poll state after [0-9]*s: \([A-Za-z]*\).*/\1/p' | tail -n 1)
	if [ "$rc" -eq 0 ]; then
		case $state in
		Accepted)
			echo "macsign.sh: $bin: notarized ($sub)" >&2
			exit 0
			;;
		Invalid | Rejected | Unknown)
			echo "macsign.sh: $bin: notarization $state ($sub); Apple's log:" >&2
			rcodesign notary-log --api-key-file "$key" "$sub" >&2 || :
			exit 1
			;;
		esac
		# Exit 0 with no verdict we know: do not guess, poll again.
		fails=$((fails + 1))
	elif printf '%s\n' "$out" | grep -q 'reached time limit waiting for notarization'; then
		fails=0
		nap=$pause
	else
		fails=$((fails + 1))
	fi
	if [ "$fails" -gt 0 ]; then
		n=$fails
		[ "$n" -le 4 ] || n=4
		nap=$((pause * n))
		echo "macsign.sh: $bin: poll of $sub failed; retrying in ${nap}s" >&2
	fi
	if [ $(($(date +%s) - start + nap)) -ge "$timeout" ]; then
		echo "macsign.sh: $bin: no verdict on submission $sub after ${timeout}s (MACSIGN_NOTARY_TIMEOUT); the submission is still at Apple, not resubmitted: check it with: xcrun notarytool info $sub, or rcodesign notary-log --api-key-file KEY $sub" >&2
		exit 1
	fi
	sleep "$nap"
done
