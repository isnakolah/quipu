# Operations

How Quipu runs a repository, for the people who maintain it. Agents read [.agent/rules/quipu.md](../.agent/rules/quipu.md); owners start with the [owner guide](owner-guide.md).

## Enforcement layers

Quipu blocks the off-flow path in three places, so no single gap lets work skip the flow.

| Layer | Where | Stops |
|---|---|---|
| Agent guard | `quipu guard`, run by Claude Code and Codex hooks before every tool call | agents editing without a lease, outside their worktree, or off their branch; pushes other than the leased branch; merges, approvals, labels, Issue and Project edits; edits to `.git`, the config, workflows, and hooks; Quipu admin commands |
| Policy check | `quipu policy pr`, a required check on every PR | PRs that do not reference exactly one leased, In progress or In review leaf Issue with `Refs #N`; closing keywords; wrong base or branch; changes to Quipu's own files without the owner's label |
| Writer and watchdog | `quipu writer reconcile`, in the Quipu Writer workflow | requests from unauthorized or mismatched actors; stale requests; forged receipts; hand-closed Issues, hand-edited board fields, `status:` labels, and Issues created outside the flow |

Two repository rulesets (`quipu protect`) make the policy and test checks required, close direct pushes to `main`, and allow only `NNN/slug` branch names.

The guard applies to agents only. Humans are checked by the policy check, the rulesets, and the watchdog.

## The guard

The guard reads the hook's JSON from standard input, parses shell commands into their parts (pipelines, subshells, `$(...)`, redirects, `cd`, `sh -c`, `eval`), and judges every part in the directory it runs in. Anything it cannot parse is denied. Reading and testing are always allowed. Every other action needs the worktree's lease: a local mirror of the GitHub lease in `<git-dir>/quipu-lease`, checked against GitHub at most every five minutes.

A denial exits 2 with one line: `QUIPU DENIED [code]: why (detail). Do instead: command. Rules: \`quipu guide\`.` The codes and their fixes are generated into `.agent/rules/quipu.md` from the same table the guard uses.

## Writer

Every lifecycle change is a request comment that the Writer applies once:

1. An agent or the owner posts a hash-verified request carrying the state fingerprint they saw.
2. The Writer checks the author is in `authorized_actors` and matches the request's actor, the comment is unedited, and the fingerprint is still current.
3. It applies the lifecycle rule (holder checks, transitions, blockers, PR and evidence checks) and updates the Project fields.
4. It answers with a receipt that records the result and a snapshot of the new state.

Receipts are posted with the workflow's `GITHUB_TOKEN`, so they are authored by `github-actions[bot]`. Only those receipts count, plus receipts posted before `receipts_trusted_before` in the config. A receipt-shaped comment from anyone else is ignored. For the same reason, `writer run --apply` and `writer reconcile --apply` refuse to run outside the workflow.

The `QUIPU_WRITER_TOKEN` secret (a classic PAT with `project` plus `public_repo`, or `repo` for a private repository) is used only for Project field changes and reads that the workflow token cannot do. Store it only as an Actions secret; `quipu configure writer-token --apply` reads it from standard input without printing or storing it. Without it, the Writer fails loudly on every event and the policy check fails every PR.

### Rules the Writer applies

- `claim` and `start` are the only way to take work; a plain status request cannot set Claimed or start Claimed work.
- `pr.link`, `evidence.submit`, `status`, and `renew` require the lease holder. `renew` may re-take your own expired lease when nobody else took it.
- `pr.link` needs an open (or, for recovery, merged) PR into the default branch, from the leased branch, with `Refs #N`.
- `evidence.submit` needs that PR merged, the final SHA equal to its merge commit, and the CI URL a successful Actions run of this repository on the PR.
- `Done` needs accepted evidence from the current claim and a synchronized Wiki journal (when `wiki_mode` is `journal`).
- `import` applies once, to an Issue with no receipt state yet, from an authorized actor; it records the manifest status (In progress waits in Ready) and closes Done and Cancelled work.
- Claimed and In progress leases that expire return to Ready with their branch kept. In review work keeps its holder.

### Watchdog

Each reconcile pass, after applying requests:

