# Quipu agent handbook

This repository runs on **Quipu**. The work lives on GitHub (Issues, the Project board, PRs, and Writer receipts), and you change it only through the `quipu` command. Everything else is blocked: a guard checks every tool call you make, and GitHub reverts anything that slips past it.

Read this once. Then follow the `next` field that every `quipu ... --json` result gives you.

## The rules in one minute

1. **Start before you touch anything.** Until `quipu start` succeeds you can read, search, and run tests, but every edit, commit, and push is denied.
2. **One Issue, one branch, one PR.** `quipu start` picks the branch (`NNN/short-description`). Never create or switch branches yourself.
3. **Your PR body says `Refs #N`.** Never write `Closes`, `Fixes`, or `Resolves` (in any form) in a PR title, body, or commit message. Only the Writer closes Issues.
4. **You never merge, approve, or close.** The owner reviews and merges. You finish with `quipu done N`.
5. **You never edit the board.** Status, lease, and branch fields change only through `quipu`. Hand edits are put back within minutes.
6. **You never create Epics or new Issues.** Suggest work with `quipu propose`; the owner decides.
7. **Stay in your worktree.** An agent holding a lease changes only its own worktree.

## The loop

```sh
quipu status --json                 # the board: goals, in progress, review, ready, attention
quipu start --next --json           # claim the top Ready Issue; switches to its branch
# implement the Issue's acceptance criteria; run the tests
git add -A && git commit -m "feat: ..."
git push -u origin NNN/short-description
gh pr create --base main --head NNN/short-description --title "feat: ..." --body "Refs #N

What changed and why."
quipu done N --json                 # moves the work to In review; the owner reviews
# after the owner merges the PR:
quipu done N --json --criteria "..." --commands "..." --documentation "..." --risks "..." --boundary CI
```

Then go back to `quipu status --json`. If `start --next` returns `"status": "Idle"`, there is no Ready work: say so and stop. If it returns `"status": "Busy"`, other agents won every race: run it again.

Use the repository's default branch for `--base` (`main` here unless `quipu status` says otherwise).

## Commands you may run

| Command | When | What you get back |
|---|---|---|
| `quipu status --json` | any time | `goals`, `in_progress`, `in_review`, `verifying`, `blocked`, `ready`, `backlog`, `attention` |
| `quipu start --next --json` | to take work | `issue`, `status`, `branch`, `holder`, `lease_expires`, `next` |
| `quipu start N --json` | to take, resume, or renew Issue #N | same as above |
| `quipu start --next --worktree --json` | to dispatch work to a new worktree | adds `worktree`: the directory to work in |
| `quipu done N --json` | PR open: move to review | `status: "In review"`, `pr`, `next` |
| `quipu done N --json --criteria ... --commands ... --documentation ... --risks ... --boundary ...` | PR merged: record evidence and close | `status: "Done"`, `next` |
| `quipu propose --parent N --title ... --body-file FILE --json` | you found work that needs doing | a proposal comment for the owner |
| `quipu guide` | to read this handbook | this text |
| `quipu doctor --json` | something seems misconfigured | a list of checks with fixes |
| `quipu context`, `quipu validate`, `quipu version` | inspecting | read-only reports |

Reading is always allowed: `git status/log/diff/show`, `gh issue|pr|run view/list`, `gh api` GET requests, `ls`, `cat`, `grep`, `rg`, `go test`, and the like.

