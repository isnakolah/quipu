# Owner guide

You are the owner of a Quipu repository. You set goals, decide what is Ready, review and merge PRs, and keep the Writer running. Agents do the work. This page covers everything you do. Agents read [.agent/rules/quipu.md](../.agent/rules/quipu.md) (`quipu guide`) instead.

## The one rule

**Once a repository uses Quipu, Quipu is the only way work moves.** Agents are blocked from everything else. When you change the board by hand, GitHub puts it back:

| You want to | Run | Not |
|---|---|---|
| Make work available to agents | `quipu ready N` | dragging the card to Ready |
| Send unstarted work back | `quipu move N backlog` (or `blocked`) | editing Status |
| Archive finished or cancelled work | `quipu move N archived` | editing Status |
| Create work | `quipu issue create --kind story --area AREA --parent EPIC --title TITLE --body-file CONTRACT.md --apply` | "New issue" without a parent and contract |
| Create a goal | `quipu issue create --kind epic --area AREA --approved-by @you --title TITLE --body-file EPIC.md --apply` | |
| Cancel work | close the Issue as **not planned** | closing it as completed |
| Finish an Epic, Decision, or Gate | close it (completed) | |
| Finish a Story, Task, Defect, or Contract | merge its PR; the agent runs `quipu done N` | closing the Issue |

The Writer runs every five minutes and on every Issue event. It:

- reopens an Issue closed without a Writer Done receipt, except one you close as not planned (recorded Cancelled) or an Epic, Decision, or Gate you close (recorded Done);
- restores Status, Lease holder, Lease expires, and Branch to the state in the last Writer receipt;
- labels Issues created outside the flow `quipu:needs-triage` and explains what is missing, then removes the label once the Issue is valid;
- removes `status:` labels.

Issues closed before the cutover (`receipts_trusted_before` in the config) are left alone.

## Setup checklist

