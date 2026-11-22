#!/usr/bin/env bash
# Capture the definition goldens from the Go reference (tag go-final).
#
# The script runs the Go tests of internal/definition with `go test -overlay`:
# Compile, Diff and ValidateSchema are renamed and wrapped by rec.go.txt, which
# records every call. golden_test.go.txt adds the examples and other vectors
# and writes them to packages/definition/test/golden/.
#
# Run from the repository root:
#   bash packages/definition/test/golden/capture/capture.sh
set -euo pipefail

root=$(git rev-parse --show-toplevel)
here="$root/packages/definition/test/golden/capture"
out="$root/packages/definition/test/golden"
ref=${GOLDEN_REF:-go-final}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

pkg="$root/internal/definition"
git -C "$root" show "$ref:internal/definition/compile.go" | sed 's/^func Compile(/func compileOrig(/' >"$work/compile.go"
git -C "$root" show "$ref:internal/definition/diff.go" | sed 's/^func Diff(/func diffOrig(/' >"$work/diff.go"
git -C "$root" show "$ref:internal/definition/schema.go" | sed 's/^func ValidateSchema(/func validateSchemaOrig(/' >"$work/schema.go"
cp "$here/rec.go.txt" "$work/zz_golden_rec.go"
cp "$here/golden_test.go.txt" "$work/zz_golden_test.go"

cat >"$work/overlay.json" <<EOF
{"Replace": {
  "$pkg/compile.go": "$work/compile.go",
  "$pkg/diff.go": "$work/diff.go",
  "$pkg/schema.go": "$work/schema.go",
  "$pkg/zz_golden_rec.go": "$work/zz_golden_rec.go",
  "$pkg/zz_golden_test.go": "$work/zz_golden_test.go"
}}
EOF

mkdir -p "$out"
cd "$root"
OF_GOLDEN_OUT="$out" go test -count=1 -overlay "$work/overlay.json" ./internal/definition/
echo "goldens written to $out"
