---
triggers:
  - provider: slack
    event: app_mention
    filter:
      channels: ["#engineering", "#product"]
---

Someone mentioned the factory in Slack. Treat the message and its thread as
the request. Reply in the thread to confirm what you will do, ask clarifying
questions there, and post the handoff in the same thread.
