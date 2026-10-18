---
triggers:
  - provider: webhook
    event: received
    filter:
      webhook_ids: [sentry-alerts]
      payload:
        action: [created]
        data:
          issue:
            level: [fatal]
            project:
              slug:
                not_in: [sandbox, staging]
---

A new fatal issue was created in Sentry. Read the attached event payload,
find the failing code path, and open a draft pull request with a fix and a
regression test. Hand it back for human review.
