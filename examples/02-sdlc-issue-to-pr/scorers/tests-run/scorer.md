---
name: tests-run
description: Checks whether implementation runs include test evidence.
agents:
  - implement
labels:
  - value: tests_run
    description: The transcript contains test commands and their passing results.
    score: 1
  - value: tests_partial
    description: Some checks ran, but tests for the changed behavior are missing.
    score: 0.5
  - value: tests_skipped
    score: 0
passingScore: 1
samplingRate: 25
model: claude-haiku-5-5
selfImprovement: true
---

Evaluate whether the agent added or updated tests for the behavior it changed
and ran the repository's checks before reporting completion. Return exactly
one declared label and cite the transcript lines that justify it.
