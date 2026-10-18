---
agent: foreman
triggers:
  - provider: linear
    event: comment_created
    filter:
      teams: [Engineering]
      labels: [factory-ready]
---

Handle the new comment in the issue's context. Do not assume a previous run
exists. Do not merge changes.
