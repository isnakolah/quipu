# Operations

How Quipu runs a repository, for the people who maintain it. Agents read [AGENTS.md](../AGENTS.md); owners start with the [owner guide](owner-guide.md).

## Enforcement layers

Quipu blocks the off-flow path in three places, so no single gap lets work skip the flow.

| Layer | Where | Stops |
|---|---|---|
| Agent guard | `quipu guard`, run by Claude Code and Codex hooks before every tool call | agents editing without a lease, outside their worktree, or off their branch; pushes other than the leased branch; merges, approvals, labels, Issue and Project edits; edits to `.git`, the config, workflows, and hooks; Quipu admin commands |
| Policy check | `quipu policy pr`, a required check on every PR | PRs that do not reference exactly one leased, In progress or In review leaf Issue with `Refs #N`; closing keywords; wrong base or branch; changes to Quipu's own files without the owner's label |
| Writer and watchdog | `quipu writer reconcile`, in the Quipu Writer workflow | requests from unauthorized or mismatched actors; stale requests; forged receipts; hand-closed Issues, hand-edited board fields, `status:` labels, and Issues created outside the flow |

Branch protection (`quipu protect`) makes the policy and test checks required and closes direct pushes to `main`.

The guard applies to agents only. Humans are checked by the policy check, branch protection, and the watchdog.

## The guard

The guard reads the hook's JSON from standard input, parses shell commands into their parts (pipelines, subshells, `$(...)`, redirects, `cd`, `sh -c`, `eval`), and judges every part in the directory it runs in. Anything it cannot parse is denied. Reading and testing are always allowed. Every other action needs the worktree's lease: a local mirror of the GitHub lease in `<git-dir>/quipu-lease`, checked against GitHub at most every five minutes.

A denial exits 2 with one line: `QUIPU DENIED [code]: why (detail). Do instead: command. Rules: \`quipu guide\`.` The codes and their fixes are generated into AGENTS.md from the same table the guard uses.

## Writer

Every lifecycle change is a request comment that the Writer applies once:

1. An agent or the owner posts a hash-verified request carrying the state fingerprint they saw.
2. The Writer checks the author is in `authorized_actors` and matches the request's actor, the comment is unedited, and the fingerprint is still current.
3. It applies the lifecycle rule (holder checks, transitions, blockers, PR and evidence checks) and updates the Project fields.
4. It answers with a receipt that records the result and a snapshot of the new state.

Receipts are posted with the workflow's `GITHUB_TOKEN`, so they are authored by `github-actions[bot]`. Only those receipts count, plus receipts posted before `receipts_trusted_before` in the config. A receipt-shaped comment from anyone else is ignored. For the same reason, `writer run --apply` and `writer reconcile --apply` refuse to run outside the workflow.

The `QUIPU_WRITER_TOKEN` secret (a classic PAT with `public_repo` and `project`) is used only for Project field changes and reads that the workflow token cannot do. Store it only as an Actions secret; `quipu configure writer-token --apply` reads it from standard input without printing or storing it. Without it, the Writer fails loudly on every event and the policy check fails every PR.

### Rules the Writer applies

- `claim` and `start` are the only way to take work; a plain status request cannot set Claimed or start Claimed work.
- `pr.link`, `evidence.submit`, `status`, and `renew` require the lease holder. `renew` may re-take your own expired lease when nobody else took it.
- `pr.link` needs an open (or, for recovery, merged) PR into the default branch, from the leased branch, with `Refs #N`.
- `evidence.submit` needs that PR merged, the final SHA equal to its merge commit, and the CI URL a successful Actions run of this repository on the PR.
- `Done` needs accepted evidence from the current claim and a synchronized Wiki journal (when `wiki_mode` is `journal`).
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

It runs on every Issue comment and Issue event (both over the Issues updated in the last 30 minutes, because Actions cancels queued runs when many agents post at once), on PR events, every five minutes, and on demand with `gh workflow run quipu-writer.yml`.

## Configuration

`.github/quipu/config.yaml` holds stable identifiers and policy, never status, leases, or tokens:

- `authorized_actors`: GitHub logins allowed to post Writer requests. Change it through a reviewed PR.
- `receipts_trusted_before`: the cutover time before which receipts from the Writer token are trusted. Set once when receipts moved to the workflow token.
- `policy.lease_ttl_minutes`, `policy.reconcile_minutes`, `wiki_mode`, `areas`, `default_branch`, and the Project field IDs.

## Bootstrap and adoption

`quipu init` needs an empty directory. It creates the repository features, Project fields and views, labels, workflows, Issue forms, config, agent guides, and hooks; pushes the first commit; then applies branch protection. `quipu adopt --apply --project-number N` refuses a repository with existing Issues, then writes the config, guides, and hooks. Neither overwrites an existing operating file.

Workflows install `quipu` from source branch `main` until v0 is tagged; release packaging must pin a verified tag.

## Evidence and the journal

Evidence is a Writer request, never a free-form comment. It records the PR, final SHA, CI run, commands, environments, acceptance result, artifacts, documentation impact, risks, and proof boundary, and is kept in the receipt JSON. When `wiki_mode` is `journal`, the Writer appends a generated entry to the Wiki; if that fails, the work stays at Evidence pending and reconciliation retries.

`quipu validate --json` reports every rule violation in the live Issues and Project. The Writer adds it to each run's summary.