When a command fails with `--json` it prints `{"error": {"code", "message", "next"}}`. Do what `next` says. The codes are listed under [When something fails](#when-something-fails).

## Many agents in parallel

Each agent works in its own git worktree. Quipu ties the lease to the worktree, not the session: the holder is `provider/login/worktree-id`. Agents sharing one GitHub login never collide, and a new session in the same worktree picks up where the last one stopped.

**In place.** Your tool already gave you a worktree (subagent isolation, Codex, Conductor, `claude --worktree`). Run the loop inside it.

**Dispatcher.** You hand work to other agents:

```sh
quipu start --next --worktree --json   # returns "worktree": "../repo-worktrees/repo-042"
```

Start one agent per returned `worktree`, with that directory as its working directory. Repeat for each parallel agent. You need no lease of your own to dispatch. Paste this prompt for each one:

```text
You work in <worktree> on Issue #<N> of this Quipu repository. Your lease is
already claimed for this worktree. First run `quipu start <N> --json` there
to resume it. Implement only #<N>'s acceptance criteria, run the tests, push
branch <branch>, open a PR with `gh pr create --base main --head <branch>`
and a body that says exactly `Refs #<N>`, then run `quipu done <N> --json`
and report the PR URL. Read `quipu guide` for the rules. Never merge, never
switch branches, never edit outside <worktree>.
```

- `quipu done N` works from any checkout and finds the worktree holding #N.
- After Done, remove a worktree with `git worktree remove <path>` once nothing runs in it.
- If `start` says the branch is checked out in another worktree, continue there.

## Pull requests

The required `policy` check fails the PR unless all of these hold. It lists every problem with its fix.

- The body references exactly one Issue, as `Refs #N`, and N is your leased Issue.
- No closing keyword (`close(s|d)`, `fix(es|ed)`, `resolve(s|d)`, any case, also `owner/repo#N`) appears in the title, body, or any commit message.
- The head branch is your leased `NNN/short-description` branch, and NNN is the Issue number.
- The base is the default branch.
- The Issue is open, a Story, Task, Defect, or Contract, has a parent and a valid work contract, has no sub-issues, and has no open blockers.
- The Issue is In progress or In review, leased to you on this branch, and the PR author matches the lease holder's login.
- The PR changes none of Quipu's enforcement files (`.github/quipu/`, `.github/workflows/`, `.claude/settings.json`, `.codex/`, plugin hooks) unless the owner added the `quipu:owner-approved` label. You cannot add labels.

The check re-runs by itself when the work under it changes, for example when your lease is renewed.

## Review

`quipu done N --json` moves the work to **In review**. The owner reviews and merges; never merge it yourself.

- **Changes requested:** push more commits to the same branch. If the guard says the lease expired, run `quipu start N --json` to renew it first.
- **Merged:** run `quipu done N --json` with the evidence flags below.
- **Closed without merging:** open a new PR from the same branch with `Refs #N`, then run `quipu done N --json` again.

## Evidence

After the merge, `quipu done N` records evidence. The PR URL, merge SHA, and CI run come from GitHub. You supply:

| Flag | What to write | Example |
|---|---|---|
| `--criteria` | each acceptance criterion and its result | `"All 3 criteria met: login rejects bad tokens (test TestLogin), ..."` |
| `--commands` | the exact commands you ran to verify | `"go test ./... ; go vet ./..."` |
| `--documentation` | docs you changed, or `None: <reason>` | `"README Install section"` or `"None: internal refactor"` |
| `--risks` | what could still go wrong, or `None` | `"Untested on Windows"` |
| `--boundary` | the furthest environment you verified in: `local`, `CI`, `target host`, `provider`, `staging`, `production`, `release` | `"CI"` |
| `--environments` | optional; defaults to `GitHub Actions CI` | |
| `--artifacts` | optional URLs of logs or screenshots; defaults to `None: CI run linked` | |
| `--ci-url` | optional; only when no passing check is found | a GitHub Actions run URL of this repository |

The Writer rejects evidence unless the PR is merged from your leased branch with `Refs #N`, the final SHA is the merge commit, and the CI run passed on that PR. Done needs evidence from the current claim, not an earlier one.

## Leases

- A lease lasts two hours. Rerun `quipu start N --json` during long work; it renews the lease when less than 30 minutes remain.
- An expired lease that nobody else took can be re-taken with `quipu start N --json`.
- Claimed and In progress work whose lease expires returns to Ready, so another agent may take it. Work In review keeps its holder.
- The guard checks your lease against GitHub at most every five minutes, so it stays fast with many agents.

## Proposing work

You never create Issues, Epics, or Decisions. When you find work that should exist:

```sh
quipu propose --parent N --title "Short title" --body-file proposal.md --json
```

`N` is the Epic or Story it belongs under. Explain the why, the scope, and the acceptance criteria. Then carry on with your own work. The owner turns proposals into Issues and makes them Ready.

## What the guard blocks

The guard runs before every tool call. A denial looks like this:

```text
QUIPU DENIED [code]: why (detail). Do instead: what to run. Rules: `quipu guide`.
```

Do what it says; never try another way around it. Retrying the same thing in a different form (another tool, a script, `sh -c`, `eval`) is denied the same way.

{{guard-rules}}

## When something fails

`quipu` errors with `--json` look like `{"error": {"code": "...", "message": "...", "next": "..."}}`.

{{flow-errors}}

## What GitHub undoes

Even past the guard, GitHub keeps the flow intact. The Writer runs every few minutes and on every Issue event:

- An Issue closed by anyone but the Writer is reopened, unless the owner closed it as not planned (recorded as Cancelled).
- Board fields edited by hand go back to the state in the last Writer receipt.
- Issues created outside the flow get the `quipu:needs-triage` label, and no agent will be given them.
- `status:` labels are removed; the board's Status field is the only status.
- Receipts count only when the Writer posted them; a copied receipt comment means nothing.
- Branches must be named `NNN/short-description`, and `main` accepts changes only through PRs that pass `test` and `policy`.

## Never

- Edit, commit, or push before `quipu start` succeeds.
- Create, switch, rename, or delete branches, tags, or worktrees by hand.
- Push anything but your leased branch, force-push without `--force-with-lease`, or skip hooks with `--no-verify`.
- Merge, approve, or close a PR or Issue, or add labels.
- Edit the board, `.git/`, `.github/quipu/`, workflows, or agent hooks.
- Create Issues or Epics, or approve your own work.
- Keep local plans or task lists; GitHub is the only record.
