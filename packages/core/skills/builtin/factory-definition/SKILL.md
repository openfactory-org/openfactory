---
name: factory-definition
description: The file format of a factory definition (factory.yaml, agents, skills, automations). Use before you read or change definition files, for example in a Self-improvement run.
---

# Change a factory definition

## Layout

```text
factory.yaml                       factory settings, repositories, defaults
agents/<name>/agent.md             one agent: YAML frontmatter + instructions
agents/<name>/skills/<s>/SKILL.md  a skill of one agent
skills/<s>/SKILL.md                a skill of every agent
automations/<name>/automation.md   triggers that start runs
machines/<name>.yaml               sandbox settings
webhooks/<name>.yaml               custom webhooks
scorers/<slug>/scorer.md           scorers
benchmarks/<suite>/...             benchmark suites and tasks
```

## agent.md

```markdown
---
description: One line that says what the agent does.
agentType: IMPLEMENT        # FOREMAN, TRIAGE, SPEC, IMPLEMENT, REVIEW, VERIFY or CUSTOM
model: <model id>           # optional; agentDefaults applies
---

# Role
...instructions...
```

- The body is the instructions of the agent. The workflow policy (spec
  approval, questions, merges, completion) is in the body of the foreman.
  The platform adds no policy text, so a change in the body changes the
  behavior.
- A factory has exactly one `FOREMAN`.

## SKILL.md

```markdown
---
name: run-checks          # equal to the directory name; lowercase and hyphens
description: What it does and when to use it.
---
...procedure...
```

## Rules for a change

1. Make the smallest change that fixes the problem. Keep the style of the
   file.
2. Validate the tree: `openfactory validate <dir>`. Fix each error.
3. Explain the change in the pull request: the failure that you saw, the
   runs that show it, and why the change helps.
4. Never add secrets to definition files. Secrets are referenced by name.
