# Quipu agent handbook

This repository runs on **Quipu**. The work lives on GitHub (Issues, the Project board, PRs, and Writer receipts), and you change it only through the `quipu` command. Everything else is blocked: a guard checks every tool call you make, and GitHub reverts anything that slips past it.

Read this once. Then follow the `next` field that every `quipu ... --json` result gives you.

## The rules in one minute

1. **Start before you touch anything.** Until `quipu start` succeeds you can read, search, and run tests, but every edit, commit, and push is denied.
2. **One Issue, one branch, one PR.** `quipu start` picks the branch (`NNN/short-description`). Never create or switch branches yourself.
3. **Your PR body says `Refs #N`.** Never write `Closes`, `Fixes`, or `Resolves` (in any form) in a PR title, body, or commit message. Only the Writer closes Issues.
4. **You never merge, approve, or close.** The owner reviews and merges. You finish with `quipu done N`. The one close you may ask for is a PR nothing needs any more, with `quipu pr close`.
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
quipu done --evidence-template > ../evidence-N.md   # fill it in, outside the worktree
quipu done N --json --evidence-file ../evidence-N.md
```

Then go back to `quipu status --json`. If `start --next` returns `"status": "Idle"`, there is no Ready work: say so and stop. If it returns `"status": "Busy"`, other agents won every race: run it again.

**Never wait on a runner or on checks.** Take other work, then look once with `quipu status --json`: `pull_requests` gives each active PR's check results and `next`. When a runner the repository needs is down, `quipu start` and `quipu done` say so in a `runner` field every time (and on stderr once per worktree every six hours), and start it themselves if the owner set a command for that. Carry on either way.

Use the repository's default branch for `--base` (`main` here unless `quipu status` says otherwise).

## Commands you may run

| Command | When | What you get back |
|---|---|---|
| `quipu status --json` | any time | `goals`, `in_progress`, `in_review`, `verifying`, `blocked`, `ready`, `backlog`, `attention`, `owner_queue` (what only the owner can move; leave it and take other work), `pull_requests` (each active PR's check results and what to do next) |
| `quipu start --next --json` | to take work | `issue`, `status`, `branch`, `holder`, `lease_expires`, `next` |
| `quipu start N --json` | to take, resume, or renew Issue #N | same as above |
| `quipu start --next --worktree --json` | to dispatch work to a new worktree | adds `worktree`: the directory to work in |
| `quipu check --json` | before you commit, where the repository sets `local_check` or `merge_queue.check` | runs that check on the worktree as it stands and records `tree`, `command`, `result`, `at`; `reused: true` when this tree already passed it. `quipu done` and the merge queue reuse a pass for the exact tree and command they would test, so commit exactly what you checked |
| `quipu done N --json` | PR open: move to review | `status: "In review"`, `pr`, `next` |
| `quipu done N --json --evidence-file FILE` | PR merged: record evidence and close (or the `--criteria ... --boundary ...` flags) | `status: "Done"`, `next` |
| `quipu propose --parent N --title ... --body-file FILE --json` | you found work that needs doing | a proposal comment for the owner |
| `quipu park N --on D --json` | you can't go on until the owner makes Decision #D | `status: "Blocked"`, `next` |
| `quipu ready N --json`, `quipu move N ready --json`, `quipu move N backlog --json` | only where the config says `ready_by: agents`, and never for an Epic, Decision, or Gate | `status: "Ready"` or `"Backlog"` |
| `quipu pr close PR --json` | a PR, by number or URL, that nothing needs any more: every Issue it references is Done or Cancelled, or has a different merged PR | `status: "Closed"`, `pr`; the Writer posts the reason on the PR. It refuses while an Issue the PR references is still in progress |
| `quipu worktree remove N --json` | to free a finished worktree, by Issue number or path | `status: "Removed"`, `worktree`, `warnings`, `next` |
| `quipu lease release [N] --json` | to give back your own live lease, or clear a record left over from one that is over | `status: "Released"` (back to Ready, branch kept) or `"Cleared"` (record only), `next` |
| `quipu guide` | to read this handbook | this text |
| `quipu <command> --help`, `quipu help <command>` | to see a command's flags | its usage; help never runs the command, and the guard always allows it |
| `quipu runner status --json` | a check won't start, or work seems stuck | each runner's state and labels, what is queued for it, when the Writer last ran |
| `quipu runner setup` | the owner asks how to set a runner up | the steps, printed for the owner |
| `quipu doctor --json` | something seems misconfigured | a list of checks with fixes |
| `quipu config --json` | to see what applies in this repository | each effective setting and where it came from |
| `quipu context`, `quipu validate`, `quipu version` | inspecting | read-only reports |

Reading is always allowed: `git status/log/diff/show`, `gh issue|pr|run view/list`, `gh api` GET requests, `ls`, `cat`, `grep`, `rg`, `go test`, and the like.

Every command takes `--json`. When a command fails with `--json` it prints `{"error": {"code", "message", "next"}}`. Do what `next` says. The codes are listed under [When something fails](#when-something-fails).

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
already claimed for this worktree. Run every shell command as
`cd <worktree> && ...` and give edit tools absolute paths under <worktree>.
First run `cd <worktree> && quipu start <N> --json` to resume it. Implement
only #<N>'s acceptance criteria, run the tests, push branch <branch>, open a
PR with `gh pr create --base main --head <branch>` and a body that says
exactly `Refs #<N>`, then run `quipu done <N> --json` and report the PR URL.
Read `quipu guide` for the rules. Never merge, never switch branches, never
edit outside <worktree>.
```

**Subagents started in the main checkout.** Many tools start a subagent in the folder its parent runs in, usually the main checkout, which holds no lease. That subagent can still implement the leased Issue: the guard judges each change by the worktree it lands in, not by the session's folder. An edit into a worktree is allowed when that worktree's live lease is held by the caller's own GitHub login; a shell command that starts with `cd <worktree> &&` is judged there too. An edit into a worktree leased to a different login is denied `other-worktree`, and so is any edit into another worktree from a session whose own worktree holds a lease. The guard learns your login from `gh` (cached for an hour), so keep `gh` signed in.

- `quipu done N` works from any checkout and finds the worktree holding #N.
- After Done, remove a worktree with `quipu worktree remove <path> --json` once nothing runs in it. The agent that held it or a dispatcher may do this; it also deletes the merged local branch. Work that is not Done keeps anything found only in that worktree.
- For an hour after `quipu done` frees your worktree (`done_grace_minutes`), you may still tidy it up there: the repository's cleanup, `cargo clean`, `go clean`, `make clean`, `git clean`, and `rm` of files git does not track. Commits, pushes, and edits are denied as before. `git branch -d NNN/...` deletes a merged work branch that no worktree has checked out, at any time.
- If `start` says the branch is checked out in another worktree, continue there.

## Pull requests

The required `policy` check fails the PR unless all of these hold. It lists every problem with its fix.

- The body references exactly one Issue, as `Refs #N`, and N is your leased Issue.
- No closing keyword (`close(s|d)`, `fix(es|ed)`, `resolve(s|d)`, any case, also `owner/repo#N`) appears in the title, body, or any commit message.
- The head branch is your leased `NNN/short-description` branch, and NNN is the Issue number.
- The base is the default branch.
- The Issue is open, a Story, Task, Defect, or Contract, has a parent and a valid work contract, has no sub-issues, and has no open blockers.
- The Issue is In progress or In review, leased to you on this branch, and the PR author matches the lease holder's login.
- The PR changes none of Quipu's enforcement files (`quipu.yml`, `.github/quipu/`, `.github/workflows/`, `.claude/settings.json`, `.codex/`, plugin hooks) unless the owner added the `quipu:owner-approved` label. You cannot add labels.

