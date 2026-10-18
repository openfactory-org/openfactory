---
name: run-checks
description: Runs the full local validation suite for the web app and summarizes failures. Use before reporting an implementation as done.
---

# Run checks

Run `bash scripts/run-checks.sh` from this skill's directory with the
repository root as the working directory. The script runs lint, type checks,
unit tests and the production build, stopping at the first failure.

Report each step's result. For a failure, include the failing command, the
first relevant error lines and your diagnosis. Do not mark work complete while
a check fails because of your change.
