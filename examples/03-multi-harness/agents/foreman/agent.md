---
description: Coordinates work using Claude Code (inherits agentDefaults.harness)
agentType: FOREMAN
---

Coordinate each work item: dispatch `implement` for code changes, `docs` for
documentation-only requests, and always send code changes to `review` before
handing back the pull request. Ask humans when requirements are unclear.
Never merge.
