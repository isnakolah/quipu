---
name: quipu
description: Quipu work selection, leases, evidence, and delivery-policy guardrails.
---

You are a member of a delivery team that runs on GitHub. The board, leases, and receipts on GitHub are the only record; never keep local plans or task lists.

## Loop

1. `quipu status --json`: read goals, work in progress, review, Ready work, and `attention`.
2. `quipu start --next --json`: claim the highest-priority Ready, unblocked leaf. It switches this worktree to branch `NNN/slug`, records the lease in this worktree's git dir, and returns `next`. To take a specific Issue, use `quipu start N --json`. If `status` is `Idle`, there is no Ready work: report that and stop. Never branch or edit before `start` succeeds.
3. Implement only the Issue's acceptance criteria. Run the project's tests.
4. Commit, push, and open a PR whose body says `Refs #N` (not `Closes`), then run `quipu done N --json` to move it to In review.
5. After the PR is merged, run `quipu done N --json --criteria "..." --commands "..." --documentation "..." --risks "..." --boundary CI`. PR, merge SHA, and CI URL are filled from GitHub. The Writer records evidence and closes the Issue.
6. Go back to step 1.

Always follow the `next` field of the JSON result.

## Many agents in parallel

Every agent works in its own git worktree. Quipu ties the lease to the worktree, not the session: the holder is `provider/login/worktree-id`, so agents never collide, and a new session in the same worktree resumes its work.

- **In place:** your tool already gave you a worktree (subagent isolation, `--worktree`, Codex, Conductor). Run the loop above inside it.
- **Dispatcher:** run `quipu start --next --worktree --json`. It creates a worktree under `../<checkout>-worktrees/`, claims the work for that worktree, and returns its path in `worktree`. Launch or direct an agent there to implement; it resumes the lease automatically. Dispatch again for each parallel agent.
- `quipu done N` works from any checkout of the repository. After Done, remove the worktree with `git worktree remove <path>` once nothing runs in it.
- If `start` says a branch is checked out in another worktree, continue the work there instead.

## Rules

- **Identity:** `QUIPU_AGENT` names your provider (the session hook sets `claude`); if it is missing, export `QUIPU_AGENT=claude`. Never share a worktree between running agents.
- **Leases:** a lease lasts two hours. Rerun `quipu start N` on long work; it renews the lease when less than 30 minutes remain.
- **Lost races:** a rejected claim means another agent got there first. `--next` already moves on to the next item.
- **Never create Epics or approve your own work.** Propose Epics and Decisions to the owner instead.
- **Links:** use native GitHub parent and blocker links.
- **Bootstrap:** `QUIPU_BOOTSTRAP_ADMIN=1` is the documented bootstrap exception only.
- **Recovery:** the lower-level `quipu work`, `pr link`, and `evidence submit` commands are for recovery only.

Hooks are guardrails. Writer, branch protection, and CI remain enforcement. Review and explicitly trust hook hashes; never bypass trust.
