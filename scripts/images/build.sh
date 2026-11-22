#!/bin/sh
# Builds the sandbox images, then checks each one. Run it on a machine with
# Docker or Podman.
#
#   sh scripts/images/build.sh [build|smoke|all|list] [image ...]
#
# Commands:
#   build   Build the images and tag them locally.
#   smoke   Run the smoke checks on images that already exist.
#   all     Build, then run the smoke checks. This is the default.
#   list    Print the image names and the tags.
#
# Images (default: both):
#   sandbox             deploy/sandbox/Dockerfile             The sandbox image (Daytona snapshot, generic).
#   sandbox-cloudflare  deploy/sandbox/Dockerfile.cloudflare  The cloudflare-sandbox image (linux/amd64).
#
# Settings (environment variables):
#   CONTAINER_ENGINE     docker or podman. Default: docker if it exists, else podman.
#   IMAGE_TAG            Tag of the local images. Default: VERSION.
#   VERSION              Default: git describe, else dev.
#   SANDBOX_IMAGE, SANDBOX_CLOUDFLARE_IMAGE   Image names without a tag.
#   PLATFORM             Build platform of the sandbox image. Default: linux/amd64.
#   RUNNER_DIR           A directory with openfactory-runtime-linux-<arch> to bake in
#                        (scripts/runner-publish.sh build). Default: dist/runner/VERSION if it exists.
#   CLAUDE_CLI_VERSION, CODEX_CLI_VERSION   Default: latest.
#
# Exit status: 0 when every step passed, 1 otherwise.

set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
IMAGE_TAG=${IMAGE_TAG:-$VERSION}
SANDBOX_IMAGE=${SANDBOX_IMAGE:-ghcr.io/openfactory-org/sandbox}
SANDBOX_CLOUDFLARE_IMAGE=${SANDBOX_CLOUDFLARE_IMAGE:-ghcr.io/openfactory-org/sandbox-cloudflare}
PLATFORM=${PLATFORM:-linux/amd64}
RUNNER_DIR=${RUNNER_DIR:-dist/runner/$VERSION}
CLAUDE_CLI_VERSION=${CLAUDE_CLI_VERSION:-latest}
CODEX_CLI_VERSION=${CODEX_CLI_VERSION:-latest}

DEFAULT_NAMES="sandbox sandbox-cloudflare"
AGENT_UID=1001

log() { printf '==> %s\n' "$*"; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

usage() {
	sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

# ref NAME prints the full image reference with the local tag.
ref() {
	case $1 in
	sandbox) echo "$SANDBOX_IMAGE:$IMAGE_TAG" ;;
	sandbox-cloudflare) echo "$SANDBOX_CLOUDFLARE_IMAGE:$IMAGE_TAG" ;;
	*) die "unknown image: $1 (use: $DEFAULT_NAMES)" ;;
	esac
}

detect_engine() {
	if [ -n "${CONTAINER_ENGINE:-}" ]; then
		ENGINE=$CONTAINER_ENGINE
	elif command -v docker >/dev/null 2>&1; then
		ENGINE=docker
	elif command -v podman >/dev/null 2>&1; then
		ENGINE=podman
	else
		die "neither docker nor podman is on the PATH"
	fi
	command -v "$ENGINE" >/dev/null 2>&1 || die "$ENGINE is not on the PATH"
	"$ENGINE" info >/dev/null 2>&1 || die "$ENGINE does not answer. Start the daemon (or the podman machine) and try again"
	# The Dockerfile uses a bind mount in RUN. It needs BuildKit.
	DOCKER_BUILDKIT=1
	export DOCKER_BUILDKIT
}

have_image() { "$ENGINE" image inspect "$1" >/dev/null 2>&1; }

# ---------- build ----------

STAGED=
unstage() {
	if [ -n "$STAGED" ]; then rm -rf deploy/sandbox/runner; fi
}

# stage_runner copies the baked runner into the build context.
stage_runner() {
	[ -d "$RUNNER_DIR" ] || {
		log "no runner in $RUNNER_DIR: the image downloads the runner at start"
		return 0
	}
	[ ! -e deploy/sandbox/runner ] || die "deploy/sandbox/runner exists. Remove it first"
	mkdir deploy/sandbox/runner
	STAGED=1
	trap unstage EXIT INT TERM
	for f in "$RUNNER_DIR"/openfactory-runtime-linux-*; do
		case $f in *.sha256) continue ;; esac
		[ -f "$f" ] && cp "$f" deploy/sandbox/runner/
	done
}

build_one() {
	name=$1
	r=$(ref "$name")
	log "build $name -> $r"
	case $name in
	sandbox)
		stage_runner
		"$ENGINE" build --platform "$PLATFORM" -f deploy/sandbox/Dockerfile \
			--build-arg "CLAUDE_CLI_VERSION=$CLAUDE_CLI_VERSION" --build-arg "CODEX_CLI_VERSION=$CODEX_CLI_VERSION" \
			-t "$r" deploy/sandbox
		unstage
		STAGED=
		;;
	sandbox-cloudflare)
		have_image "$(ref sandbox)" || build_one sandbox
		"$ENGINE" build --platform linux/amd64 -f deploy/sandbox/Dockerfile.cloudflare \
			--build-arg "SANDBOX_IMAGE=$(ref sandbox)" -t "$r" deploy/sandbox
		;;
	esac
}

# ---------- smoke checks ----------

FAILS=0
PASSES=0

pass() {
	PASSES=$((PASSES + 1))
	printf '  PASS  %s\n' "$1"
}

