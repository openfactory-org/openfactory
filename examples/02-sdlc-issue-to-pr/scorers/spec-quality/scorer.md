---
name: spec-quality
description: Checks that specs have testable acceptance and validation criteria.
output: classification
agents:
  - name: foreman
    includeDescendants: true
labels:
  - value: complete
    description: Product and technical specs exist and every acceptance criterion is verifiable.
    score: 1
  - value: vague
    description: A spec exists but criteria are missing or not verifiable.
    score: 0.3
  - value: missing
    score: 0
passingScore: 0.8
samplingRate: 12.345
model: claude-sonnet-5-5
---

Look at the spec agent's child run in this work item. Decide whether its spec
contains a product spec, a technical spec and verifiable validation criteria.
Return exactly one declared label.
