#!/usr/bin/env bash
set -euo pipefail
echo "==> lint";  pnpm lint
echo "==> types"; pnpm exec tsc --noEmit
echo "==> tests"; pnpm test -- --reporter=dot
echo "==> build"; pnpm build
echo "All checks passed."