- reopens Issues closed without a Writer Done receipt, unless closed before the cutover, closed by an authorized actor as not planned (recorded Cancelled), or an Epic, Decision, or Gate closed by an authorized actor (recorded Done);
- restores Status, Lease holder, Lease expires, and Branch to the snapshot in the last receipt. Issues with no receipts may sit in Backlog, Ready, or Blocked with no lease; anything else goes back to Backlog;
- labels `quipu:needs-triage`, with one explanatory comment, any open Issue that is not in the Project, has no `kind:` label, is a non-Epic without a parent, or is a leaf without a valid work contract; removes the label once fixed;
- removes `status:` labels;
- fills blank Project fields from labels (Status Backlog, Priority P2, Proof Not started) without overwriting set values;
- re-runs the `policy` check of open PRs whose verdict changed with the work under them;
- carries on past an Issue that fails and reports every failure at the end.

It runs on every Issue comment and Issue event, on PR events, every five minutes, and on demand with `gh workflow run quipu-writer.yml`. Event-triggered runs scan every Issue updated since the last successful Writer run began (`--since-last-run`), so nothing is lost when Actions drops queued runs (it keeps one per concurrency group) or a self-hosted runner sleeps for hours; scheduled runs scan every Issue. All reads page past 100 Issues.

## Configuration

`.github/quipu/config.yaml` holds stable identifiers and policy, never status, leases, or tokens:

- `authorized_actors`: GitHub logins allowed to post Writer requests. Change it through a reviewed PR.
- `receipts_trusted_before`: the cutover time before which receipts from the Writer token are trusted. Set once when receipts moved to the workflow token.
- `policy.lease_ttl_minutes`, `policy.reconcile_minutes`, `wiki_mode`, `areas`, `default_branch`, and the Project field IDs.
- `ready_by` (`owner` or `agents`), `policy_exempt_authors` (bot accounts only), and `required_checks` (default `[test]`; `policy` is always required).
- `merge`: `owner` (default) or `agent`. With `agent`, `quipu done` gates on the PR's status check rollup: every required check and `policy` must have a successful result and none may be failing or running (`checks-failing`, `checks-pending`); a draft, behind, or conflicting PR is `merge-blocked`. It merges with `gh pr merge --merge --match-head-commit`, so a push after the checks ran cannot slip in.
- `commit_format`: unset or `issue-prefix` (`#NNN type(scope): summary`, footer `Refs: #N`, merge commits from git exempt). Enforced by `policy` for every PR commit and the PR title, and by `quipu verify-commit FILE` in a `commit-msg` hook.
- `workflows`: runners, pinned Quipu version, schedule, and PR trigger for the generated workflows.

## Bootstrap and adoption

`quipu init` needs an empty directory. It creates the repository, the Project, and everything below, pushes the first commit, then applies the rulesets.

`quipu adopt --apply` works in a clone of an existing repository. Its audit counts Issues, not PRs, and the first adoption refuses a repository that already has Issues. Each step reads what GitHub has and changes only what is missing, so it can run any number of times:

- labels (`kind:`, `area:`, `quipu:needs-triage`, `quipu:owner-approved`), Issues and (for `wiki_mode: journal`) the Wiki;
- the Project fields, Status options, and standard views; options are never rewritten on a Project with items;
- the config, workflows, Issue forms, agent handbook, and hooks; existing files that differ are kept unless `--replace`.

`quipu import --manifest FILE --apply` brings existing work in after adoption: it creates Issues, Project fields, parents, and blockers, and posts an `import` Writer request for every status beyond Backlog. The Writer applies an import request only to an Issue with no receipt state yet; Done closes with historical evidence (boundary `local, before Quipu`), Cancelled closes as not planned. See [import](import.md).

The workflows are rendered from the config's `workflows` section: runner labels per job, the pinned `quipu_version`, the Writer schedule, and whether PR events run the Writer. They install that version with `go install`, unless the runner already has `quipu` on its `PATH`.

## Evidence and the journal

Evidence is a Writer request, never a free-form comment. It records the PR, final SHA, CI run, commands, environments, acceptance result, artifacts, documentation impact, risks, and proof boundary, and is kept in the receipt JSON. When `wiki_mode` is `journal`, the Writer appends a generated entry to the Wiki; if that fails, the work stays at Evidence pending and reconciliation retries.

`quipu validate --json` reports every rule violation in the live Issues and Project. The Writer adds it to each run's summary.
