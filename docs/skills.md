# Skills

A skill gives an agent a repeatable procedure, for example a test run, a review checklist or an incident runbook. Skills are `SKILL.md` files. They extend an agent's instructions without a change to its prompt.

A skill gives instructions only, not access. If a procedure needs a credential or an MCP server, configure the agent's [secrets](secrets.md) and [MCP servers](agents.md#mcp-servers).

## Where skills come from

| Order | Source | Where | Scope |
|---|---|---|---|
| 1 | Agent skills | `agents/<agent>/skills/<skill>/SKILL.md`, and the skills that `run_agents` attaches to a child. | That agent or child run. |
| 2 | Factory skills | `skills/<skill>/SKILL.md` | Every agent of the factory. |
| 3 | Repository skills | `.agents/skills/<skill>/SKILL.md` and `.claude/skills/<skill>/SKILL.md` in the run's repositories, found by the harness. See [Harnesses](harnesses.md#what-each-harness-loads-from-the-repository). | Runs that check out the repository. |
| 4 | Built-in skills | The `openfactory` binary. See [Built-in skills](#built-in-skills). | Depends on the skill. |

For two skills with the same name, the lower order number wins. For example, an agent skill replaces a factory skill for that agent, and a factory skill named `github` replaces the built-in `github` skill.

Example layout: every agent gets the repository conventions, and only `implement` gets the check runner. The full example is [`../examples/02-sdlc-issue-to-pr`](../examples/02-sdlc-issue-to-pr).

```text
skills/
  repository-conventions/
    SKILL.md
agents/
  implement/
    agent.md
    skills/
      run-checks/
        SKILL.md
        scripts/
          run-checks.sh
```

## Built-in skills

A run gets built-in skills by its agent type and the factory's connections:

| Skill | What it covers | Which runs get it |
|---|---|---|
| `github` | Branches, commits, pull requests and review comments on GitHub. | Every agent of a factory with a GitHub repository. |
| `gitlab` | Branches, merge requests and discussions on GitLab. | Every agent of a factory with a GitLab project. |
| `azure-devops` | Branches and pull requests on Azure DevOps. | Every agent of a factory with an Azure DevOps repository. |
| `chat` | Talking in a Slack or Microsoft Teams thread with the factory tools. | The foreman, when the factory connects Slack or Microsoft Teams. |
| `linear` | Reading and updating a Linear issue. | The foreman, triage and spec agents, when the factory connects Linear. |
| `jira` | Reading and updating a Jira issue. | The foreman, triage and spec agents, when the factory connects Jira. |
| `factory-definition` | The definition file format. | Self-improvement runs. `run_agents` can attach it to any child. |

## Skill file format

`SKILL.md` starts with YAML frontmatter; the body holds the procedure.

```markdown
---
name: run-checks
description: Runs the full local validation suite and summarizes failures. Use before reporting an implementation as done.
---

# Run checks

Run `bash scripts/run-checks.sh` from this skill's directory with the
repository root as the working directory. Report each step's result.
```

- `name` (required) is lowercase letters, digits and single hyphens, at most 64 characters, and equals the directory name.
- `description` (required, at most 1024 characters) says what the skill does and when to use it. Agents choose a skill by its description.
- Other files in the directory, for example scripts or templates, are copied with the same relative path.

All field and directory rules are in the [definition reference](definition-reference.md#skills).

## Add or change a skill

1. Create or edit the skill in the definition (the dashboard or definition API for `managed`, the definition repository for `git`).
2. Validate with `openfactory validate <dir>` or `POST /api/v1/factories/{uid}/definition/validate`.
3. Apply: save a `managed` definition, or merge a pull request for a `git` definition (the [`openfactory/definition` check](definition-reference.md#pull-request-check-for-git-definitions) validates it).

New runs use the new skill. A run that already has a sandbox keeps the skill files that it started with.

## Attach a skill to one run

`run_agents` takes a `skills` list for each child (see [Start child runs with run_agents](agents.md#start-child-runs-with-run_agents)). Each entry is a factory skill name, another agent's skill name, a built-in skill name, or a skill spec `owner/repo:path`:

| `path` | Example | Meaning |
|---|---|---|
| A skill directory | `acme/tools:skills/deploy` | The `SKILL.md` and other files of that directory. |
| A `SKILL.md` file | `acme/tools:skills/deploy/SKILL.md` | The same as the directory. |
| A bare name | `acme/tools:deploy` | The first of `deploy`, `.agents/skills/deploy`, `.claude/skills/deploy` and `skills/deploy` that has a `SKILL.md`. |

Open Factory reads the repository's default branch through a forge connection of the team. If no forge can read the skill, the request fails.

## How skills reach a run

The sandbox gets the run's agent, factory and built-in skills. Repository skills stay in the repository, except where a harness needs a copy:

| Location | Harness |
|---|---|
| `.openfactory/skills/<name>/` | All harnesses. |
| `.claude/skills/<name>/` | Claude Code. An existing skill at that path in the repository is not overwritten. The copy gets its own `.gitignore`. |
| `.openfactory/codex/skills/<name>/` (`CODEX_HOME/skills`) | Codex. |

The system prompt lists each skill with its description and the path of its `SKILL.md`. `.openfactory/` has a `.gitignore`, so skill files stay out of `git status`.

A scorer with `selfImprovement: true` can start a run that proposes definition changes, including skill changes, as a pull request. See [Measure and improve](measure-and-improve.md).
