#!/bin/sh
# Build every container image that the repository defines, then check each one.
# Run it on a machine with Docker or Podman. CI builds the same images with the
# same Dockerfiles and build arguments (.github/workflows/ci.yml and release.yml).
#
#   sh scripts/images/build.sh [build|smoke|all|list] [image ...]
#
# Commands:
#   build   Build the images and tag them locally.
#   smoke   Run the smoke checks on images that already exist.
#   all     Build, then run the smoke checks. This is the default.
#   list    Print the image names and the tags.
#
# Images (default: all except "release"):
#   server        deploy/docker/Dockerfile       The server, the worker, the egress proxy
#                 and the runner binary (the default runner image).
#   sandbox       deploy/sandbox/Dockerfile      The default task image.
#   tool-claude   images/tool-claude/Dockerfile  The tool image with the claude CLI.
#   tool-codex    images/tool-codex/Dockerfile   The tool image with the codex CLI.
#   tool-example  images/tool-example/Dockerfile The example tool image.
#   release       deploy/docker/Dockerfile.release The GoReleaser runtime image. It needs
#                 Go on this machine: the script builds the binaries first.
#
# Settings (environment variables):
#   CONTAINER_ENGINE   docker or podman. Default: docker if it exists, else podman.
#   IMAGE_TAG          Tag of the local images. Default: VERSION.
#   VERSION            Version in the binaries. Default: git describe, else dev.
#   IMAGE, SANDBOX_IMAGE, TOOL_CLAUDE_IMAGE, TOOL_CODEX_IMAGE,
#   TOOL_EXAMPLE_IMAGE   Image names without a tag. The defaults are the names in the Makefile.
#   NODE_VERSION, CLAUDE_CLI_PACKAGE, CLAUDE_CLI_VERSION, CODEX_CLI_PACKAGE,
#   CODEX_CLI_VERSION    Build arguments of the tool-claude and tool-codex images.
#
# Exit status: 0 when every step passed, 1 otherwise.

set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}
DATE=${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
IMAGE_TAG=${IMAGE_TAG:-$VERSION}

IMAGE=${IMAGE:-ghcr.io/openfactory-org/openfactory}
SANDBOX_IMAGE=${SANDBOX_IMAGE:-ghcr.io/openfactory-org/sandbox}
TOOL_CLAUDE_IMAGE=${TOOL_CLAUDE_IMAGE:-ghcr.io/openfactory-org/tool-claude}
TOOL_CODEX_IMAGE=${TOOL_CODEX_IMAGE:-ghcr.io/openfactory-org/tool-codex}
TOOL_EXAMPLE_IMAGE=${TOOL_EXAMPLE_IMAGE:-example.test/tool-hello}
NODE_VERSION=${NODE_VERSION:-22}
CLAUDE_CLI_PACKAGE=${CLAUDE_CLI_PACKAGE:-}
CLAUDE_CLI_VERSION=${CLAUDE_CLI_VERSION:-}
CODEX_CLI_PACKAGE=${CODEX_CLI_PACKAGE:-}
CODEX_CLI_VERSION=${CODEX_CLI_VERSION:-}

DEFAULT_NAMES="server sandbox tool-claude tool-codex tool-example"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
	sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

# ref NAME prints the full image reference with the local tag.
ref() {
	case $1 in
	server) echo "$IMAGE:$IMAGE_TAG" ;;
	sandbox) echo "$SANDBOX_IMAGE:$IMAGE_TAG" ;;
	tool-claude) echo "$TOOL_CLAUDE_IMAGE:$IMAGE_TAG" ;;
	tool-codex) echo "$TOOL_CODEX_IMAGE:$IMAGE_TAG" ;;
	tool-example) echo "$TOOL_EXAMPLE_IMAGE:$IMAGE_TAG" ;;
	release) echo "$IMAGE:$IMAGE_TAG-release" ;;
	*) die "unknown image: $1 (use: $DEFAULT_NAMES release)" ;;
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
	# The Dockerfiles use cache mounts and BUILDPLATFORM. Both need BuildKit.
	DOCKER_BUILDKIT=1
	export DOCKER_BUILDKIT
}

have_image() { "$ENGINE" image inspect "$1" >/dev/null 2>&1; }

# ---------- build ----------

