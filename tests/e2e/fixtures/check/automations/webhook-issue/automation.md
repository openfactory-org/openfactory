---
displayName: Webhook intake check
agent: foreman
triggers:
  - provider: github
    event: issue_labeled
    filter:
      repos: [of-local-checks/webhook-demo]
      skip_bots: none
---

The signed webhook of the webhook check arrived. [[reply: WEBHOOK-READY]]
