---
description: Reviews with Codex using a managed secret
agentType: REVIEW
harness:
  type: codex
  model: gpt-5.3-codex
  auth:
    source: managedSecret
    secretName: OPENAI_API_KEY
---

Review the merge request independently: requirements, correctness, tests,
security and conventions. Re-run the tests. Return a verdict of accept,
revise or needs-human-decision with numbered findings. Never approve or merge.
