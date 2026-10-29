---
name: github-first-delivery
description: GitHub-first work selection, leases, evidence, and delivery-policy guardrails.
---

You are a member of a delivery team that runs on GitHub. The board, leases, and receipts on GitHub are the only record; never keep local plans or task lists.

## Loop

1. `gfd status --json`: read goals, work in progress, review, Ready work, and `attention`.
2. `gfd start --next --json`: claim the highest-priority Ready, unblocked leaf. It creates branch `NNN/slug`, records the lease in `.git/gfd-lease`, and returns `next`. To take a specific Issue, use `gfd start N --json`. If `status` is `Idle`, there is no Ready work: report that and stop. Never branch or edit before `start` succeeds.
3. Implement only the Issue's acceptance criteria. Run the project's tests.
4. Commit, push, and open a PR whose body says `Refs #N` (not `Closes`), then run `gfd done N --json` to move it to In review.
5. After the PR is merged, run `gfd done N --json --criteria "..." --commands "..." --documentation "..." --risks "..." --boundary CI`. PR, merge SHA, and CI URL are filled from GitHub. The Writer records evidence and closes the Issue.
6. Go back to step 1.

Always follow the `next` field of the JSON result.

## Rules

- **Identity:** each session gets its own identity (`provider/login/session`) from `GFD_AGENT`, set by the session hook. If it is missing, export `GFD_AGENT=<provider>/<session>` yourself.
- **Worktrees:** use one git worktree per agent, so leases never collide.
- **Leases:** a lease lasts two hours. Rerun `gfd start N` on long work; it renews the lease when less than 30 minutes remain.
- **Lost races:** a rejected claim means another agent got there first. `--next` already moves on to the next item.
- **Never create Epics or approve your own work.** Propose Epics and Decisions to the owner instead.
- **Links:** use native GitHub parent and blocker links.
- **Bootstrap:** `GFD_BOOTSTRAP_ADMIN=1` is the documented bootstrap exception only.
- **Recovery:** the lower-level `gfd work`, `pr link`, and `evidence submit` commands are for recovery only.

Hooks are guardrails. Writer, branch protection, and CI remain enforcement. Review and explicitly trust hook hashes; never bypass trust.
