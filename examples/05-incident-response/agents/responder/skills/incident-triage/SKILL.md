---
name: incident-triage
description: Step-by-step runbook for investigating a payments-service incident. Use at the start of every incident investigation.
---

# Incident triage runbook

1. **Scope.** From the alert, record the service, environment, start time,
   severity and affected endpoints.
2. **Errors.** In Sentry, find the top new or regressed issues since the start
   time. Note stack traces, release versions and event counts.
3. **Metrics.** In Datadog, compare error rate, latency and saturation for the
   affected endpoints with the previous day.
4. **Changes.** List deploys and merged pull requests in the two hours before
   the start time (`git log --since`). Correlate with the first errors.
5. **Hypothesis.** State the most likely cause with your confidence and the
   evidence for and against it.
6. **Mitigation.** Recommend the least risky mitigation (feature flag, revert,
   config change, or code fix). A code fix needs a regression test.
7. **Report.** Summarize timeline, impact, cause, mitigation and follow-ups.
