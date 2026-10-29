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
required_checks: [test]             # CI checks main requires besides policy
```

- A runner that already has `quipu` on its `PATH` uses it and skips installing Go and Quipu. Keep it at `quipu_version`.
- Without the schedule, expired leases are reclaimed and PR policy rechecked only when an Issue event or a manual run (`gh workflow run quipu-writer.yml`) wakes the Writer.
- Self-hosted runners: register one under the repository's **Settings → Actions → Runners** with the labels you list, and keep it online. `policy` runs on `pull_request_target`, which checks out only the default branch, so a PR's code never runs on your runner through Quipu's workflows.

## Day to day

**Planning.** Goals are Epics that you create and approve. Work is a Story, Task, Defect, or Contract under a parent, with a work contract (Outcome, Scope with Includes and Excludes, Acceptance criteria, Required evidence with a Verification boundary, Documentation impact, Source and relationships). Agents suggest work with `quipu propose`, which posts a comment on the parent; turn the ones you want into Issues.

**Making work Ready.** `quipu ready N`. Agents take the highest-priority Ready, unblocked leaf with `quipu start --next`.

**Review.** Agents open PRs that say `Refs #N` and move the work to In review. Review and merge as usual; the `policy` check tells you whether the PR follows the flow. After the merge, the agent records evidence and the Writer closes the Issue.

**Changing Quipu's own rules.** A PR that touches `.github/quipu/`, `.github/workflows/`, `.claude/settings.json`, `.codex/`, or the plugin hooks fails `policy` until you add the `quipu:owner-approved` label. Agents cannot add labels.

**Watching.** `quipu status` shows the board and anything needing attention. `quipu validate` lists every rule violation in the live data; the Writer also writes it to each run's summary.

## Options

Set these in `.github/quipu/config.yaml` through a reviewed PR (the file is owner-only, and the PR needs `quipu:owner-approved`). Every option that relaxes enforcement is off by default.

| Option | Default | Effect |
|---|---|---|
| `ready_by: agents` | `owner` | Agents may run `quipu ready N` to make work Ready. |
| `policy_exempt_authors: [dependabot[bot]]` | none | PRs opened by these bot accounts skip the work checks in `policy`. Changes to Quipu's own files still need your label. Only `...[bot]` accounts are accepted, because your login is shared with your agents. |
| `required_checks: [build, unit]` | `[test]` | The CI checks `quipu protect` requires on the default branch, besides `policy`. |
| `workflows` | hosted runners, every five minutes | Runners, pinned Quipu version, schedule, and PR trigger; see [Runners and workflows](#runners-and-workflows). |

`quipu start --next` never picks Epics, Decisions, or Gates: you decide those.

## Recovery

| Problem | Fix |
|---|---|
| Requests get no receipt | The Writer is not running. Check the Quipu Writer workflow and the `QUIPU_WRITER_TOKEN` secret, then `gh workflow run quipu-writer.yml`. |
| An agent died holding work | Its lease expires within two hours and the work returns to Ready on its own. A new agent in the same worktree resumes it with `quipu start N`. |
| A PR merged before `quipu done` moved it to review | From the worktree holding the lease, get the ID and fingerprint with `quipu context --issue-number N --json`, then run `quipu pr link` and `quipu evidence submit` with `--apply`. |
| The guard blocks something legitimate | Run the command yourself in a terminal; humans are not guarded. If agents need it routinely, open an Issue against Quipu. |
| `quipu doctor` fails | Each failure prints its fix. |

`quipu writer reconcile --apply` and `quipu writer run --apply` refuse to run outside the Writer workflow, because receipts count only when the workflow posts them (as `github-actions[bot]`). Trigger the workflow instead.

## Limits to know

- **Agents share your GitHub login.** On GitHub, your agents' API calls look like yours. The local guard is what stops an agent from merging, approving, or editing the board; the Writer and the `policy` check stop anything that reaches GitHub off-flow. For the strongest setup, give agents their own GitHub account (a bot user added to `authorized_actors`) so GitHub's own permissions separate you.
- **Hooks need trust.** Claude Code and Codex run project hooks only after a person approves them. An agent session where the hooks were never approved is not guarded locally; GitHub still reverts what it does.
- **Protection needs the right plan.** Without rulesets, GitHub cannot stop direct pushes to `main`.
