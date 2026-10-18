#!/bin/sh
# shellcheck disable=SC2034 # the settings are read by the scripts that source this file
# Shared settings and helpers of the local stack scripts (up.sh, down.sh,
# status.sh, env.sh, host.sh). Source it; it runs nothing. Test only.
#
# Settings (environment variables), see docs/self-hosting/local-testing.md:
#   LOCAL_SERVER_MODE  chart (default) or host
#   LOCAL_MODEL        fake (default) or claude. The claude harness runs the CLI
#                      stand-in of deploy/local/testkit (fixed answers, no cost).
#                      With claude, the image also has the real CLI, and a run with
#                      a model credential uses it.
#   LOCAL_FORGE        none (default) or github
#   LOCAL_GITHUB_REPO  owner/repo, with LOCAL_FORGE=github
#   KUBE_CONTEXT       kubectl context. Default: orbstack. The scripts use only this one.
#                      kind-<name>: a kind cluster. up.sh creates it when it does not exist (kind.sh).
#   LOCAL_DOMAIN       Domain of the host names. Default: k8s.orb.local (127.0.0.1.nip.io on kind)
#   LOCAL_DIR          Files of the stack. Default: ~/.openfactory-local
#   LOCAL_BUILD        1 (default) builds the images, 0 uses the images that exist
#   LOCAL_RETENTION    OF_OBJECT_STORE_RETENTION of the server. Default: 5m (sweep every 1m)

# docker and kubectl of OrbStack come first.
PATH="$HOME/.orbstack/bin:$PATH"
export PATH

LOCAL_REPO=$(cd "$(dirname "$0")/../.." && pwd)
LOCAL_DIR=${LOCAL_DIR:-$HOME/.openfactory-local}
# down.sh and status.sh set LOCAL_USE_SAVED=1: a setting that is not in the
# environment then comes from the env file of the last make local-up.
if [ "${LOCAL_USE_SAVED:-0}" = 1 ] && [ -f "$LOCAL_DIR/env" ]; then
	for name in LOCAL_SERVER_MODE LOCAL_MODEL LOCAL_FORGE LOCAL_GITHUB_REPO LOCAL_DOMAIN KUBE_CONTEXT; do
		eval "cur=\${$name:-}"
		if [ -z "$cur" ]; then
			val=$(sed -n "s/^export $name='\(.*\)'\$/\1/p" "$LOCAL_DIR/env")
			[ -z "$val" ] || eval "$name=\$val"
		fi
	done
fi
LOCAL_SERVER_MODE=${LOCAL_SERVER_MODE:-chart}
LOCAL_MODEL=${LOCAL_MODEL:-fake}
LOCAL_FORGE=${LOCAL_FORGE:-none}
LOCAL_GITHUB_REPO=${LOCAL_GITHUB_REPO:-}
KUBE_CONTEXT=${KUBE_CONTEXT:-orbstack}
# On OrbStack every *.k8s.orb.local name resolves to the cluster, on the
# workstation and in Pods. On kind, *.127.0.0.1.nip.io resolves to 127.0.0.1 on
# the workstation, and CoreDNS answers the names in Pods (kind.sh).
case $KUBE_CONTEXT in
kind-*)
	KIND_CLUSTER=${KUBE_CONTEXT#kind-}
	LOCAL_DOMAIN=${LOCAL_DOMAIN:-127.0.0.1.nip.io}
	;;
*)
	KIND_CLUSTER=
	LOCAL_DOMAIN=${LOCAL_DOMAIN:-k8s.orb.local}
	;;
esac
LOCAL_DIR=${LOCAL_DIR:-$HOME/.openfactory-local}
LOCAL_BUILD=${LOCAL_BUILD:-1}
LOCAL_RETENTION=${LOCAL_RETENTION:-5m}
# One fixed tag for the images of the stack. The Pods use the pull policy Never.
LOCAL_IMAGE_TAG=${LOCAL_IMAGE_TAG:-local}
TESTKIT_IMAGE=of-local/testkit:latest
# The tool image with the claude CLI stand-in (target tool-claude of the testkit Dockerfile).
STANDIN_IMAGE=of-local/tool-claude-standin:latest

NS_AWS=aws-local
NS_INFRA=of-local-infra
NS_SERVER=of-local
NS_SANDBOXES=of-local-sandboxes
RELEASE=openfactory
HOST_PORT_1=18081
HOST_PORT_2=18082

