---
displayName: Nightly dependency audit
triggers:
  - provider: schedule
    event: cron_fired
    schedule:
      cron: "@daily"
  - provider: schedule
    event: cron_fired
    schedule:
      name: midday-recheck
      cron: "@every 12h"
idleTimeoutMinutes: 5
---

Audit dependencies in every repository for known vulnerabilities. For each
fixable high or critical advisory, open a draft pull request with the upgrade
and passing tests. Summarize the results in #engineering.