1. **Install Quipu** from source: [source-install.md](source-install.md).
2. **Bootstrap or adopt.** `quipu init` for a new repository; `quipu adopt` for an existing one (see [Adopting an existing repository](#adopting-an-existing-repository)). Both write the config, the workflows, the Issue forms, the agent handbook (`.agent/rules/quipu.md`), and the guard hooks, and create AGENTS.md and CLAUDE.md pointers only if the repository has none. If you already have your own, add a line pointing agents at `.agent/rules/quipu.md` (and `@.agent/rules/quipu.md` in CLAUDE.md); `quipu guide --sync` reminds you. Commit them.
3. **Create the Writer token.** A classic personal access token with the `project` scope plus `public_repo` for a public repository, or `repo` for a private one. Fine-grained tokens cannot change a personal Project. Set it as an Actions secret, never anywhere else:
   ```sh
   gh secret set QUIPU_WRITER_TOKEN --repo OWNER/REPO   # paste the token when prompted
   ```
   Without it the Writer cannot claim, review, or finish anything, and the `policy` check fails every PR.
4. **Protect the repository.** `quipu protect` shows the plan; `quipu protect --apply` applies it as two repository rulesets:
   - `quipu-main`: the default branch takes changes only through PRs that pass `policy` and the `required_checks` (default `test`), reported by GitHub Actions, with conversations resolved; no force pushes or deletion;
   - `quipu-branch-names`: new branches must be `NNN/short-description` (or Dependabot's).

   Rulesets apply to admins too. `quipu protect --apply --admins-exempt` lets repository admins bypass `quipu-main`, so your own merges can skip failing checks; agents using your login can then bypass it too if they get past the guard. Private repositories need a paid plan for rulesets.
5. **Install the hooks** if `init` or `adopt` did not: `quipu hooks --apply`, then commit `.claude/settings.json` and `.codex/hooks.json`. Each person running agents approves the hooks once (Claude Code: `/hooks`; Codex: trust the project). Install the plugin for the session hints too ([commands](source-install.md#agent-hooks-and-plugins)).
6. **Check everything.** `quipu doctor` must print no `FAIL`.

## Adopting an existing repository

`quipu adopt` works in a clone of a repository that has code and PRs but no Issues yet. Create an empty Project first (`gh project create --owner OWNER --title TITLE`), then:

```sh
quipu adopt --owner OWNER --repo REPO                         # audit: counts Issues, ignoring PRs
quipu adopt --owner OWNER --repo REPO --project-number N --apply \
  --runs-on self-hosted,macOS --quipu-version v0.2.0
```

It provisions what `init` does, changing only what is missing: labels, the Project fields, Status options, and views, the workflows and Issue forms, the config, the handbook, and the hooks. Run it again at any time; a second run reports `"changed": false`. It refuses a repository that already has Issues. Then bring your existing backlog and history in with `quipu import --manifest FILE --apply` ([import](import.md)): it creates the Issues with parents, blockers, and fields, and the Writer closes imported Done work with historical evidence.

Files that already exist and differ from Quipu's (your own `ci.yml`, say) are kept and listed under `kept`. Rerun with `--replace` to overwrite them. When you keep your own CI, set `required_checks` to its job names so protection requires them.

Adopt never rewrites the options of a Project field on a Project that has items, because GitHub clears every item's value when options change. It tells you which options to add by hand instead.

## Runners and workflows

The generated workflows follow the `workflows` section of the config. Change it through a reviewed PR, then regenerate the files with `quipu adopt --apply --replace` and commit them.

```yaml
workflows:
  runs_on:
    default: [self-hosted, macOS]   # every job; ubuntu-latest when unset
    test: [ubuntu-latest]           # or per job: writer, policy, test
  quipu_version: v0.2.0             # tag or commit the workflows install
  schedule: "*/15 * * * *"          # Writer cron; "off" to rely on events only
  writer_on_pull_request: false     # skip Writer runs on PR events
  defects_from: [CI]                # failed default-branch runs become Ready P0 Defects; [] for off
  runner_start: "ssh box 'cd actions-runner && ./svc.sh start'"  # starts a runner that is down; unset by default
required_checks: [test]             # CI checks main requires besides policy
```

- A runner that already has `quipu` on its `PATH` uses it and skips installing Go and Quipu. Keep it at `quipu_version`.
- Without the schedule, expired leases are reclaimed and PR policy rechecked only when an Issue event or a manual run (`gh workflow run quipu-writer.yml`) wakes the Writer.
- Self-hosted runners: register one under the repository's **Settings → Actions → Runners** with the labels you list, and keep it online. `policy` runs on `pull_request_target`, which checks out only the default branch, so a PR's code never runs on your runner through Quipu's workflows.
- `quipu runner setup` prints the steps for the runners your config needs: the registration with the right labels, the service, and `quipu` at the right version on the runner's `PATH`. `quipu doctor` checks the labels, whether each runner is online, and this machine's `PATH` copy. It cannot see another machine's service, or a runner's own `PATH`.
- `quipu runner status --json` shows each runner's state and labels, what is queued for it, and when the Writer last succeeded.

**What `workflows.runner_start` runs, and where.** It is one line, run with `sh -c` from the home directory, on the machine of the agent running `quipu start` or `quipu done`, as that agent's user. It does not run from the repository, so a script it calls must live outside it, or an agent's branch could change what runs. It must work from every machine your agents use, and needs its own way to reach the runner's host, such as an SSH key. Quipu runs it when a runner a generated or required job needs is missing or offline, because an autoscaled runner registers only once it starts. Output goes to `quipu-runner-start.log` in the worktree's git directory. Quipu waits two seconds to catch a fast failure, then carries on while the command keeps running in its own session, and runs it at most once every ten minutes per clone. A failure is reported to each agent during those ten minutes.

Only the default branch's copy runs, and it is never a command-line flag, so a PR or a worktree edit cannot make an agent run a command through it. If it is unset or fails, `quipu start` and `quipu done` say so, name your options, and carry on.

### The serial Writer

By default each Issue comment and event queues a Writer run. Actions keeps one pending run per concurrency group, so a busy repository cancels most of them, each pass takes about a minute, and a claim needs two. With `workflows.writer_mode: serial`, one job keeps applying requests in comment order, and events only wake it. As long as a runner is free, each request is answered in about 15 seconds while the job runs. Measured on a hosted runner, `quipu start` (a claim and a start) took 27 to 29 seconds with the job running and 74 seconds when it had to start and install Quipu.

It polls the repository's comments every 3 seconds with an ETag, so the usual answer costs nothing against the rate limit. It sweeps every Issue when a schedule or dispatch starts it, and every 5 minutes while it runs. It stops after 60 seconds idle or 10 minutes, starting another run if requests remain. It also stops when a build is waiting for its runner, and fails on a low rate limit or repeated API errors. Trust is unchanged: it runs in Actions, and receipts still come from `github-actions[bot]`.

What you give up: `issues` and `pull_request` events no longer start it, because the Writer's own work raises them. A hand-closed Issue reopens at the next sweep rather than straight away, within 15 minutes by default. With your own `workflows.schedule`, that schedule sets the wait.

**A second runner.** Those times hold only while a runner is free. If your only runner is building, builds win and claims wait. `quipu runner setup` explains adding a runner only the Writer can use: register it with `--no-default-labels --labels quipu-writer`, then set `workflows.runs_on.writer: [quipu-writer]`. A runner with its default labels would also take your builds.

**Off your machine.** A Writer on a self-hosted runner stops the whole board while that machine sleeps, and one that shares the runner with your builds makes every claim wait behind a build. `quipu doctor` warns about both (`writer runner`). The fix is `workflows.runs_on.writer: [ubuntu-latest]` with `workflows.writer_mode: serial`; the Writer needs no toolchain, only `go install` of the pinned Quipu. On a private repository hosted minutes count against your plan, which is why Quipu recommends this and never sets it for you.

**A slow Writer never fails a merge.** When `quipu done` has merged the PR and the Writer has not answered within `--wait`, it reports `"status": "Merged, receipt pending"` and exits 0 instead of `no-receipt`: the request stays queued, and the agent moves on. When the Writer accepts the evidence and the agent's own Done request never arrives, the Writer closes the Issue itself ten minutes later.

**To migrate**, through a PR you review:

1. Update `workflows.quipu_version` to a version with `quipu writer serve`, and update the `quipu` on your runner's PATH, because a PATH binary wins over the pin.
2. Set `workflows.writer_mode: serial`. `quipu doctor` warns until step 3 is merged.
3. Regenerate with `quipu adopt --apply --replace` and merge the workflow change. It needs your `quipu:owner-approved` label.

Nothing changes in a repository until you do this. To go back, revert that PR.

### The daemon

`quipu daemon` is optional. Run it from your main checkout, and it fills free slots from Ready work by priority, starting `dispatch.agent_command` in each new worktree. It also removes finished worktrees, keeps the queue worker alive, and, below `limits.min_free_disk_gb`, runs `cleanup` on finished worktrees before anything else. It takes work only through `quipu start`, so it never starts an Epic, Decision or Gate.

```yaml
dispatch:
  agent_command: 'claude -p "$QUIPU_PROMPT"'
  max_agents: 2       # agents at once on this clone; limits.build_slots caps builds
  poll_seconds: 60
```

The command runs through `sh` with `QUIPU_ISSUE`, `QUIPU_BRANCH`, `QUIPU_WORKTREE` and `QUIPU_PROMPT` set, and logs to `.git/quipu-daemon-logs/N.log`. It comes only from the default branch, so an agent can't change what runs. If 3 agents in a row fail within a minute, it stops starting more, since the command is probably broken. Fix it and restart the daemon.

It isn't installed as a service. To keep it running, use `launchd` or `systemd`, or simply `nohup quipu daemon >> daemon.log 2>&1 &`. **You give up** choosing what starts when, and each agent spends whatever your command spends, so set `max_agents` with that in mind.

## Day to day

**Planning.** Goals are Epics that you create and approve. Work is a Story, Task, Defect, or Contract under a parent, with a work contract (Outcome, Scope with Includes and Excludes, Acceptance criteria, Required evidence with a Verification boundary, Documentation impact, Source and relationships). Agents suggest work with `quipu propose`, which posts a comment on the parent; turn the ones you want into Issues.

**Making work Ready.** `quipu ready N`. Agents take the highest-priority Ready, unblocked leaf with `quipu start --next`.

**Review.** Agents open PRs that say `Refs #N` and move the work to In review. Review and merge as usual; the `policy` check tells you whether the PR follows the flow. After the merge, the agent records evidence and the Writer closes the Issue.

**Changing Quipu's own rules.** A PR that touches `quipu.yml`, `.github/quipu/`, `.github/workflows/`, `.claude/settings.json`, `.codex/`, or the plugin hooks fails `policy` until you add the `quipu:owner-approved` label. Agents cannot add labels.

**Watching.** `quipu status` shows the board, anything needing attention, and what is waiting on you and why (`owner_queue` with `--json`): PRs to review, Decisions and Gates, Issues to triage, blocked work, and work whose lease expired. Agents leave those and take other work. It does not list proposals, which have no state to tell an open one from a decided one. `quipu validate` lists every rule violation in the live data; the Writer also writes it to each run's summary.

## Options

Set these in `quipu.yml` at the repository root through a reviewed PR (the file is owner-only, and the PR needs `quipu:owner-approved`). Every option that relaxes enforcement is off by default, and takes effect only once merged to the default branch: a copy edited in a worktree cannot turn one on. An unknown key fails validation, so a misspelled option can't silently stay off. `quipu config --json` shows what is in effect.

**Moving from `.github/quipu/config.yaml`.** The old path keeps working. To move, first upgrade every copy of quipu that touches the repository: the one on the runner's PATH, each agent machine, and the plugins. An older quipu doesn't know `quipu.yml`, so it would treat the repository as not using Quipu: its guard would allow every change, and the Writer would stop. Then run `git mv .github/quipu/config.yaml quipu.yml` in a PR with the `quipu:owner-approved` label. Keeping both files is an error.

| Option | Default | Effect |
|---|---|---|
| `ready_by: agents` | `owner` | Agents may run `quipu ready N` to make work Ready. |
| `policy_exempt_authors: [dependabot[bot]]` | none | PRs opened by these bot accounts skip the work checks in `policy`. Changes to Quipu's own files still need your label. Only `...[bot]` accounts are accepted, because your login is shared with your agents. |
| `merge: agent` | `owner` | `quipu done N` merges the agent's PR itself, with a merge commit pinned to the checked head, once `policy` and every `required_checks` entry pass; it then records evidence and closes the Issue. You stop reviewing before merge, so keep `required_checks` meaningful. `gh pr merge` stays blocked for agents. |
| `commit_format: issue-prefix` | none | Commits must read `#NNN type(scope): summary` with a `Refs: #N` footer line. `policy` checks every PR commit and that the PR title is `type(scope): summary`; with `merge: agent`, `quipu done` writes the merge commit the same way. For local checks, call `quipu verify-commit "$1"` from a `commit-msg` hook. When you merge by hand, write the merge subject yourself. |
| `issues_by: agents` | `owner` | Agents may create Stories, Tasks, and Defects under an open Epic with a valid work contract. **You give up** deciding what work exists; you still decide what is Ready unless `ready_by: agents` is on. New Epics stay proposals. |
| `triage_by: agents` | `owner` | Agents may run `quipu triage N` on `quipu:needs-triage` Issues: set kind (if none), Status, priority, and area, add them to the Project, and link a parent. They may also file Decisions and Gates under an open Epic. **You give up** sorting incoming Issues yourself. Ready still needs `ready_by: agents`; agents never decide a Decision or Gate, set Done or Cancelled, or close anything. |
| `update_behind: auto` | `off` | When a PR is behind the default branch, `quipu done` merges the default branch in and pushes, and the Writer does the same for a queued merge. **You give up** seeing a behind PR stop for a person, and the merge commits appear on the work branch. A conflict still stops and goes back to the agent, or for a queued merge returns the Issue to Ready. With `merge_without_ci`, `quipu done` runs `local_check` on the updated head and merges in the same call. If the default branch moved during the check, it runs again, up to three times, then stops with `base-moved`. Needs `merge: agent`. |
| `merge_wait: background` | `foreground` | With complete evidence and only checks still running, `quipu done` returns at once and frees the agent's worktree. The Writer holds the evidence and merges with `QUIPU_WRITER_TOKEN` when the checks pass. **You give up** an agent watching the result: a failed check returns the Issue to Ready with its branch and the reason in a receipt, for whichever agent takes it next. A failed check never merges. A merge waits for the Writer's next run, so a slow schedule means a slow merge, and with `schedule: off` only an Issue event wakes it. A required check that never appears, or a merge GitHub keeps refusing, returns the Issue to Ready after an hour. With strict rulesets, one merge puts other queued PRs behind, so turn on `update_behind` too. Turning the flag off stops the queue. Needs `merge: agent`. |
| `merge_without_ci: true` with `local_check: "<command>"` | off | When required checks are pending, skipped, never ran, or were cancelled before starting, `quipu done` merges anyway, once the worktree is clean and at the PR's head, the policy rules pass there, and `local_check` exits 0. A check that ran and failed still blocks. Needs `merge: agent`, and can't be combined with `merge_wait: background`, which waits for the checks this skips. **You give up** CI before merge: nothing checks other operating systems or a clean machine. The exit code is the client's report, which the Writer cannot verify, and a PR can change what the command runs, such as a Makefile on its branch. The receipt says the merge skipped CI, with the command, SHA, and exit code, worded as reported by `quipu done`, and the boundary is always `local`. |
| `cleanup: "<command>"` | none | Quipu runs this repository's cleanup in a linked worktree after the last step that needs the build: when the Issue reaches Done, and when `quipu worktree remove` takes the worktree away. It runs once per claim, never in the main checkout, and a failure is a warning that doesn't undo the finished work. Agents should no longer run it themselves, so take any such step out of your agent instructions. It's a shell command with your agent's rights, read only from the default branch. **You give up** nothing: without it, nothing is cleaned. |
| `done_grace_minutes: <minutes>` | 60 | For this long after `quipu done` frees a worktree, its holder may still tidy it up without a lease: the `cleanup` command, `cargo clean`, `go clean`, `make clean`, `git clean`, and `rm` of files git does not track. Commits, pushes, and edits stay denied. `0` turns it off; at most 1440. The window is written into the worktree's git directory by `quipu done`, and a new lease ends it. **You give up** nothing that reaches GitHub: the worktree can only lose build output. |
| `auto_ready: true` | off | When the last blocker of a Backlog or Blocked Issue closes, the Writer makes it Ready at its next sweep, unless it is labelled `quipu:needs-owner`. Adopting creates that label; on a repository already adopted, rerun `quipu adopt --apply` or create it by hand. It covers every kind `quipu start` can take, never Decisions or Gates. **You give up** deciding when unblocked work starts; label what you want to hold. |
| `dispatch: {agent_command: "<command>"}` | off | Lets `quipu daemon` start agents. See [the daemon](#the-daemon). |
| `scoped_checks: [{command: "<command>", paths: ["src/**"]}, {paths: ["docs/**"]}]` | off | Chooses what `merge: queue` and `merge_without_ci` run from the paths a change touches. An entry with no `command` says those paths need no check, which is how docs-only changes skip builds. A path no entry names runs the full check, so forgetting a directory costs a build rather than skipping one. Evidence says what ran, what didn't and why. It doesn't filter required checks in your own workflows. **You give up** a full check on every change. |
| `slow_checks: [{workflow: Linux, when: nightly}]` | off | Starts a workflow on the default branch after each batch (`when: batch`, needs `merge: queue`) or when it hasn't run for a day (`when: nightly`, at the Writer's next full sweep). It needs `workflow_dispatch`, and must be in `workflows.defects_from`, so a failure opens a P0 Defect. |
| `workflows.writer_mode: serial` | a run per event | One Writer job applies requests in order and events only wake it, so `quipu start` takes about 30 seconds while the job runs and a runner is free. See [the serial Writer](#the-serial-writer) for what you give up and how to migrate. |
| `merge: queue` with `merge_queue: {check: "<command>"}` | off | `quipu done` validates policy and queues the PR, then returns at once. One local worker per clone takes up to `batch_size` PRs (default 4), waiting at most `max_wait_minutes` (default 30) for a batch to fill. It replays them in order onto the latest default branch in a reusable worktree and runs the check once. On a pass it lands them through GitHub one at a time, each pinned to its head, and compares each merge's tree with the tested one. On a failure it bisects, lands the PRs ahead of the one at fault, and returns only that PR to In review with the failure attached; a conflict returns that PR and the rest carry on. `flaky_retries` (default 1) sets how many more times a failed check runs. A failure followed by a pass lands, flagged in the evidence's risks and in status. If nothing reproduces the failure, or the default branch itself fails, nothing is ejected and the queue holds for you: read `quipu status`, then run `quipu queue resume`. A timeout (`check_timeout_minutes`, default 120) isn't a verdict. The worker restarts by itself from `quipu done`, `start`, and `status`, and a restart neither loses nor merges a PR twice. `quipu protect` turns off "require up to date" in this mode, since each landing leaves the next one behind for a moment. Where a ruleset requires the waived checks, set `merge_bypass_rulesets`; `quipu doctor` says so. **You give up** CI before merge and a person's review: the evidence states the check, its exit, the base, the tree, the runs, and any flakes, as the worker's report, which the Writer can't verify. Only the Writer still closes Issues. |
| `merge_bypass_rulesets: true` | off | With `merge_without_ci` on, `quipu done` retries with the admin bypass when GitHub refuses a merge whose checks it waived, and `quipu protect` adds that bypass to the ruleset. It doesn't retry when any check failed, a review requested changes, or a review thread is unresolved, and the receipt says when the bypass was used. **You give up** GitHub enforcing those checks for agents, who share your login, so a raw merge would not be stopped by GitHub: only the guard denies it. With this off, GitHub's refusal is `merge-blocked`, and `quipu doctor` fails while a ruleset requires the waived checks. |
| `limits: {build_slots: 2, build_areas: [core], min_free_disk_gb: 20}` | none | `quipu start` counts builds across every repository and worktree on this machine, using a local lock so two sessions can't both take the last slot. When short, it returns `Busy` with the reason and claims nothing; `--next` first offers Ready work outside `build_areas`. The disk floor stops every new claim. Work already held always resumes, even over the limit. `local_check` holds a slot while it runs. Leave `build_areas` out to count every area, and each repository's own numbers apply to claims in that repository. A slot clears itself when its worktree or lease goes, its lease lapses, its process dies, or its agent makes no tool call for `slot_idle_minutes` (default 45). A single command running longer than that lets its slot go. Usage is under `machine` in `quipu status --json`. State lives in `~/.quipu/machine`, so agents need to be able to write there. Not enforced on Windows, which has no file locks here. **You give up** some throughput: work waits for a slot. |
| `guard_reads: open` | `closed` | The guard allows more commands it can prove read-only without a lease, such as `git tag --sort`, `git branch --list`, and `nproc`. **You give up** little: it adds only commands that cannot write. Interpreters such as `python`, `perl`, and `awk` stay denied. |
| `guard: deny-list` | `allow-list` (`quipu adopt` writes `deny-list` for new repositories) | The guard refuses only what is dangerous or irreversible: force pushes and pushes to the default branch, merging, approving or closing outside Quipu, another agent's live worktree or branch, Quipu's own enforcement files, the `guard_deny` commands, and changes to a governed worktree's files without a lease (commits, edits, redirects, `rm`/`mv` into it). Commands it has no rule for pass, so reads, interpreters (`python3 -c`), `awk`, loops, `--help`, `docker ps`, `df` and writes outside governed worktrees no longer stop agents. Every denial names the subcommand it refused. **You give up** the guarantee that an unknown command cannot write into a worktree: a build tool or script run without a lease can still change files there, though nothing reaches GitHub without a lease, a PR and `policy`. To migrate, add `guard: deny-list` to `quipu.yml` on the default branch; remove it to go back. |
| `guard_deny: [<command>, ...]` | none | Commands the guard always refuses agents, matched by their leading words, such as `fly deploy`, `npm publish` or `cargo publish`. Applies in both guard modes. **You give up** nothing: it only tightens. |
| `required_checks: [build, unit]` | `[test]` | The CI checks `quipu protect` requires on the default branch, besides `policy`. |
| `workflows` | hosted runners, every five minutes, Defects from `CI` | Runners, pinned Quipu version, schedule, PR trigger, the command that starts a runner, and which failed default-branch workflows become Ready P0 Defects; see [Runners and workflows](#runners-and-workflows) and [operations](operations.md#defects-from-failed-runs). |

`quipu start --next` never picks Epics, Decisions, or Gates: you decide those.

**These stay outside every flag:** changing `quipu.yml`, workflows, hooks, or other enforcement files without your label; merging a PR with a failed check; `gh pr merge`, force pushes, and direct pushes to the default branch; how receipts are trusted; creating Epics; and deciding Decisions or Gates.

**After changing a flag,** run `quipu guide --sync` and commit `.agent/rules/quipu.md`, because the handbook states the flags that are on. `quipu doctor` warns while it is out of date.

## Recovery

| Problem | Fix |
|---|---|
| Requests get no receipt | The Writer is not running. The agent's `no-receipt` error says when it last ran and whether its runner is offline. Requests stay queued, so nothing needs resending. Check the Quipu Writer workflow, its runner, and the `QUIPU_WRITER_TOKEN` secret, then `gh workflow run quipu-writer.yml`. |
| `quipu done` reports `checks-no-runner` | A required check is queued and no online runner has its labels; the error names them. Start that runner, register one with those labels, change the job's `runs-on`, or take the check out of `required_checks`. `quipu doctor` reports the same problem before any PR hits it. |
| `quipu start` or `quipu done` reports a runner down | Its `runner` field says what was tried. Set `workflows.runner_start`, or fix it using `quipu-runner-start.log`. Agents carry on meanwhile; `quipu status` lists the PRs waiting on it. |
| An agent died holding work | Its lease expires within two hours and the work returns to Ready on its own. A new agent in the same worktree resumes it with `quipu start N`. If its branch has an open PR, the work stays with that holder, so CI or Writer downtime can't take it away; `quipu status` lists it as waiting on you. If the agent is really gone, close the PR and the next Writer pass returns the work to Ready. |
| A lease record was left behind in a checkout (work started there by mistake, the branch switched back by hand) | Nothing to do. Once the lease has expired, or GitHub says its Issue is finished or held by someone else, the guard clears the record on the next change there and `quipu status` clears expired ones in every worktree. An agent can clear it at once with `quipu lease release N --json`; a holder gives back its own live lease the same way. Nobody can release someone else's live lease. |
| A PR merged before `quipu done` moved it to review | From the worktree holding the lease, get the ID and fingerprint with `quipu context --issue-number N --json`, then run `quipu pr link` and `quipu evidence submit` with `--apply`. |
| The guard blocks something legitimate | Run the command yourself in a terminal; humans are not guarded. If agents need it routinely, open an Issue against Quipu. |
| A queued merge landed but its evidence was rejected | The receipt on the Issue gives the command. From any checkout: `quipu done N --actor HOLDER --json --criteria ... --commands ... --documentation ... --risks ... --boundary ... --ci-url URL`. |
| `quipu doctor` fails | Each failure prints its fix. |

`quipu writer reconcile --apply` and `quipu writer run --apply` refuse to run outside the Writer workflow, because receipts count only when the workflow posts them (as `github-actions[bot]`). Trigger the workflow instead.

## Limits to know

- **Agents share your GitHub login.** On GitHub, your agents' API calls look like yours. The local guard is what stops an agent from merging, approving, or editing the board; the Writer and the `policy` check stop anything that reaches GitHub off-flow. For the strongest setup, give agents their own GitHub account (a bot user added to `authorized_actors`) so GitHub's own permissions separate you.
- **Hooks need trust.** Claude Code and Codex run project hooks only after a person approves them. An agent session where the hooks were never approved is not guarded locally; GitHub still reverts what it does.
- **Protection needs the right plan.** Without rulesets, GitHub cannot stop direct pushes to `main`.
