---
displayName: Human review feedback
triggers:
  - provider: github
    event: pull_request_review_submitted
    filter:
      repos: [acme/web-app, acme/ios-app]
      review_states: [changes_requested]
      skip_bots: all
---

A human requested changes on a pull request. If it belongs to an existing work
item, continue that work item: send the feedback to the implement agent's run,
re-run review, and reply on the pull request. Otherwise, summarize the
feedback and ask the author whether the factory should take it on.
