---
triggers:
  - provider: github
    event: pull_request_review_requested
    filter:
      repos: [acme/api, acme/web]
      reviewers: [builder-bot]
      skip_bots: none
---

The factory was requested as a reviewer. Review the pull request and post a
comment with findings and a verdict. Do not approve or merge.
