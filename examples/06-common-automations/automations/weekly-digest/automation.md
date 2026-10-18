---
agent: reporter
displayName: Weekly factory digest
triggers:
  - provider: schedule
    event: cron_fired
    schedule:
      name: monday-morning
      cron: "0 9 * * 1"
---

Write the weekly factory digest: work items completed, pull requests opened
and merged, items waiting on humans, and failed runs, for the past seven days.
Post it to the #engineering Slack channel.
