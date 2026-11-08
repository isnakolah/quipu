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

**Never wait on a runner or on checks.** Take other work, then look once with `quipu status --json`: `pull_requests` gives each active PR's check results and `next`. When a runner the repository needs is down, `quipu start` and `quipu done` say so in a `runner` field and on stderr, and start it themselves if the owner set a command for that. Carry on either way.

Use the repository's default branch for `--base` (`main` here unless `quipu status` says otherwise).

## Commands you may run

| Command | When | What you get back |
|---|---|---|
| `quipu status --json` | any time | `goals`, `in_progress`, `in_review`, `verifying`, `blocked`, `ready`, `backlog`, `attention`, `owner_queue` (what only the owner can move; leave it and take other work), `pull_requests` (each active PR's check results and what to do next) |
| `quipu start --next --json` | to take work | `issue`, `status`, `branch`, `holder`, `lease_expires`, `next` |
| `quipu start N --json` | to take, resume, or renew Issue #N | same as above |
| `quipu start --next --worktree --json` | to dispatch work to a new worktree | adds `worktree`: the directory to work in |
| `quipu done N --json` | PR open: move to review | `status: "In review"`, `pr`, `next` |
| `quipu done N --json --criteria ... --commands ... --documentation ... --risks ... --boundary ...` | PR merged: record evidence and close | `status: "Done"`, `next` |
| `quipu propose --parent N --title ... --body-file FILE --json` | you found work that needs doing | a proposal comment for the owner |
| `quipu park N --on D --json` | you can't go on until the owner makes Decision #D | `status: "Blocked"`, `next` |
| `quipu ready N --json` | only where the config says `ready_by: agents` | `status: "Ready"` |
| `quipu worktree remove N --json` | to free a finished worktree, by Issue number or path | `status: "Removed"`, `worktree`, `warnings`, `next` |
| `quipu lease release [N] --json` | to give back your own live lease, or clear a record left over from one that is over | `status: "Released"` (back to Ready, branch kept) or `"Cleared"` (record only), `next` |
| `quipu guide` | to read this handbook | this text |
| `quipu runner status --json` | a check won't start, or work seems stuck | each runner's state and labels, what is queued for it, when the Writer last ran |
| `quipu runner setup` | the owner asks how to set a runner up | the steps, printed for the owner |
| `quipu doctor --json` | something seems misconfigured | a list of checks with fixes |
| `quipu config --json` | to see what applies in this repository | each effective setting and where it came from |
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
- After Done, remove a worktree with `quipu worktree remove <path> --json` once nothing runs in it. The agent that held it or a dispatcher may do this; it also deletes the merged local branch. Work that is not Done keeps anything found only in that worktree.
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

**When the config says `merge: agent`,** `quipu done` merges for you instead, and only through Quipu: run it with every evidence flag, and once the required checks and `policy` pass it merges the PR with a merge commit, records the evidence, and closes the Issue in one step. A PR behind the default branch never merges, because nothing tested the two together, even where GitHub would allow it: it stops with `merge-blocked`. While checks run it stops with `checks-pending`; rerun the same command later. If a required check is queued and no online runner has its labels, it stops with `checks-no-runner` instead: don't poll, take other work, and tell the owner what it names. `gh pr merge` stays blocked.

**When the config says `commit_format: issue-prefix`,** every commit is `#NNN type(scope): summary` (NNN your Issue, as in your branch) with a `Refs: #N` footer line, and the PR title is `type(scope): summary`. `quipu verify-commit FILE` checks a message; the `policy` check checks every commit in the PR.

```text
#042 feat(sync): retry failed uploads

Why the change was needed.

Refs: #42
```

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

| Code | Why | Do instead |
|---|---|---|
| `no-lease` | changes need an active lease in this worktree | run `quipu start --next --json` (or `quipu start N --json`), then retry |
| `wrong-branch` | this worktree is not on its leased branch | run `quipu start N --json` with the leased Issue; it switches to the branch. A record left over from other work clears itself once its lease is over, or now with `quipu lease release N --json` |
| `other-worktree` | agents change only the worktree that holds their own lease | work inside your own worktree; dispatch other work with `quipu start --next --worktree --json` |
| `lease-inactive` | the live lease is expired, held by someone else, or no longer active | run `quipu start N --json` to renew or re-take it; `quipu status --json` shows the holder. The leftover record is cleared, so nothing else is blocked |
| `git-internals` | files under .git hold git and Quipu state (lease, identity) | use git and quipu commands; never edit .git directly |
| `quipu-config` | Quipu's config, workflows, and agent hooks are owner-only | ask the owner in a comment on your PR |
| `branch-change` | the work branch is chosen by quipu start | run `quipu start N --json`; restore files with `git restore <file>` or `git checkout -- <file>` |
| `git-refs` | tags, remotes, ref edits, and git config are owner-only | none needed for the flow; ask the owner if you think it is |
| `worktree-add` | work worktrees are created by Quipu so each gets its own identity and lease | run `quipu start --next --worktree --json` |
| `worktree-busy` | that worktree still holds a live lease, or GitHub could not say that it doesn't | finish it with `quipu done N`, then run `quipu worktree remove N --json` |
| `push-target` | agents push only their leased branch to origin | run `git push -u origin <leased branch>` |
| `force-push` | force pushes rewrite shared history | use `git push --force-with-lease origin <leased branch>` |
| `no-verify` | git hooks must run | rerun without --no-verify (or -n) |
| `merge-approve` | agents never merge, approve, or close PRs; the owner reviews | run `quipu done N --json` and wait for review |
| `github-write` | GitHub state changes go through the Quipu Writer | use `quipu start`, `quipu done`, or `quipu propose`; read with gh view/list commands |
| `pr-body` | a PR must be created from the leased branch with a body saying exactly `Refs #N` for the leased Issue and no closing keywords | run `gh pr create --base <default> --head <leased branch> --title ... --body "Refs #N ..."` |
| `quipu-admin` | Quipu recovery and admin commands are owner-only | use `quipu start`, `quipu done`, or `quipu propose`; ask the owner for recovery |
| `quipu-missing` | quipu is not installed, so no call can be checked | ask the owner to install quipu (docs/source-install.md in the Quipu repository) |
| `unparsable` | the guard could not tell where this part of the command writes; nothing else on the line was refused | this is not a policy refusal: give the path or command literally, or assign the variable earlier in the same command (`L=/tmp/check.log; make check > "$L"`) |
| `guard-deny` | the repository refuses this command for agents (deploy, publish, spend) | ask the owner, who runs it or opens a Gate; take other work meanwhile |

## When something fails

`quipu` errors with `--json` look like `{"error": {"code": "...", "message": "...", "next": "..."}}`.

| Code | What happened | Next |
|---|---|---|
| `held-by` | the Issue's lease belongs to another worktree or agent | continue in the worktree that holds it (`quipu status --json` shows holder and branch), or take other work with `quipu start --next --json` |
| `not-ready` | the Issue is not Ready, so it cannot be started | take Ready work with `quipu start --next --json`; only the owner makes work Ready |
| `branch-elsewhere` | the work branch is checked out in another worktree | continue in that worktree, or remove it with `quipu worktree remove <path> --json` if nothing runs there |
| `worktree-held` | that worktree still holds a live lease | finish it with `quipu done N --json` first, or take other work |
| `worktree-dirty` | that worktree has changes or commits found nowhere else, and its Issue is not Done | commit and push them, or `git restore` them from inside it, then rerun the same command |
| `worktree-missing` | no linked worktree of this clone matches | check `git worktree list`; the main checkout is never removed |
| `no-receipt` | the Writer did not answer before the wait ran out | the request stays queued and still applies when the Writer next runs, so rerun the same command later instead of taking other work; it is safe. If it keeps happening, tell the owner what the message says about the Writer and its runner |
| `rejected-stale` | the work changed while your request was in flight | rerun the same command |
| `rejected-taken` | another agent claimed the work first | run `quipu start --next --json` for other work |
| `rejected-blocked` | the work has an open blocker | take other work with `quipu start --next --json` |
| `rejected-journal` | the wiki journal is not synchronized yet | wait a minute, then rerun the same command |
| `rejected-unauthorized` | your GitHub login is not an authorized actor | tell the owner to add your login to `authorized_actors` |
| `rejected-holder` | the request came from an identity that does not hold the lease | run the command in the worktree that holds the work |
| `rejected` | the Writer refused the request | read the message, rerun once, then tell the owner with the message |
| `pr-missing` | no PR exists from the work branch | push the branch and run `gh pr create --base <default> --head <branch> --title ... --body "Refs #N"`, then rerun `quipu done N --json` |
| `pr-closed` | the PR was closed without merging | open a new PR from the same branch with `Refs #N`, then rerun `quipu done N --json` |
| `pr-merged-early` | the PR merged before `quipu done` moved the work to review | tell the owner; they recover it with `quipu pr link` and `quipu evidence submit` |
| `evidence-missing` | recording evidence after merge needs every evidence field | rerun `quipu done N --json --criteria ... --commands ... --documentation ... --risks ... --boundary ...` (add `--ci-url URL` if checks have not passed) |
| `checks-pending` | with `merge: agent`, a required check on the PR has not finished | take other work, look once with `quipu status --json`, and when they have finished rerun the same `quipu done N --json ...` |
| `checks-no-runner` | with `merge: agent`, a required check is queued and no online runner has its labels, so it cannot start | do not poll; take other work with `quipu start --next --json` and tell the owner what the message names. The owner starts that runner, registers one with those labels, changes the job's `runs-on`, or takes the check out of `required_checks`. Then rerun the same `quipu done N --json ...` |
| `checks-failing` | with `merge: agent`, a required check on the PR failed or never ran | fix the cause, push to the work branch, then rerun the same `quipu done N --json ...` |
| `merge-blocked` | GitHub cannot merge the PR: it is a draft, behind the default branch, conflicting, or refused by a rule | update the branch (`git fetch origin && git merge origin/<default>`), resolve conflicts, push, then rerun the same `quipu done N --json ...` |
| `commit-format` | a commit message breaks the repository's `commit_format` | write it as `#NNN type(scope): summary` with a `Refs: #N` footer line (`git commit --amend` for the last commit), and title the PR `type(scope): summary` |
| `not-active` | the Issue is not in progress, in review, or awaiting evidence | check `quipu status --json`; start work with `quipu start N --json` |
| `no-record` | no lease record matches, here or for that Issue in any worktree of this clone | nothing to release; carry on, or take work with `quipu start --next --json` |
| `lease-unverified` | GitHub could not be read and the record has not expired, so the lease may still be live | rerun the same command in a minute; a record clears itself once its lease expires |
| `not-leaf` | the Issue has sub-issues, so no PR may implement it | start one of its leaf Issues instead; `next` names the Ready ones |
| `not-implementable` | the Issue is closed, of a kind PRs do not implement, without a parent, without a valid work contract, or blocked | take other work with `quipu start --next --json`; the message says what is wrong, and the owner fixes the Issue |

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
- Edit the board, `.git/`, `quipu.yml`, `.github/quipu/`, workflows, or agent hooks.
- Create Issues or Epics, or approve your own work.
- Keep local plans or task lists; GitHub is the only record.

## Flags on in this repository

The owner turned these on in `quipu.yml`. Everything above still applies, except where they say otherwise.

- **`issues_by: agents`:** you may create a Story, Task, or Defect under an open Epic with `quipu issue create --kind story --area AREA --parent EPIC --title TITLE --body-file CONTRACT.md --apply`. The body must be a valid work contract. New Epics stay proposals: use `quipu propose`.
- **`triage_by: agents`:** you may triage an Issue labeled `quipu:needs-triage` with `quipu triage N --json` and `--kind`, `--status`, `--priority`, `--area`, or `--parent`; it adds the Issue to the Project if needed. `--status` takes `backlog`, `ready`, or `blocked`, for a move the lifecycle allows, and `ready` needs `ready_by: agents`. `--parent` must be an open Epic. You may file a Decision or Gate under an open Epic with `quipu issue create`, but never decide one. Triage never sets Done or Cancelled and never closes anything.
- **`guard_reads: open`:** the guard also treats more read-only commands as reads, such as `git tag --sort`. Interpreters stay denied, and so does everything else in the handbook.
