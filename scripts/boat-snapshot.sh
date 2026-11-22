#!/bin/sh
# Builds the boat.dev saved snapshot of the Open Factory sandbox: it creates a
# build sandbox from the boat base image, runs deploy/sandbox/install.sh
# --profile boat in it, saves the snapshot and deletes the build sandbox.
# Prints the snapshot name for the boatdev provider (the task image).
#
#   BOAT_API_KEY=boat_... sh scripts/boat-snapshot.sh
#
# Settings (environment variables):
#   BOAT_API_KEY    Required. A key with the ci preset (create, use and delete sandboxes).
#   BOAT_API_URL    Default: https://boat.dev/api/v1.
#   VERSION         Default: git describe, else dev.
#   SNAPSHOT_NAME   Default: of-sandbox-<VERSION with dashes>. Must match ^[a-z0-9][a-z0-9-]{0,62}$.
#   RUNNER          The linux/amd64 openfactory-runtime to bake in. Default:
#                   dist/runner/VERSION/openfactory-runtime-linux-amd64 if it exists (scripts/runner-publish.sh build).
#   CLAUDE_CLI_VERSION, CODEX_CLI_VERSION   Default: latest.
#   TIMEOUT         Seconds to wait for each step. Default: 1800.
#
# Needs curl and jq. Exit status: 0 on success, 1 otherwise.

set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

: "${BOAT_API_KEY:?set BOAT_API_KEY}"
API=${BOAT_API_URL:-https://boat.dev/api/v1}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
NAME=${SNAPSHOT_NAME:-of-sandbox-$(printf '%s' "$VERSION" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9\n' '-')}
RUNNER=${RUNNER:-dist/runner/$VERSION/openfactory-runtime-linux-amd64}
CLAUDE_CLI_VERSION=${CLAUDE_CLI_VERSION:-latest}
CODEX_CLI_VERSION=${CODEX_CLI_VERSION:-latest}
TIMEOUT=${TIMEOUT:-1800}

log() { printf '==> %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

command -v curl >/dev/null 2>&1 || die "curl is not on the PATH"
command -v jq >/dev/null 2>&1 || die "jq is not on the PATH"
printf '%s' "$NAME" | grep -Eq '^[a-z0-9][a-z0-9-]{0,62}$' || die "invalid snapshot name: $NAME"

TMP=$(mktemp -d)
SANDBOX=
cleanup() {
	if [ -n "$SANDBOX" ]; then
		log "delete build sandbox $SANDBOX"
		api DELETE "/sandboxes/$SANDBOX" -H "X-Ascii-Confirm-Delete: $SANDBOX" >/dev/null 2>&1 || true
	fi
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

# The key goes in a header file, so it does not show in the process list.
umask 077
printf 'Authorization: Bearer %s\n' "$BOAT_API_KEY" >"$TMP/auth"

# api METHOD PATH [curl args...]  Prints the response body; fails on HTTP >= 400.
api() {
	m=$1
	p=$2
	shift 2
	curl -fsS -X "$m" -H "@$TMP/auth" -H 'Accept: application/json' "$@" "$API$p"
}

# wait_for DESCRIPTION COMMAND...  Runs the command every 5 s until it succeeds.
wait_for() {
	desc=$1
	shift
	t=0
	until "$@"; do
		t=$((t + 5))
		[ $t -lt "$TIMEOUT" ] || die "timed out: $desc"
		sleep 5
	done
}

sandbox_ready() {
	st=$(api GET "/sandboxes/$SANDBOX" | jq -r '(.sandbox // .).state')
	case $st in
	ready | idle | running) return 0 ;;
	error | cancelled | archived) die "build sandbox is $st" ;;
	esac
	return 1
}

# upload LOCAL REMOTE  Through an upload URL (files above 5 MiB need it).
upload() {
	url=$(api POST "/sandboxes/$SANDBOX/files/upload-url" -H 'Content-Type: application/json' \
		--data "$(jq -nc --arg p "$2" '{path:$p}')" | jq -r '.url // .uploadUrl')
	[ -n "$url" ] && [ "$url" != null ] || die "no upload URL for $2"
	curl -fsS -X PUT --data-binary "@$1" "$url" >/dev/null
}

command_done() {
	r=$(api GET "/commands/$PID")
	st=$(printf '%s' "$r" | jq -r '.status')
	case $st in
	exited)
		code=$(printf '%s' "$r" | jq -r '.exitCode')
		[ "$code" = 0 ] || die "install.sh exited with $code"
		return 0
		;;
	lost | failed) die "install.sh command is $st" ;;
	esac
	return 1
}

snapshot_ready() {
	st=$(api GET "/saved-snapshots/$NAME" | jq -r '(.snapshot // .).status')
	case $st in
	ready) return 0 ;;
	failed | error) die "saved snapshot is $st" ;;
	esac
	return 1
}

log "create build sandbox"
SANDBOX=$(api POST /sandboxes -H 'Content-Type: application/json' \
	--data '{"type":"default","ttlSeconds":3600,"noEnv":true,"labels":{"of-build":"1"}}' |
	jq -r '(.sandbox // .).id')
[ -n "$SANDBOX" ] && [ "$SANDBOX" != null ] || die "boat did not return a sandbox ID"
wait_for "build sandbox ready" sandbox_ready

log "upload install files"
for f in install.sh bootstrap.sh trust-ca.sh; do
	upload "deploy/sandbox/$f" "/tmp/of/$f"
done
runner_arg=
if [ -f "$RUNNER" ]; then
	upload "$RUNNER" /tmp/of/openfactory-runtime
	runner_arg="--runner /tmp/of/openfactory-runtime"
else
	log "no runner at $RUNNER: the snapshot downloads the runner at boot"
fi

log "run install.sh --profile boat"
cmd="sudo -n sh /tmp/of/install.sh --profile boat --claude-version $CLAUDE_CLI_VERSION --codex-version $CODEX_CLI_VERSION $runner_arg"
PID=$(api POST "/sandboxes/$SANDBOX/commands" -H 'Content-Type: application/json' \
	--data "$(jq -nc --arg c "$cmd" '{command:$c, detached:true}')" | jq -r '.processId')
[ -n "$PID" ] && [ "$PID" != null ] || die "boat did not return a process ID"
wait_for "install.sh" command_done

log "save snapshot $NAME"
api POST /saved-snapshots -H 'Content-Type: application/json' \
	--data "$(jq -nc --arg s "$SANDBOX" --arg n "$NAME" '{sandboxId:$s, name:$n}')" >/dev/null
wait_for "saved snapshot ready" snapshot_ready

printf '%s\n' "$NAME"