build_one() {
	name=$1
	r=$(ref "$name")
	log "build $name -> $r"
	case $name in
	server)
		# The binary names this image as its default runner image, and the
		# sandbox image of the same tag as its default task image. The release
		# build does the same (.goreleaser.yaml).
		"$ENGINE" build -f deploy/docker/Dockerfile \
			--build-arg "VERSION=$VERSION" --build-arg "COMMIT=$COMMIT" --build-arg "DATE=$DATE" \
			--build-arg "DEFAULT_SANDBOX_IMAGE=$(ref sandbox)" --build-arg "DEFAULT_RUNNER_IMAGE=$r" \
			-t "$r" .
		;;
	sandbox)
		"$ENGINE" build -f deploy/sandbox/Dockerfile -t "$r" deploy/sandbox
		;;
	tool-claude)
		# An empty package or version keeps the default of the Dockerfile.
		set -- --build-arg "NODE_VERSION=$NODE_VERSION"
		[ -z "$CLAUDE_CLI_PACKAGE" ] || set -- "$@" --build-arg "CLAUDE_CLI_PACKAGE=$CLAUDE_CLI_PACKAGE"
		[ -z "$CLAUDE_CLI_VERSION" ] || set -- "$@" --build-arg "CLAUDE_CLI_VERSION=$CLAUDE_CLI_VERSION"
		"$ENGINE" build -f images/tool-claude/Dockerfile "$@" -t "$r" images/tool-claude
		;;
	tool-codex)
		# An empty package or version keeps the default of the Dockerfile.
		set -- --build-arg "NODE_VERSION=$NODE_VERSION"
		[ -z "$CODEX_CLI_PACKAGE" ] || set -- "$@" --build-arg "CODEX_CLI_PACKAGE=$CODEX_CLI_PACKAGE"
		[ -z "$CODEX_CLI_VERSION" ] || set -- "$@" --build-arg "CODEX_CLI_VERSION=$CODEX_CLI_VERSION"
		"$ENGINE" build -f images/tool-codex/Dockerfile "$@" -t "$r" images/tool-codex
		;;
	tool-example)
		"$ENGINE" build -f images/tool-example/Dockerfile -t "$r" images/tool-example
		;;
	release)
		build_release "$r"
		;;
	esac
}

# GoReleaser makes the binaries for deploy/docker/Dockerfile.release. This
# function makes the same files with `go build`, with the flags of .goreleaser.yaml.
build_release() {
	command -v go >/dev/null 2>&1 || die "the release image needs Go on this machine"
	ctx=$(mktemp -d)
	trap 'rm -rf "$ctx"' EXIT INT TERM
	case $ENGINE in
	*podman*) fmt='{{.Host.Arch}}' ;;
	*) fmt='{{.Architecture}}' ;;
	esac
	arch=$("$ENGINE" info --format "$fmt" 2>/dev/null || true)
	case $arch in
	arm64 | aarch64) arch=arm64 ;;
	amd64 | x86_64) arch=amd64 ;;
	*) arch=$(go env GOARCH) ;;
	esac
	log "build the Linux $arch binaries for the release image"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
		-o "$ctx/openfactory" ./cmd/openfactory
	for p in gitlab azuredevops; do
		CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" \
			-o "$ctx/openfactory-plugin-$p" "./plugins/$p"
	done
	"$ENGINE" build -f "$ROOT/deploy/docker/Dockerfile.release" -t "$1" "$ctx"
	rm -rf "$ctx"
	trap - EXIT INT TERM
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

# check DESCRIPTION PATTERN COMMAND...  Run the command. It passes when the
# command succeeds and its output matches the grep pattern (use . for any).
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

# in_image IMAGE ENTRYPOINT ARG...  Run a command in a new container.
in_image() {
	img=$1
	ep=$2
	shift 2
	"$ENGINE" run --rm --entrypoint "$ep" "$img" "$@"
}

# sh_in IMAGE SCRIPT  Run a shell script in a new container.
sh_in() { in_image "$1" sh -c "$2"; }

smoke_server() {
	r=$1
	check "openfactory version prints the version" '^openfactory ' \
		"$ENGINE" run --rm "$r" version
	check "the version is the build version ($VERSION)" "$VERSION" \
		"$ENGINE" run --rm "$r" version
	check "the process runs as user 10001" '^10001$' sh_in "$r" 'id -u'
	check "git and ssh are on the PATH (definition sync)" '/git' sh_in "$r" 'command -v git && command -v ssh'
	check "iptables is present (egress lock of the proxy sidecar)" 'iptables v' sh_in "$r" 'iptables --version'
	# shellcheck disable=SC2016 # $OF_PLUGIN_DIR is set in the container; it must expand later, not here
	check "the bundled plugins are in OF_PLUGIN_DIR" 'openfactory-plugin-gitlab' \
		sh_in "$r" 'ls "$OF_PLUGIN_DIR"; test -x "$OF_PLUGIN_DIR/openfactory-plugin-azuredevops"'
	check "the netguard proxy command exists" 'forward proxy' "$ENGINE" run --rm "$r" netguard --help
	smoke_runner "$r"
	# The proxy must answer 403 for a host that the policy does not allow.
	check "netguard blocks a host that is not allowed" '403' sh_in "$r" \
		'openfactory netguard --listen 127.0.0.1:3128 --allow allowed.example >/dev/null 2>&1 &
		 sleep 2
		 http_proxy=http://127.0.0.1:3128 wget -S -O /dev/null http://denied.example/ 2>&1 || true'
}

