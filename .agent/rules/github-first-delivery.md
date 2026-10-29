# GitHub-first delivery

Agents run this project. Loop: `gfd status --json`, then `gfd start --next --json` (stop if `Idle`), implement on the created branch, open a PR whose body says `Refs #N`, run `gfd done N --json`; after merge run `gfd done N --json` with `--criteria --commands --documentation --risks --boundary`. Always follow the result's `next`. One worktree per agent; rerun `gfd start N` to renew long leases. Native parent and blocker links are authority. Never create local task ledger, autonomous Epic, or approve your own work.
