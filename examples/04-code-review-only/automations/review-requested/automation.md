---
agent: foreman
triggers:
  - provider: github
    event: pull_request_mentioned
    filter:
      repos: [acme/api-service, acme/web-app]
      mentioned: [reviewbot]
      keywords: [review, re-review]
---

Someone mentioned the factory on a pull request asking for a review. Dispatch
the reviewer with their request and reply in the same thread with the result.
