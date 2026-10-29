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

Every result carries a `next` field that says exactly what to do. Agents follow it; people can too, without `--json`. The full agent handbook is [.agent/rules/quipu.md](.agent/rules/quipu.md) (`quipu guide`); owners read the [owner guide](docs/owner-guide.md).

## Quipu is the only way in

Once a repository uses Quipu, work moves only through it:

- **Agents are guarded.** A hook runs `quipu guard` before every agent tool call. Without a lease an agent can only read and test; with one it can change only its own worktree and branch, push only that branch, and open PRs that say `Refs #N`. Merging, approving, labels, Issue and board edits, and changes to `.git`, the config, workflows, and hooks are denied, each with the command to use instead.
- **PRs are checked.** The required `policy` check fails any PR that does not implement exactly one leased leaf Issue, or that would close an Issue on merge.
- **GitHub undoes the rest.** The Writer reopens Issues closed outside the flow, restores hand-edited board fields from its receipts, flags Issues created outside the flow, and only trusts receipts its own workflow posted.
- **`main` is protected.** `quipu protect` requires the checks, blocks direct and force pushes, and allows only `NNN/slug` branches.

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

Every Quipu repository commits its guard hooks (`.claude/settings.json`, `.codex/hooks.json`; `quipu hooks --apply` writes them). Approve them once in Claude Code and Codex, and optionally install the plugin from `plugins/` for session hints. See [source install](docs/source-install.md).

## Operating contract

GitHub is authority. No local `plan/`, `tasks/`, roadmap, or TODO ledger. Every live non-Epic Issue has native parent. Every implementation branch and PR maps to one leaf Issue. Project `Status` is only mutable workflow state; status labels forbidden. Writer serializes mutations. Evidence receipt, not merged PR, closes work.

Read the [agent handbook](.agent/rules/quipu.md), [owner guide](docs/owner-guide.md), [operations guide](docs/operations.md), [source install](docs/source-install.md), and [architecture](docs/architecture.md).

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/quipu
scripts/check-plugins.sh
go run ./cmd/quipu guide --sync   # after changing internal/guide/agent.md or the guard/error tables
```

Plugins live under `plugins/codex` and `plugins/claude`. `.agent/rules/quipu.md` and both plugin skills are generated from `internal/guide/agent.md`; `TestGuidesInSync` fails when they drift. AGENTS.md and CLAUDE.md are short pointers to it, created only when a repository has none.

Projects started before the rename from `gfd` keep working: Quipu still reads the old `gfd-request`/`gfd-receipt`/`gfd-journal` comment markers, and new records use `quipu-*`.

License: [MIT](LICENSE).
