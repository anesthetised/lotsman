# Agent instructions

Before planning, implementing, or reviewing audit remediation, read
[tasks/plan.md](tasks/plan.md) and the assigned GitHub issue. The plan defines
task order, file ownership, evidence limits, verification, and approval boundaries.
GitHub Issues are the task tracker; do not create a competing `tasks/todo.md`.

An open issue or this plan is not an instruction to start implementing the entire
backlog. Follow the user's current authorized scope. Preserve unrelated work.
Only the parent agent assigns work or commits; subagents must not delegate, commit,
push, or open PRs. A push needs an explicit user request; opening a PR needs approval.

## Roles and agent tiers

The parent agent orchestrates: it assigns issues, reviews the actual diffs,
integrates, and commits. Subagents implement the assigned slice.

Each issue carries an agent tier label that selects the implementing model:

| Label | Claude | Codex |
|---|---|---|
| `agent:light` | Sonnet, high effort | Sol, high effort |
| `agent:strong` | Opus, high effort | Astra, high effort |

The user chooses the provider for a run; Claude is the current default. The
parent may lower the effort for a narrow task and may use Haiku for trivial
mechanical work (renames, comment fixes, fixture updates). Escalate a light issue
to strong only with a concrete reason and say so in the assignment.
