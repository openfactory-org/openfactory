---
displayName: Main branch CI failure
triggers:
  - provider: github
    event: workflow_run_completed
    filter:
      repos: [acme/api, acme/web]
      branches: [main]
      workflows: [CI]
      conclusions: [failure]
  - provider: github
    event: commit_status
    filter:
      repos: [acme/api]
      states: [failure, error]
      contexts: [ci/build]
---

CI failed on `main`. Find the failing job and the commit that broke it. If
the cause is clear and the fix is small, open a draft pull request with the
fix; otherwise post a diagnosis on the commit and ask the author.
