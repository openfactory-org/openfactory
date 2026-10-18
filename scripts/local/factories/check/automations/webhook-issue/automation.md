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

The signed webhook of check W3 arrived. [[reply: WEBHOOK-READY]]
