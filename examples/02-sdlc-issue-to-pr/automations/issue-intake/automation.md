---
agent: foreman
triggers:
  - provider: github
    event: issue_labeled
    filter:
      repos: [acme/web-app, acme/api-service]
      labels: [factory-ready]
---

An issue was labeled `factory-ready`. Take it from intake to a reviewed pull
request. For changes to the user interface, dispatch the `verify` agent after implementation and
include its evidence in the handoff. Do not merge.
