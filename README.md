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

- **Races are safe.** The Writer serializes every change. When two agents claim the same item, one wins, and `--next` moves the other on to the next item.
- **Long work keeps its lease.** Leases last two hours; rerunning `quipu start N` renews one that has less than 30 minutes left.
- **The owner decides.** Agents never create Epics or approve their own work. The owner makes work Ready, approves goals, and reviews.

## Many agents in parallel worktrees

Each agent works in its own git worktree, and Quipu ties everything local to the worktree, not to the session:

- **Identity.** The lease holder is `provider/login/worktree-id`. The id is created once per worktree, so agents sharing one GitHub login never collide, and a fresh session in the same worktree resumes the work it left.
- **Lease.** `start` records the lease in that worktree's git directory. The edit guard checks the worktree that a tool call touches, so parallel agents are each held to their own lease and branch. A live check is cached for five minutes to keep many agents off the API rate limit.

There are two ways to run several agents:

```sh
# In place: the agent tool made the worktree (Claude Code, Codex, Conductor...)
quipu start --next --json                  # inside that worktree

# Dispatcher: Quipu makes the worktree
quipu start --next --worktree --json       # returns "worktree": "../quipu-worktrees/quipu-086"
# launch an agent in that directory; it resumes the lease. Repeat per agent.
```

`quipu done N` works from any checkout and finds the worktree holding the lease. Remove a finished worktree with `git worktree remove <path>` when nothing runs in it. If `start` says a branch is checked out in another worktree, continue there.

Under load the Writer drains every request posted in the last 30 minutes on each run, so requests from many agents are not lost when GitHub Actions cancels queued runs.

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
