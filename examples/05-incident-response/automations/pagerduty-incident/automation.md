---
displayName: PagerDuty incident triggered
triggers:
  - provider: webhook
    event: received
    filter:
      webhook_ids: [pagerduty-incidents]
      payload:
        event:
          event_type: [incident.triggered]
          data:
            service:
              summary: [payments-service]
            priority:
              exists: true
idleTimeoutMinutes: 60
---

A PagerDuty incident was triggered for the payments service. Investigate it,
keep the incident channel updated, and propose a mitigation. Do not take any
production action without a human's approval.
