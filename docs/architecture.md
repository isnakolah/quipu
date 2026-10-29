# Architecture

`quipu` stores durable delivery truth in GitHub: Issues and native relationships define work graph; Project fields define workflow; Pull Requests define code review; Writer receipts define mutation/evidence history. Local cache is disposable metadata only. Repository config stores stable identifiers and policy, never status, leases, tokens, or cache.

Writer accepts unedited, hash-verified request comments, rereads state fingerprint, mutates once, then writes immutable-by-policy receipt. Reconciliation replays unreceipted requests every five minutes. Hooks only protect common local paths. CI and Writer enforce policy.

Lifecycle: `Backlog → Ready → Claimed → In progress → In review → Evidence pending → Done`, with explicit Blocked and recovery paths. Lease TTL is two hours.
