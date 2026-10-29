# Operations

Store `QUIPU_WRITER_TOKEN` only as an Actions secret. For a personal Project V2, GitHub currently requires a dedicated classic PAT with `public_repo` and `project`; fine-grained PATs cannot mutate that Project. Classic PATs cannot be restricted to one public repository, so use a dedicated token with a short expiry, rotate and revoke it promptly, and never place it in config, cache, logs, request comments, or shell history. `quipu configure writer-token --apply` reads it from standard input without printing or persisting it locally.

For local commands, `quipu` uses `GITHUB_TOKEN` when supplied; otherwise it reads the active `gh auth token` credential in memory for that invocation. It never writes either credential to repository config or cache.

`authorized_actors` in `.github/quipu/config.yaml` is stable Writer authorization, initially repository owner only. Add a GitHub login through reviewed source change before that person or agent may submit a Writer request. Public Issue comments alone never authorize a mutation.

All changing commands require `--apply`; bootstrap requires `--yes`. `quipu context` is first command for agents. Claim leaf work before branch/edit. Submit evidence with final SHA, PR/CI URLs, commands, environments, acceptance result, artifacts, documentation, risks, and proof boundary. A merged PR alone is not Done.

`quipu init` requires an empty local directory. It creates repository features, Project fields, stable labels, source-controlled operating files, `.github/quipu/config.yaml`, then makes and pushes bootstrap commit. It does not silently adopt an existing repository. `quipu adopt --apply --project-number N` first audits for existing Issues, refuses to reclassify them, and records only an already-complete Project contract.

It also installs Writer, policy, and Go CI workflows, source-controlled Epic, Work, Decision, and routed Issue forms plus compact Quipu agent rule. Writer and policy install canonical `quipu` from source branch `main` until v0 is tagged; release packaging must replace that source ref with a verified tag. Bootstrap refuses to overwrite any installed operating file.

`quipu validate --json` reads live GitHub Issues and Project fields. It reports every detected live-work violation: missing parent/contract/Project Status/Kind/Area, parent or blocker cycle, unresolved blocker in Ready/Claimed/In progress/Done, Epic branch ownership, field-label mismatch, and a Done parent with unfinished child. Closed historical Issues remain relationship context but do not require current Project classification.

Use `quipu context --issue-number N --json` to obtain Issue node ID and current state fingerprint. The Writer rejects a changed fingerprint. Claims also require `--branch NNN/short-description`; leases use a two-hour maximum. On an Issue-comment event, active Writer runs fingerprint verification, lifecycle validation, Project field update, then receipt emission. It never activates without `QUIPU_WRITER_TOKEN`.

`quipu writer reconcile --apply` scans every open Issue, replays unreceipted requests, and returns expired Claimed/In progress leases to Ready while retaining branch context. Scheduled Writer runs invoke it every five minutes. Reconciliation writes an expiry receipt only once.

On first active reconciliation, Writer fills blank Project Status (`Backlog`), Kind/Area from stable labels, Priority (`P2`), and Proof (`Not started`) without overwriting populated values. It adds `area:stable` only where legacy bootstrap Epics have no Area label, preserving field-label agreement.

Evidence is a Writer request, never a free-form completion comment. `quipu evidence submit` requires `--pr`, final SHA, CI URL, exact commands, environments, criteria result, artifacts (`None: reason` allowed), documentation impact, residual risks (`None` allowed), and proof boundary. Valid evidence moves only In review work to Evidence pending and is preserved in Writer receipt JSON.

`quipu pr link --issue-number N --issue-id ID --fingerprint SHA --pr URL --apply` records an open canonical PR request and moves only In progress work to In review. Evidence re-reads its PR and requires merged state plus exact merge-commit SHA match before any evidence state mutation.

Wiki journal is generated from receipts and is noncanonical. If Wiki write fails, Writer leaves work at Evidence pending and reconciliation retries.
