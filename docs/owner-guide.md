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
2. **Bootstrap or adopt.** `quipu init` for a new repository, `quipu adopt --apply --project-number N` for an existing one with no Issues. Both write the config, the agent handbook (`.agent/rules/quipu.md`), and the guard hooks, and create AGENTS.md and CLAUDE.md pointers only if the repository has none. If you already have your own, add a line pointing agents at `.agent/rules/quipu.md` (and `@.agent/rules/quipu.md` in CLAUDE.md); `quipu guide --sync` reminds you. Commit them.
3. **Create the Writer token.** A classic personal access token with `public_repo` and `project` (use `repo` instead of `public_repo` for a private repository). Fine-grained tokens cannot change a personal Project. Set it as an Actions secret, never anywhere else:
   ```sh
   gh secret set QUIPU_WRITER_TOKEN   # paste the token when prompted
   ```
   Without it the Writer cannot claim, review, or finish anything, and the `policy` check fails every PR.
4. **Protect the repository.** `quipu protect` shows the plan; `quipu protect --apply` applies it:
   - `main` takes changes only through PRs that pass the `test` and `policy` checks, from GitHub Actions, with conversations resolved, admins included;
   - no force pushes or deletions of `main`;
   - new branches must be `NNN/short-description` (or Dependabot's).
   Private repositories need a paid plan for protection.
5. **Install the hooks** if `init` or `adopt` did not: `quipu hooks --apply`, then commit `.claude/settings.json` and `.codex/hooks.json`. Each person running agents approves the hooks once (Claude Code: `/hooks`; Codex: trust the project). Install the Claude or Codex plugin from `plugins/` for the session hints too.
6. **Check everything.** `quipu doctor` must print no `FAIL`.

## Day to day

**Planning.** Goals are Epics that you create and approve. Work is a Story, Task, Defect, or Contract under a parent, with a work contract (Outcome, Scope with Includes and Excludes, Acceptance criteria, Required evidence with a Verification boundary, Documentation impact, Source and relationships). Agents suggest work with `quipu propose`, which posts a comment on the parent; turn the ones you want into Issues.

**Making work Ready.** `quipu ready N`. Agents take the highest-priority Ready, unblocked leaf with `quipu start --next`.

**Review.** Agents open PRs that say `Refs #N` and move the work to In review. Review and merge as usual; the `policy` check tells you whether the PR follows the flow. After the merge, the agent records evidence and the Writer closes the Issue.

**Changing Quipu's own rules.** A PR that touches `.github/quipu/`, `.github/workflows/`, `.claude/settings.json`, `.codex/`, or the plugin hooks fails `policy` until you add the `quipu:owner-approved` label. Agents cannot add labels.

**Watching.** `quipu status` shows the board and anything needing attention. `quipu validate` lists every rule violation in the live data; the Writer also writes it to each run's summary.

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
- **Protection needs the right plan.** Without branch protection, GitHub cannot stop direct pushes to `main`.
