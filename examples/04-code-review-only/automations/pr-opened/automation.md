---
enabled: true
agent: reviewer
triggers:
  - provider: github
    event: pull_request_opened
    filter:
      repos: [acme/api-service, acme/web-app]
      base_branches: [main]
      labels:
        not_in: [wip, no-review]
  - provider: github
    event: pull_request_ready
    filter:
      repos: [acme/api-service, acme/web-app]
      baseBranches: [main]
---

A pull request was opened or marked ready for review against `main`. If it is
a draft, stop silently. Otherwise review it and post your findings and
verdict on the pull request.
