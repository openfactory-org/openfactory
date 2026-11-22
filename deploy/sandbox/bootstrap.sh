#!/bin/sh
# Starts the Open Factory runner in a sandbox. Runs as root. Every provider
# runs the same command:
#
#   /usr/bin/tini -g -- /opt/openfactory/bootstrap.sh
#
# Environment (the sandbox image and bootstrap contract):
#   OF_CONTROL_URL, OF_RUN_ID, OF_SESSION, OF_BOOTSTRAP_TOKEN   required
#   OF_RUNNER_URL      where to download openfactory-runtime (control-plane route or presigned R2)
#   OF_RUNNER_SHA256   sha256 of that binary. Required when OF_RUNNER_URL is set.
#   OF_RUNNER_VERSION  version the control plane wants (the runner checks it at session start)
#   OF_EGRESS_LOCK     none | netns | iptables (read by the runner)
#   OF_ENV_FILE        optional root-only file with `export NAME='value'` lines
#                      (boat.dev: its command API has no env field)
#   OF_TRUST_CA        optional CA file to trust first (Cloudflare HTTPS intercept)
#
# The baked binary is used when its sha256 matches, so a matching image
# downloads nothing.
#
# Exit codes: 64 unsupported platform, 65 checksum mismatch or no runner
# (both map to invalid_spec), else the code of the runner.

set -eu

if [ -n "${OF_ENV_FILE:-}" ] && [ -f "$OF_ENV_FILE" ]; then
	# Only a root-owned file that others cannot read or write.
	if [ "$(stat -c %u:%a "$OF_ENV_FILE")" != "0:600" ]; then
		echo "bootstrap: $OF_ENV_FILE must be owned by root with mode 0600" >&2
		exit 65
	fi
	# shellcheck disable=SC1090 # the file is written by the provider setup script
	. "$OF_ENV_FILE"
fi

: "${OF_CONTROL_URL:?}" "${OF_RUN_ID:?}" "${OF_SESSION:?}" "${OF_BOOTSTRAP_TOKEN:?}"

case "$(uname -m)" in
x86_64) A=amd64 ;;
aarch64 | arm64) A=arm64 ;;
*)
	echo "bootstrap: unsupported architecture $(uname -m)" >&2
	exit 64
	;;
esac

D=/opt/openfactory/bin
BIN=$D/openfactory-runtime
umask 022
mkdir -p "$D" /run/openfactory

# Cloudflare: trust the intercept CA before any HTTPS call (no-op elsewhere).
if [ -n "${OF_TRUST_CA:-}" ]; then
	/opt/openfactory/trust-ca.sh "$OF_TRUST_CA"
	SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
	CURL_CA_BUNDLE=$SSL_CERT_FILE
	GIT_SSL_CAINFO=$SSL_CERT_FILE
	REQUESTS_CA_BUNDLE=$SSL_CERT_FILE
	NODE_EXTRA_CA_CERTS=$OF_TRUST_CA
	export SSL_CERT_FILE CURL_CA_BUNDLE GIT_SSL_CAINFO REQUESTS_CA_BUNDLE NODE_EXTRA_CA_CERTS
fi

sum_ok() { [ -f "$2" ] && echo "$1  $2" | sha256sum -c --status 2>/dev/null; }

if [ -n "${OF_RUNNER_URL:-}" ]; then
	WANT=${OF_RUNNER_SHA256:-}
	case $WANT in
	[0-9a-f]*) ;;
	*)
		echo "bootstrap: OF_RUNNER_SHA256 is missing or not a sha256" >&2
		exit 65
		;;
	esac
	[ ${#WANT} -eq 64 ] || {
		echo "bootstrap: OF_RUNNER_SHA256 is not a sha256" >&2
		exit 65
	}
	if ! sum_ok "$WANT" "$BIN"; then
		# OF_RUNNER_URL may name the arch-neutral route; fill in the arch.
		U=$(printf '%s' "$OF_RUNNER_URL" | sed "s/{arch}/$A/g")
		curl -fsSL --retry 6 --retry-all-errors --max-time 300 -o "$BIN.tmp" "$U"
		if ! sum_ok "$WANT" "$BIN.tmp"; then
			rm -f "$BIN.tmp"
			echo "bootstrap: runner checksum mismatch" >&2
			exit 65
		fi
		chmod 0755 "$BIN.tmp"
		mv -f "$BIN.tmp" "$BIN"
	fi
elif [ -n "${OF_RUNNER_SHA256:-}" ] && ! sum_ok "$OF_RUNNER_SHA256" "$BIN"; then
	echo "bootstrap: the baked runner does not match OF_RUNNER_SHA256, and OF_RUNNER_URL is not set" >&2
	exit 65
fi

if [ ! -x "$BIN" ]; then
	echo "bootstrap: no runner at $BIN and no OF_RUNNER_URL" >&2
	exit 65
fi

echo $$ >/run/openfactory/runner.pid
exec "$BIN" runner