# smoke_runner IMAGE  The server image is the runner image. The kubernetes
# backend copies the binary into a task with `cp` as a user that is not root
# (1000 by default), into an empty volume. The binary must then run in a glibc
# task image as yet another user.
smoke_runner() {
	r=$1
	check "the runner command exists" 'Run the runner of one session' "$ENGINE" run --rm "$r" runner --help
	check "the binary is static (it can be copied into any task image)" 'not a dynamic|statically linked|Not a valid dynamic' \
		sh_in "$r" 'ldd /usr/local/bin/openfactory 2>&1 || true'
	check "the runner install copies the binary as user 1000" 'ok' \
		"$ENGINE" run --rm --user 1000:1000 --entrypoint sh "$r" -c \
		'mkdir -p /tmp/runner && cp /usr/local/bin/openfactory /tmp/runner/openfactory && test -x /tmp/runner/openfactory && echo ok'
	vol=of-smoke-runner-$$
	if "$ENGINE" volume create "$vol" >/dev/null 2>&1; then
		# A fresh volume is owned by root. The backend uses emptyDir volumes,
		# which every user can write; chmod 1777 makes the same mode here.
		"$ENGINE" run --rm -v "$vol:/opt/openfactory" --entrypoint chmod debian:bookworm-slim 1777 /opt/openfactory >/dev/null 2>&1 || true
		"$ENGINE" run --rm --user 1000:1000 -v "$vol:/opt/openfactory" --entrypoint cp "$r" \
			/usr/local/bin/openfactory /opt/openfactory/openfactory >/dev/null 2>&1 || true
		check "the copied binary runs in a glibc image as another user" '^openfactory ' \
			"$ENGINE" run --rm --user 1001:1001 -v "$vol:/opt/openfactory" --entrypoint /opt/openfactory/openfactory \
			debian:bookworm-slim version
		"$ENGINE" volume rm "$vol" >/dev/null 2>&1 || true
	else
		fail "cannot create a volume for the runner install check"
	fi
}

# smoke_tool IMAGE CLI  A tool image with a harness CLI.
smoke_tool() {
	r=$1
	cli=$2
	check "sh and cp are present (tool image contract)" '/cp' sh_in "$r" 'command -v sh && command -v cp'
	check "$cli is on the PATH at /opt/openfactory/tool/bin" "^/opt/openfactory/tool/bin/$cli\$" \
		sh_in "$r" "command -v $cli"
	check "$cli runs ($cli --version)" '[0-9]' in_image "$r" "$cli" --version
	check "node runs" '^v[0-9]+' in_image "$r" node --version
	# The backend copies as a user that is not root, into a volume that root owns.
	# So it must not use cp -a: that sets owner, mode and times of the target.
	check "the tool image contract: cp -RP copies /opt/openfactory/tool as user 1000" 'ok' \
		"$ENGINE" run --rm --user 1000:1000 --entrypoint sh "$r" -c \
		'mkdir -p /tmp/dest && cp -RP /opt/openfactory/tool/. /tmp/dest/ && test -x /tmp/dest/bin/'"$cli"' && echo ok'
}

smoke_sandbox() {
	r=$1
	check "sh, git and coreutils are present (backend rules)" '/git' \
		sh_in "$r" 'command -v sh && command -v cp && command -v git'
	check "the image is glibc-based" 'GLIBC|GNU libc' sh_in "$r" 'ldd --version 2>&1 | head -n 1'
	check "no harness CLI is in the image (the tool images deliver them)" '^none$' \
		sh_in "$r" 'command -v claude || command -v codex || echo none'
	check "node, git and gh run" 'git version' sh_in "$r" 'node --version >/dev/null && gh --version >/dev/null && git --version'
	check "the user agent exists" '^agent$' sh_in "$r" 'id -un agent'
	check "/workspace exists" 'agent' sh_in "$r" 'stat -c %U /workspace'
}

smoke_tool_example() {
	r=$1
	check "sh and cp are present (tool image contract)" '/cp' sh_in "$r" 'command -v sh && command -v cp'
	check "the tool script is in /opt/openfactory/tool/bin" 'hello from' in_image "$r" /opt/openfactory/tool/bin/hello
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
	server | release) smoke_server "$r" ;;
	sandbox) smoke_sandbox "$r" ;;
	tool-claude) smoke_tool "$r" claude ;;
	tool-codex) smoke_tool "$r" codex ;;
	tool-example) smoke_tool_example "$r" ;;
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
		printf '%-14s %s\n' "$n" "$(ref "$n")"
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
