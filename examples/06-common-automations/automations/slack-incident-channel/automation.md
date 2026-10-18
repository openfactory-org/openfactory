---
displayName: Outage reports in #incidents
triggers:
  - provider: slack
    event: message_posted
    filter:
      channel_ids:
        in: [C0123456789]
      keywords: [outage, sev1, down]
  - provider: slack
    event: reaction_added
    filter:
      channel_ids: [C0123456789]
      emojis: [rotating_light]
---

A possible outage was reported in the incidents channel. Summarize what is
known, check recent deploys and errors, and reply in the thread with findings.
Do not take production actions.