fail() {
	FAILS=$((FAILS + 1))
	printf '  FAIL  %s\n' "$1"
	if [ -n "${2:-}" ]; then
		printf '%s\n' "$2" | sed 's/^/        /'
	fi
}

# check DESCRIPTION PATTERN COMMAND...  Passes when the command succeeds and
# its output matches the grep pattern (use . for any).
check() {
	desc=$1
	pattern=$2
	shift 2
	if out=$("$@" 2>&1); then
		if printf '%s\n' "$out" | grep -Eq "$pattern"; then
			pass "$desc"
		else
			fail "$desc (the output does not match: $pattern)" "$out"
		fi
	else
		fail "$desc (the command failed)" "$out"
	fi
}

# sh_in IMAGE SCRIPT  Run a shell script in a new container, as root.
sh_in() { "$ENGINE" run --rm --platform linux/amd64 --entrypoint sh "$1" -c "$2"; }
# sh_as_agent IMAGE SCRIPT  The same, as the agent user.
sh_as_agent() { "$ENGINE" run --rm --platform linux/amd64 --user "$AGENT_UID:$AGENT_UID" --entrypoint bash "$1" -lc "$2"; }

# shellcheck disable=SC2016 # the scripts expand inside the container
smoke_sandbox() {
	r=$1
	check "the image is glibc-based" 'GLIBC|GNU libc' sh_in "$r" 'ldd --version 2>&1 | head -n 1'
	check "the user agent has uid $AGENT_UID" "^$AGENT_UID\$" sh_in "$r" 'id -u agent'
	check "the user agent is only in its own group" '^agent$' sh_in "$r" 'id -Gn agent'
	check "the user ofproxy exists" '^[0-9]+$' sh_in "$r" 'id -u ofproxy'
	check "sudo is not installed" '^none$' sh_in "$r" 'command -v sudo || echo none'
	check "no sudoers entry names agent" '^none$' sh_in "$r" \
		'grep -rlsE "^[[:space:]]*agent[[:space:]]" /etc/sudoers /etc/sudoers.d || echo none'
	check "no setuid or setgid binaries" '^none$' sh_in "$r" \
		'f=$(find / -xdev \( -perm -4000 -o -perm -2000 \) -type f 2>/dev/null); [ -z "$f" ] && echo none || echo "$f"'
	check "iptables, ip6tables and ip run" 'ip6tables' sh_in "$r" 'iptables --version && ip6tables --version && ip -V'
	check "git, node and gh run" 'git version' sh_in "$r" 'node --version >/dev/null && gh --version >/dev/null && git --version'
	check "claude runs as agent" '[0-9]' sh_as_agent "$r" 'claude --version'
	check "codex runs as agent" '[0-9]' sh_as_agent "$r" 'codex --version'
	check "/workspace belongs to agent" '^agent$' sh_in "$r" 'stat -c %U /workspace'
	check "bootstrap.sh is root-owned, mode 755" '^root:755$' sh_in "$r" 'stat -c %U:%a /opt/openfactory/bootstrap.sh'
	check "agent cannot write /opt/openfactory" '^denied$' sh_as_agent "$r" 'touch /opt/openfactory/x 2>/dev/null && echo written || echo denied'
	check "the entrypoint is tini + bootstrap.sh" 'tini.*bootstrap\.sh' \
		"$ENGINE" image inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' "$r"
	check "the bootstrap refuses a bad runner checksum (exit 65)" '^65$' sh_in "$r" \
		'OF_CONTROL_URL=http://x.invalid OF_RUN_ID=r OF_SESSION=1 OF_BOOTSTRAP_TOKEN=t OF_RUNNER_SHA256=bad \
		 /opt/openfactory/bootstrap.sh >/dev/null 2>&1; echo $?'
}

smoke_cloudflare() {
	r=$1
	smoke_sandbox "$r"
	check "the image is linux/amd64" '^linux/amd64$' "$ENGINE" image inspect --format '{{.Os}}/{{.Architecture}}' "$r"
	check "trust-ca.sh is present" '^root:755$' sh_in "$r" 'stat -c %U:%a /opt/openfactory/trust-ca.sh'
}

smoke_one() {
	name=$1
	r=$(ref "$name")
	log "smoke $name ($r)"
	if ! have_image "$r"; then
		fail "the image $r does not exist. Run the build first"
		return
	fi
	case $name in
	sandbox) smoke_sandbox "$r" ;;
	sandbox-cloudflare) smoke_cloudflare "$r" ;;
	esac
}

# ---------- main ----------

cmd=all
case ${1:-} in
build | smoke | all | list)
	cmd=$1
	shift
	;;
-h | --help | help)
	usage
	exit 0
	;;
esac

names=${*:-$DEFAULT_NAMES}
for n in $names; do
	ref "$n" >/dev/null
done

if [ "$cmd" = list ]; then
	for n in $names; do
		printf '%-20s %s\n' "$n" "$(ref "$n")"
	done
	exit 0
fi

detect_engine
log "engine: $ENGINE, tag: $IMAGE_TAG, version: $VERSION"

if [ "$cmd" = build ] || [ "$cmd" = all ]; then
	for n in $names; do
		build_one "$n"
	done
fi

if [ "$cmd" = smoke ] || [ "$cmd" = all ]; then
	for n in $names; do
		smoke_one "$n"
	done
	printf '\n%s checks passed, %s failed\n' "$PASSES" "$FAILS"
	if [ "$FAILS" -gt 0 ]; then
		echo "RESULT: FAIL"
		exit 1
	fi
	echo "RESULT: PASS"
fi
