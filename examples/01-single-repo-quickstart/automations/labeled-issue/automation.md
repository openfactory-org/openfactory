---
agent: foreman
triggers:
  - provider: github
    event: issue_labeled
    filter:
      repos: [acme/payments-service]
      labels: [factory-ready]
---

An issue was labeled `factory-ready`. Work it through to a draft pull
request, keeping the issue's acceptance criteria. Ask a human about any
unresolved product question instead of guessing.
