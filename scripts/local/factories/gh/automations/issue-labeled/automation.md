---
displayName: Issue with the factory label
agent: foreman
triggers:
  - provider: github
    event: issue_labeled
    filter:
      repos: [__GITHUB_OWNER__/__GITHUB_NAME__]
      skip_bots: none
---

An issue got the label of this factory. Do what the issue text says.
