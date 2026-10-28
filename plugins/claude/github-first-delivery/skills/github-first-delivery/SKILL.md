---
name: github-first-delivery
description: GitHub-first work selection, leases, evidence, and delivery-policy guardrails.
---

Everyday flow:

1. `gfd status` shows goals, work in progress, review, Ready work, and anything needing attention.
2. `gfd start N` claims Ready, unblocked leaf Issue #N, creates branch `NNN/slug`, and records the lease in `.git/gfd-lease` so hooks allow edits. Never branch or edit before it succeeds.
3. Push and open a PR whose body says `Refs #N`, then run `gfd done N` to move it to In review.
4. After merge, run `gfd done N --criteria ... --commands ... --documentation ... --risks ... --boundary ...`. PR, merge SHA, and CI URL are filled from GitHub; the Writer records evidence and closes the Issue.

Use native GitHub parent and blocker links. Never create local task ledger or autonomous Epic. `GFD_BOOTSTRAP_ADMIN=1` is documented bootstrap exception only. Lower-level `gfd work`, `pr link`, and `evidence submit` commands remain for recovery.
