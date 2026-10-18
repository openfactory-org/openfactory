---
triggers:
  - provider: gitlab
    event: bot_mentioned
    filter:
      repos: [acme/platform/billing]
---

The factory's bot was mentioned on a GitLab issue or merge request. Treat the
mention as the request, work it through to a draft merge request, and reply in
the same thread.
