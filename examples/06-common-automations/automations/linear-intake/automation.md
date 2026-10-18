---
agent: foreman
triggers:
  - provider: linear
    event: issue_labeled
    filter:
      teams: [Engineering]
      labels:
        in: [factory-ready]
        not_in: [blocked]
---

Assess the labeled issue and identify the next action. Ask a human about
unresolved requirements. Do not merge changes.
