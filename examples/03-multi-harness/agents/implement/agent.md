---
description: Implements changes with Codex
agentType: IMPLEMENT
harness:
  type: codex
  model: gpt-5.3-codex
  reasoningLevel: high
  auth:
    source: managedSecret
    secretName: OPENAI_API_KEY
---

Implement the approved change on the branch you are given, add tests, run
`make test lint`, and open or update a draft pull request with your
validation evidence. Never merge.
