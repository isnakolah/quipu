# GitHub-first Delivery

`gfd` is source-first Go control plane for GitHub-only delivery state: Issue graph, Project workflow, leases, durable mutation requests, receipts, evidence, and generated noncanonical Wiki journal.

Status: release candidate. `v0` remains unreleased until source-install, trusted-hook, and disposable-repository evidence is recorded in Gate #27.

## Source install

```sh
gh repo clone isnakolah/github-first-delivery
cd github-first-delivery
GOBIN="$HOME/.local/bin" go install ./cmd/gfd
gfd doctor
```

## Everyday flow

```sh
gfd status        # goals, in progress, in review, ready, needs attention
gfd start 42      # claim #42, create branch 042/slug, record lease for hooks
# ...work, push, open a PR whose body says "Refs #42"...
gfd done 42       # PR open: move #42 to In review
gfd done 42       # PR merged: record evidence (PR, SHA, CI filled in) and close #42
```

Built for AI agents first:
- Every command takes `--json` and returns a `next` field that says exactly what to do.
- `gfd start --next` picks the highest-priority Ready work. When two agents race for the same item, the Writer accepts one claim and the other moves on.
- Each agent session gets its own lease identity (`provider/login/session`) from the plugin session hook.

Run one git worktree per agent.

`start` and `done` post the same durable Writer requests as `gfd work`, `gfd pr link`, and `gfd evidence submit`, and wait for each receipt. Those lower-level commands remain for recovery.

## Operating contract

GitHub is authority. No local `plan/`, `tasks/`, roadmap, or TODO ledger. Every live non-Epic Issue has native parent. Every implementation branch and PR maps to one leaf Issue. Project `Status` is only mutable workflow state; status labels forbidden. Writer serializes mutations. Evidence receipt, not merged PR, closes work.

Read [operations guide](docs/operations.md), [source install and recovery](docs/source-install.md), [architecture](docs/architecture.md), and GitHub Project before work.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/gfd
scripts/check-plugins.sh
```

Plugins live under `plugins/codex` and `plugins/claude`. Review and explicitly trust bundled hooks. Hooks guard common paths only; Writer, CI, and protected branches enforce policy.

License: [MIT](LICENSE).
