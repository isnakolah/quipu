<!-- work:v1 -->

## Outcome

When `quipu done` merges a PR, the cleanup it runs and the cleanup the guard allows afterwards come from the default branch as merged, so a `cleanup` command that lands in that same PR runs, and done says that it ran.

## Scope
Includes:
- After the work reaches Done, done fetches the default branch again and takes `cleanup` from it before releasing the worktree.
- The finished record keeps the cleanup command done resolved, and during done_grace_minutes the guard allows that exact command in the finished worktree even if the clone's copy of the default branch is stale.
- done's result names the cleanup it ran.

Excludes:
- Which commands count as cleanup otherwise, and the length of the grace window.

## Acceptance criteria
- [ ] Test: with `cleanup` absent on the default branch when done starts and present once the PR merges, release runs the merged command and the result names it.
- [ ] Test: during the grace window, the guard allows the recorded cleanup command in the finished worktree while the clone's default-branch copy has no `cleanup`, and denies a different command.
- [ ] Test: after the window closes, the recorded command is denied as before.

## Required evidence
- go test ./... and go vet ./... output
- Verification boundary: CI

## Documentation impact
None: #134 already promises this; this makes it hold when the config arrives with the PR.

## Source and relationships
- Found moving isnakolah/calla onto the new Quipu (2026-11-10): `quipu done 315` merged PR 316, which added `cleanup: cargo xtask cleanup --agent`; done never ran it, and a minute later the guard denied that command in the worktree as no-lease, because done and the guard both read the default branch's config from before the merge.
- Native parent and blocker links are authoritative.

