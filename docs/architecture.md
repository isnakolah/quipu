# Architecture

GitHub holds every durable fact. Issues and their native parent and blocker links are the work graph. Project fields are the workflow state. Pull requests are the code review. Writer receipts are the history of every change and its evidence. The repository config holds only stable identifiers and policy.

The config is `quipu.yml` at the repository root, or `.github/quipu/config.yaml` in repositories adopted earlier; having both is an error. Settings that relax enforcement are read from origin's default branch, whose name comes from git, not from the config. A worktree's copy cannot turn one on, and the Writer and `policy` only ever check out the default branch. With an origin but no readable copy on its default branch, every relaxing setting is off.

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

The Writer is the only mutator of lifecycle state. It accepts unedited, hash-verified requests from authorized actors, re-reads the state fingerprint, applies one lifecycle transition, and writes a receipt carrying the resulting state. Reconciliation replays unanswered requests, clears expired leases, puts back anything changed outside the Writer, and, under `merge_wait: background`, merges queued PRs whose checks passed. A queued merge is a receipt the Writer posted, so it lives on GitHub like everything else and needs no agent running.

Lifecycle: `Backlog → Ready → Claimed → In progress → In review → Evidence pending → Done`, with Blocked, Cancelled, and Archived. A lease lasts two hours.

`quipu daemon` adds no authority: it takes work through `quipu start` and removes worktrees through the same check as `quipu worktree remove`, so the Writer still decides everything. `quipu park` is one Writer request that checks the Decision, adds the blocker and moves the work. `auto_ready` is the Writer moving work once its last blocker closes, and it posts a receipt like any other move.

With `workflows.writer_mode: serial`, one Actions job applies requests in comment order and GitHub events only wake it. Job-level concurrency keeps comments that aren't requests from taking the one pending place. Trust is unchanged, because it is still the Writer in Actions: there is no local Writer, since receipts count only from `github-actions[bot]`.

With `merge: queue`, a local worker per clone tests batches of queued PRs merged onto the latest default branch, lands exactly the tested result through GitHub one merge at a time, and reports each to the Writer, which alone closes the Issues. Writer receipts decide what is queued and GitHub decides what landed, so a restart neither loses nor merges twice.

With `merge_without_ci`, the client merges on its own check and tells the Writer so. Trust stays where it was: the Writer still posts every receipt, and it accepts that evidence only when its own read of the default branch's config allows it. The receipt records a client's report, not a verified run.

Agents never wait on a runner. `quipu start` and `quipu done` may start one with the default branch's `workflows.runner_start`, on the agent's machine and without blocking; this changes nothing on GitHub, so the Writer stays the only mutator. Check results reach agents by reading: `quipu status` reports them when asked, and nothing polls. With `merge_wait: background`, the Writer wakes when the checks' workflows finish.

## Enforcement

- **Locally, for agents:** `quipu guard` denies everything off the flow (see [operations](operations.md#the-guard)).
- **On every PR:** the required `policy` check.
- **On GitHub, for everyone:** the `quipu-main` and `quipu-branch-names` rulesets (`quipu protect`), and the Writer's watchdog.

The agent handbook in `internal/guide/agent.md` is the single source for agent instructions. `quipu guide --sync` writes it to `.agent/rules/quipu.md` and the plugin skills, filling in the guard and error tables from the code, and `TestGuidesInSync` fails on drift. It creates AGENTS.md and CLAUDE.md as pointers only when a repository has none, and never modifies a project's own; when they lack the pointer, it says so. Rules for the autonomy flags live with the code in `cmd/quipu/guide.go`: a repository's `.agent/rules/quipu.md` adds a section for each flag on, while the plugin skills, which ship to every repository, state the flags-off rules.
