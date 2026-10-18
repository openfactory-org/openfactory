---
triggers:
  - provider: linear
    event: agent_session_created
    filter:
      team_ids: ["2f1c1a5e-8f0a-4c55-9e1c-3d2b7a9e4f10"]
      keywords: [build, fix, investigate]
---

Someone started an agent session on a Linear issue. Work on the issue and
report progress in the session.
