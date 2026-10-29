# Quipu

**Quipu** runs a software project on GitHub for a team of AI agents, with a human owner who sets goals and approves the work.

The name comes from the *quipu* (khipu), the knotted cords the Inca used to keep records of work and tribute without writing. Here each piece of work becomes a knot in the record: which agent held it, which branch and PR it went through, and the proof that it was done. Every knot lives on GitHub (Issues, the Project, PRs, receipts), never in local files.

Status: release candidate. `v0` remains unreleased until source-install, trusted-hook, and disposable-repository evidence is recorded in Gate #27.

## The agent loop

```sh
quipu status --json              # the board: goals, in progress, review, ready, attention
quipu start --next --json        # claim top Ready work, create branch NNN/slug, hold the lease
# ...implement, test, push, open a PR whose body says "Refs #N"...
quipu done N --json              # PR open: move to In review
quipu done N --json --criteria "..." --commands "..." --documentation "..." --risks "..." --boundary CI
                                 # PR merged: record evidence (PR, SHA, CI filled in) and close
```

Every result carries a `next` field that says exactly what to do. Agents follow it; people can too, without `--json`.

- **Many agents at once.** Each session holds leases as `provider/login/session` (set by the plugin session hook), so agents sharing one GitHub login never collide. Run one git worktree per agent.
- **Races are safe.** The Writer serializes every change. When two agents claim the same item, one wins, and `--next` moves the other on to the next item.
- **Long work keeps its lease.** Leases last two hours; rerunning `quipu start N` renews one that has less than 30 minutes left.
- **The owner decides.** Agents never create Epics or approve their own work. The owner makes work Ready, approves goals, and reviews.

`start` and `done` post the same durable Writer requests as `quipu work`, `quipu pr link`, and `quipu evidence submit`, and wait for each receipt. Those lower-level commands remain for recovery.

## How work is organised

| Piece | Lives in | Meaning |
|---|---|---|
| Goal (Epic) | Issue, owner-approved | An outcome; never implemented directly |
| Task (Story, Task, Defect) | Issue with native parent | One branch, one PR |
| Board | GitHub Project `Status` | `Backlog → Ready → Claimed → In progress → In review → Evidence pending → Done` |
| Lease | Project fields | Which agent holds the task, for two hours at a time |
| Receipt | Issue comment by the Writer | The knot: an append-only record of each change |

## Source install

```sh
gh repo clone isnakolah/quipu
cd quipu
GOBIN="$HOME/.local/bin" go install ./cmd/quipu
quipu doctor
```

Install the Claude or Codex plugin from `plugins/`, then review and explicitly trust its hooks.

## Operating contract

GitHub is authority. No local `plan/`, `tasks/`, roadmap, or TODO ledger. Every live non-Epic Issue has native parent. Every implementation branch and PR maps to one leaf Issue. Project `Status` is only mutable workflow state; status labels forbidden. Writer serializes mutations. Evidence receipt, not merged PR, closes work.

Read [operations guide](docs/operations.md), [source install and recovery](docs/source-install.md), [architecture](docs/architecture.md), and GitHub Project before work.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/quipu
scripts/check-plugins.sh
```

Plugins live under `plugins/codex` and `plugins/claude`. Hooks guard common paths only; Writer, CI, and protected branches enforce policy.

Projects started before the rename from `gfd` keep working: Quipu still reads the old `gfd-request`/`gfd-receipt`/`gfd-journal` comment markers, and new records use `quipu-*`.

License: [MIT](LICENSE).
