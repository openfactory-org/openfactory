#!/bin/sh
# Report Go API changes in the public extension packages under pkg/ between a
# base git ref and the working tree. pkg/ is experimental in v1,
# so this script only reports: it exits 0 even when it finds incompatible
# changes. Set APIDIFF_STRICT=1 to exit 1 on incompatible changes once the Go
# API is declared stable.
#
#   sh scripts/go-apidiff.sh [base-ref]     # default base: origin/main
#
# Uses golang.org/x/exp/cmd/apidiff (pinned below) through `go run`.

set -eu

BASE=${1:-origin/main}
APIDIFF=${APIDIFF:-golang.org/x/exp/cmd/apidiff@v0.0.0-20261014143641-f0251eb4aa3e}
STRICT=${APIDIFF_STRICT:-0}

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"
MODULE=$(go list -m)

if ! git rev-parse --verify --quiet "$BASE^{commit}" >/dev/null; then
	echo "go-apidiff: base ref $BASE not found; nothing to compare (fetch it first)"
	exit 0
fi

TMP=$(mktemp -d)
# shellcheck disable=SC2329 # invoked by the trap below
cleanup() {
	git worktree remove --force "$TMP/base" >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

git worktree add --quiet --detach "$TMP/base" "$BASE"

# Packages under pkg/ (import paths), at base and at head.
list_pkgs() {
	if [ -d pkg ]; then
		go list ./pkg/... 2>/dev/null || true
	fi
}
(cd "$TMP/base" && list_pkgs) | sort >"$TMP/base.pkgs"
list_pkgs | sort >"$TMP/head.pkgs"

incompatible=0
echo "Go API diff of $MODULE/pkg/... against $BASE (report only: pkg/ is experimental until v1.x)"

for pkg in $(comm -23 "$TMP/base.pkgs" "$TMP/head.pkgs"); do
	echo
	echo "## $pkg"
	echo "Incompatible: package removed"
	incompatible=1
done
for pkg in $(comm -13 "$TMP/base.pkgs" "$TMP/head.pkgs"); do
	echo
	echo "## $pkg"
	echo "Compatible: new package"
done
for pkg in $(comm -12 "$TMP/base.pkgs" "$TMP/head.pkgs"); do
	name=$(echo "$pkg" | tr '/.' '__')
	if ! (cd "$TMP/base" && go run "$APIDIFF" -w "$TMP/$name.export" "$pkg"); then
		echo "go-apidiff: could not export $pkg at $BASE; skipping"
		continue
	fi
	out=$(go run "$APIDIFF" -incompatible=false "$TMP/$name.export" "$pkg" 2>&1) || {
		echo "go-apidiff: apidiff failed for $pkg: $out"
		continue
	}
	if [ -n "$out" ]; then
		echo
		echo "## $pkg"
		echo "$out"
		case $out in
		*Incompatible*) incompatible=1 ;;
		esac
	fi
done

echo
if [ "$incompatible" -eq 1 ]; then
	echo "go-apidiff: incompatible changes found (reported only)"
	[ "$STRICT" = "1" ] && exit 1
else
	echo "go-apidiff: no incompatible changes"
fi
exit 0
