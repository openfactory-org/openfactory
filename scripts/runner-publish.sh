#!/bin/sh
# Builds openfactory-runtime for linux/amd64 and linux/arm64, writes the
# checksums, and uploads them to R2 with wrangler. The control plane serves
# them on the runner routes, and bootstrap.sh checks OF_RUNNER_SHA256.
#
#   sh scripts/runner-publish.sh [build|upload|all]
#
# Commands:
#   build    Build the binaries and the checksums into OUT_DIR. No upload.
#   upload   Upload OUT_DIR to R2 (needs a wrangler login or CLOUDFLARE_API_TOKEN).
#   all      build, then upload. This is the default.
#
# Settings (environment variables):
#   VERSION      Version in the binary and the R2 path. Default: git describe, else dev.
#   OUT_DIR      Default: dist/runner/VERSION.
#   R2_BUCKET    Default: openfactory-data (the control-plane DATA bucket).
#   R2_PREFIX    Default: runner. Objects: R2_PREFIX/VERSION/<file>.
#   RUNTIME_PKG  Go main package. Default: ./cmd/openfactory-runtime if it exists, else ./cmd/openfactory.
#
# Files per arch: openfactory-runtime-linux-ARCH and openfactory-runtime-linux-ARCH.sha256
# ("<sha256>  VERSION"); one checksums.txt for all (sha256sum format).
#
# Exit status: 0 on success, 1 otherwise.

set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
OUT_DIR=${OUT_DIR:-dist/runner/$VERSION}
R2_BUCKET=${R2_BUCKET:-openfactory-data}
R2_PREFIX=${R2_PREFIX:-runner}
if [ -z "${RUNTIME_PKG:-}" ]; then
	if [ -d cmd/openfactory-runtime ]; then RUNTIME_PKG=./cmd/openfactory-runtime; else RUNTIME_PKG=./cmd/openfactory; fi
fi
ARCHES="amd64 arm64"

log() { printf '==> %s\n' "$*"; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

build() {
	command -v go >/dev/null 2>&1 || die "go is not on the PATH"
	mkdir -p "$OUT_DIR"
	: >"$OUT_DIR/checksums.txt"
	for a in $ARCHES; do
		f=openfactory-runtime-linux-$a
		log "build $f ($VERSION)"
		CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath \
			-ldflags "-s -w -X main.version=$VERSION" -o "$OUT_DIR/$f" "$RUNTIME_PKG"
		s=$(sha256 "$OUT_DIR/$f")
		printf '%s  %s\n' "$s" "$VERSION" >"$OUT_DIR/$f.sha256"
		printf '%s  %s\n' "$s" "$f" >>"$OUT_DIR/checksums.txt"
	done
	log "checksums: $OUT_DIR/checksums.txt"
	cat "$OUT_DIR/checksums.txt"
}

upload() {
	[ -f "$OUT_DIR/checksums.txt" ] || die "no build in $OUT_DIR. Run the build first"
	for a in $ARCHES; do
		f=openfactory-runtime-linux-$a
		want=$(grep "  $f\$" "$OUT_DIR/checksums.txt" | cut -d' ' -f1)
		[ "$(sha256 "$OUT_DIR/$f")" = "$want" ] || die "$f does not match checksums.txt"
	done
	for f in $(cd "$OUT_DIR" && ls); do
		case $f in
		*.sha256 | *.txt) type=text/plain ;;
		*) type=application/octet-stream ;;
		esac
		log "upload r2://$R2_BUCKET/$R2_PREFIX/$VERSION/$f"
		npx wrangler r2 object put "$R2_BUCKET/$R2_PREFIX/$VERSION/$f" \
			--file "$OUT_DIR/$f" --content-type "$type" --remote
	done
	log "set OF_RUNNER_VERSION=$VERSION on the control plane and the cloudflare-sandbox provider"
}

case ${1:-all} in
build) build ;;
upload) upload ;;
all)
	build
	upload
	;;
-h | --help | help) sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//' ;;
*) die "unknown command: $1 (use build, upload or all)" ;;
esac
