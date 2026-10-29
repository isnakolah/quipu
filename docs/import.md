# Importing existing work

`quipu import --manifest FILE` turns a repository's existing backlog and history into Quipu Issues in one repeatable command. Run it after `quipu adopt`, from a clone of the repository. It is an owner command; agents are blocked from it.

```sh
quipu import --manifest quipu-import/backlog.yaml           # plan: which keys it creates, which exist
quipu import --manifest quipu-import/backlog.yaml --apply   # create, link, and request statuses
gh workflow run quipu-writer.yml                            # or wait for the Writer to pick them up
```

## What it does

1. **Validates the whole manifest** and lists every problem at once. Nothing is created until it is valid.
2. **Creates each Issue**, parents before children, with its `kind:` and `area:` labels, adds it to the Project with Kind, Area, Priority, Proof, and Status Backlog, and records Epic approval.
3. **Links native parents and blockers.**
4. **Asks the Writer to record every status beyond Backlog** with an `import` request. The Writer applies it once, to an Issue that has not entered the flow yet, and posts a receipt:

| Manifest status | Board after the Writer | Issue |
|---|---|---|
| `Backlog` | Backlog (no request) | open |
| `Ready` | Ready | open |
| `Blocked` | Blocked | open |
| `In progress` | Ready, waiting for an agent to resume it with `quipu start N` | open |
| `Done` | Done, with historical evidence | closed as completed |
| `Cancelled` | Cancelled, with the reason | closed as not planned |

Done receipts carry historical evidence: the commits as the final SHA, the source as the artifact, and the proof boundary `local, before Quipu`. Because the Writer posts them, the watchdog treats imported Done and Cancelled Issues like any other finished work and leaves them closed.

`In progress` lands in Ready because no agent holds a lease on imported work. A `branch` from before Quipu is noted in the Issue body; `quipu start N` gives the Issue its own `NNN/short-description` branch, and the agent brings the old commits over.

## Running it again

Every Issue body ends with `<!-- quipu-import:v1 key=KEY -->`. A second run matches Issues by that key and creates nothing twice. It only repairs what is missing: an Issue not on the Project, a parent or blocker link, or a status request that was never posted or was rejected. Fix a rejected request's cause, then run it again.

A large manifest makes several GitHub calls per item. If GitHub's rate limit stops the import partway (`API rate limit already exceeded`), wait for the limit to reset (`gh api rate_limit`) and run the same command again; it picks up where it stopped.

Changing an item's title, body, or status in the manifest after it was imported does nothing; change the Issue through the normal flow instead.

## Manifest schema

YAML, `schema: quipu-import/v1`. Unknown fields are refused, so a typo cannot be silently ignored.

```yaml
schema: quipu-import/v1
source: quipu-import/backlog.yaml      # optional default for each item's source
items:
  - key: CALLA-EPIC-1                  # required, unique, stable: 1-64 of A-Z a-z 0-9 . _ -
    kind: epic                         # required: epic | story | task | defect | contract | decision | gate
    title: Calla v1                    # required
    priority: P1                       # P0 | P1 | P2 | P3 (default P2)
    area: app                          # a configured area (default: the first in the config)
    approved_by: "@owner"              # Epics only; default "@<owner> (import manifest)"
    body: |                            # Epics: free text (default: the title)
      Ship the first release.

  - key: CALLA-12
    kind: story
    title: Sync engine
    parent: CALLA-EPIC-1               # required for everything but an Epic: another item's key
    status: Done                       # Backlog | Ready | Blocked | In progress | Done | Cancelled (default Backlog)
    commits: [0123abc, 4567def]        # Done only: lowercase hex SHAs, 7-40 characters
    source: docs/plan.md:10-40         # where this record came from (default: the manifest's source)

  - key: CALLA-13
    kind: story
    title: Offline mode
    parent: CALLA-EPIC-1
    status: Ready
    blockers: [CALLA-12]               # keys of items that block this one
    body: |                            # open work needs a work:v1 contract
      <!-- work:v1 -->
      ## Outcome
      ...

  - key: CALLA-14
    kind: task
    title: Widget spike
    parent: CALLA-13
    status: Cancelled
    reason: Replaced by the sync engine  # required for Cancelled

  - key: CALLA-15
    kind: task
    title: Widgets
    parent: CALLA-13
    status: In progress
    branch: feature/widgets            # In progress only: the branch the work started on
    body: |
      <!-- work:v1 -->
      ...
```

### Rules

- Every item but an Epic has a `parent` in the manifest; Epics have none. Parents may not form a cycle.
- Open work (any status but Done or Cancelled) that is not an Epic needs a body that satisfies the [work contract](owner-guide.md#day-to-day), starting with `<!-- work:v1 -->`: Outcome, Scope with Includes and Excludes, Acceptance criteria, Required evidence with a Verification boundary, Documentation impact, Source and relationships. Done and Cancelled history needs no contract.
- Done work cannot be blocked by open work.
- `commits` belong to Done work, `branch` to In progress work, and `reason` is required for Cancelled work.
- The repository's area list (`areas` in `.github/quipu/config.yaml`) must include every `area` used.

## Limits

- Import is meant for a freshly adopted repository: `quipu adopt` refuses one that already has Issues, and import never touches Issues without its key marker.
- GitHub limits how fast Issues can be created; import paces itself at about one Issue per second.
- The Writer needs the `QUIPU_WRITER_TOKEN` secret to apply the requests. Until it is set, imported Issues stay in Backlog with their requests pending, and nothing is lost.
