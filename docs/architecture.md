# Architecture

GitHub holds every durable fact. Issues and their native parent and blocker links are the work graph. Project fields are the workflow state. Pull requests are the code review. Writer receipts are the history of every change and its evidence. The repository config holds only stable identifiers and policy.

Local state is a mirror, never authority:

- `<git-dir>/quipu-agent-id`: the worktree's agent identity, so the lease holder is `provider/login/worktree-id`.
- `<git-dir>/quipu-lease`: the lease this worktree holds, written by `quipu start`, re-verified against GitHub by the guard at most every five minutes.
- The metadata cache: fetched data, safe to delete.

Both mirrors live inside the git directory, which the guard forbids agents to touch.

## Change path

```text
agent ── quipu start / done ──► request comment ──► Writer (Actions) ──► Project fields
  │                                                    │
  └─ every tool call ─► quipu guard                    └─► receipt (github-actions[bot])
```

The Writer is the only mutator of lifecycle state. It accepts unedited, hash-verified requests from authorized actors, re-reads the state fingerprint, applies one lifecycle transition, and writes a receipt carrying the resulting state. Reconciliation replays unanswered requests, clears expired leases, and puts back anything changed outside the Writer.

Lifecycle: `Backlog → Ready → Claimed → In progress → In review → Evidence pending → Done`, with Blocked, Cancelled, and Archived. A lease lasts two hours.

## Enforcement

- **Locally, for agents:** `quipu guard` denies everything off the flow (see [operations](operations.md#the-guard)).
- **On every PR:** the required `policy` check.
- **On GitHub, for everyone:** branch protection and the branch-name ruleset (`quipu protect`), and the Writer's watchdog.

The agent handbook in `internal/guide/agent.md` is the single source for agent instructions. `quipu guide --sync` generates AGENTS.md, CLAUDE.md, `.agent/rules/quipu.md`, and the plugin skills from it, filling in the guard and error tables from the code, and `TestGuidesInSync` fails on drift.