The check re-runs by itself when the work under it changes, for example when your lease is renewed.

## Review

`quipu done N --json` moves the work to **In review**. The owner reviews and merges; never merge it yourself.

**When the config says `merge: agent`,** `quipu done` merges for you instead, and only through Quipu: run it with your evidence file (or every evidence flag), and once the required checks and `policy` pass it merges the PR with a merge commit, records the evidence, and closes the Issue in one step. A PR behind the default branch never merges, because nothing tested the two together, even where GitHub would allow it: it stops with `merge-blocked`. While checks run it stops with `checks-pending`; rerun the same command later. If a required check is queued and no online runner has its labels, it stops with `checks-no-runner` instead: don't poll, take other work, and tell the owner what it names. If it merged but the Writer hasn't answered yet, it returns `"status": "Merged, receipt pending"`: you are finished, the Writer closes the Issue, so take other work. `gh pr merge` stays blocked.

**When the config says `commit_format: issue-prefix`,** every commit is `#NNN type(scope): summary` (NNN your Issue, as in your branch) with a `Refs: #N` footer line, and the PR title is `type(scope): summary`. `quipu verify-commit FILE` checks a message; the `policy` check checks every commit in the PR.

```text
#042 feat(sync): retry failed uploads

Why the change was needed.

Refs: #42
```

- **Changes requested:** push more commits to the same branch. If the guard says the lease expired, run `quipu start N --json` to renew it first.
- **Merged:** run `quipu done N --json --evidence-file FILE` with the evidence below.
- **Closed without merging:** open a new PR from the same branch with `Refs #N`, then run `quipu done N --json` again.

## Evidence

After the merge, `quipu done N` records evidence. The PR URL, merge SHA, and CI run come from GitHub. You supply the rest in a file, so none of it is on the command line:

```sh
quipu done --evidence-template > ../evidence-N.md
quipu done N --json --evidence-file ../evidence-N.md
```

The file has one `## Section` per field below (`## Criteria`, `## Commands`, `## Documentation`, `## Risks`, `## Boundary`, and optionally `## Environments`, `## Artifacts`, `## CI URL`), or is a JSON object with the keys `criteria`, `commands`, `documentation`, `risks`, `boundary`, `environments`, `artifacts`, `ci_url`. Lines in a section are joined with `; `. A missing section is named in the error. The flags still work, and a flag given with a file overrides that field.

| Flag / section | What to write | Example |
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
- Claimed and In progress work whose lease expires returns to Ready, so another agent may take it, unless its branch has an open PR: that work stays yours, and `quipu start N --json` renews it. Work In review keeps its holder.
- The guard checks your lease against GitHub at most every five minutes, so it stays fast with many agents.
- A lease record never outlives its lease. Once a lease has expired, or GitHub says its Issue is finished or held by someone else, the guard clears that worktree's record on the next change and judges the change as if there were none; `quipu status` clears expired records in every worktree of the clone. To give back work you are not going to finish, run `quipu lease release N --json`: it returns your live lease to Ready and keeps the branch. It never touches someone else's live lease.

## Parking on a Decision

When your work can't go on until the owner decides something, commit and push what you have, then run `quipu park N --on D --json`, where D is an open Decision. Your Issue moves to Blocked and keeps its branch, your lease and build slot are freed, and the owner sees it in `owner_queue`. Then take other work. When the work is Ready again, `quipu start N --json` picks it up from its branch, in any worktree.

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
- Merge, approve, or close a PR or Issue, or add labels. (`quipu pr close` may close a PR nothing needs.)
- Edit the board, `.git/`, `quipu.yml`, `.github/quipu/`, workflows, or agent hooks.
- Create Issues or Epics, or approve your own work.
- Keep local plans or task lists; GitHub is the only record.