AWS_HOST=aws.$LOCAL_DOMAIN
DB_HOST=db.$LOCAL_DOMAIN
GIT_HOST=git.$LOCAL_DOMAIN
SERVER_HOST=of.$LOCAL_DOMAIN

log() { printf '== %s\n' "$*"; }
note() { printf '   %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# k runs kubectl against the stack's context only.
k() { kubectl --context "$KUBE_CONTEXT" "$@"; }

check_settings() {
	case $LOCAL_SERVER_MODE in chart | host) ;; *) die "LOCAL_SERVER_MODE=$LOCAL_SERVER_MODE: want chart or host" ;; esac
	case $LOCAL_MODEL in fake | claude) ;; *) die "LOCAL_MODEL=$LOCAL_MODEL: want fake or claude" ;; esac
	case $LOCAL_FORGE in none | github) ;; *) die "LOCAL_FORGE=$LOCAL_FORGE: want none or github" ;; esac
	case $LOCAL_BUILD in 0 | 1) ;; *) die "LOCAL_BUILD=$LOCAL_BUILD: want 0 or 1" ;; esac
	if [ -n "$KIND_CLUSTER" ] && [ "$LOCAL_SERVER_MODE" = host ]; then
		die "LOCAL_SERVER_MODE=host needs OrbStack: sandbox Pods call the host servers on host.orb.internal. Use chart mode on kind"
	fi
	if [ "$LOCAL_FORGE" = github ] && [ -z "$LOCAL_GITHUB_REPO" ]; then
		die "LOCAL_FORGE=github needs LOCAL_GITHUB_REPO=<owner>/<repo>"
	fi
}

# server_url prints the URL of the server for the workstation.
server_url() {
	if [ "$LOCAL_SERVER_MODE" = host ]; then
		echo "http://127.0.0.1:$HOST_PORT_1"
	else
		echo "http://$SERVER_HOST"
	fi
}

# image_ref NAME prints the local image reference of an image of scripts/images/build.sh.
image_ref() { (cd "$LOCAL_REPO" && IMAGE_TAG=$LOCAL_IMAGE_TAG sh scripts/images/build.sh list "$1" | awk '{print $2}'); }

# image_id REF prints the content id of a local image, or nothing.
image_id() { docker image inspect --format '{{.Id}}' "$1" 2>/dev/null | sed 's/^sha256://' | cut -c1-12 || true; }

# gen_file FILE COMMAND...  Writes the output of the command to FILE (mode 0600) once.
gen_file() {
	f=$1
	shift
	if [ ! -s "$f" ]; then
		(umask 077 && "$@" > "$f")
		chmod 600 "$f"
	fi
}

# secret_value FILE  prints the content of a file without the trailing newline.
secret_value() { tr -d '\n' < "$1"; }

# env_value NAME  prints the value of NAME in $LOCAL_DIR/env, or nothing.
env_value() {
	[ -f "$LOCAL_DIR/env" ] || return 0
	# shellcheck source=/dev/null
	( . "$LOCAL_DIR/env" >/dev/null 2>&1 && eval "printf '%s' \"\${$1:-}\"" )
}

# render TEMPLATE  Prints the template with __NAME__ replaced by the value of
# the environment variable P_NAME. A name without a value is an error. awk
# reads the values from its environment, so no character needs escaping.
render() {
	awk '
	{
		line = $0; out = ""
		while (match(line, /__[A-Z][A-Z0-9_]*__/)) {
			name = substr(line, RSTART + 2, RLENGTH - 4)
			if (!(("P_" name) in ENVIRON)) { print "error: no value for __" name "__ in the template" > "/dev/stderr"; bad = 1 }
			out = out substr(line, 1, RSTART - 1) ENVIRON["P_" name]
			line = substr(line, RSTART + RLENGTH)
		}
		print out line
	}
	END { exit bad }' "$1"
}

# curl_auth KEY URL [curl args]  curl with the API key kept off the command line.
curl_auth() {
	key=$1
	shift
	printf 'Authorization: Bearer %s\n' "$key" | curl -H @- "$@"
}

# emulator_env_ok  true when the emulator answers with the stored keys as account 000000000000.
emulator_env_ok() {
	[ -f "$LOCAL_DIR/aws/connect.env" ] || return 1
	# shellcheck source=/dev/null
	acct=$( . "$LOCAL_DIR/aws/connect.env" && aws sts get-caller-identity --query Account --output text 2>/dev/null) || return 1
	[ "$acct" = 000000000000 ]
}
