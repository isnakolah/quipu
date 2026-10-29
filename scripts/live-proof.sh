#!/bin/sh
set -eu

# Creates only an exact generated private target. It never deletes a
# user-supplied repository. QUIPU_LIVE_PROOF=1 is required because this script
# creates a repository, Project, Issues, branch, PR, and Writer receipts.
if [ "${QUIPU_LIVE_PROOF:-}" != "1" ]; then
  printf '%s\n' 'set QUIPU_LIVE_PROOF=1 to create disposable GitHub proof resources' >&2
  exit 2
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
owner=${QUIPU_LIVE_OWNER:-$(gh api user --jq .login)}
repo="quipu-proof-$(date -u +%Y%m%d%H%M%S)"
project_title="Quipu proof $repo"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/quipu-live-proof.XXXXXX")
target="$scratch/target"
binary="$scratch/quipu"
project_number=""

cleanup() {
  status=$?
  if [ -n "$project_number" ]; then
    gh project delete "$project_number" --owner "$owner" >/dev/null 2>&1 || true
  fi
  if gh repo view "$owner/$repo" >/dev/null 2>&1; then
    gh repo delete "$owner/$repo" --yes >/dev/null 2>&1 || {
      printf '%s\n' "cleanup blocked; private repository retained: $owner/$repo" >&2
      exit 1
    }
  fi
  rm -rf "$scratch"
  exit "$status"
}
trap cleanup EXIT INT TERM

cd "$root"
go build -o "$binary" ./cmd/quipu
mkdir "$target"
(
  cd "$target"
  "$binary" init --owner "$owner" --repo "$repo" --visibility private \
    --project-name "$project_title" --wiki off --areas stable,core --apply --yes
)

project_number=$(gh project list --owner "$owner" --limit 100 --format json --jq ".projects[] | select(.title == \"$project_title\") | .number")
test -n "$project_number"

epic_body="$scratch/epic.md"
work_body="$scratch/work.md"
printf '%s\n' 'Disposable Epic. Owner approval recorded by CLI.' >"$epic_body"
printf '%s\n' '<!-- work:v1 -->' '' '## Outcome' '' 'Exercise Writer lifecycle in disposable repository.' '' '## Scope' 'Includes:' '- One disposable Story.' '' 'Excludes:' '- Production work.' '' '## Acceptance criteria' '- [ ] Lease and completion receipt exist.' '' '## Required evidence' '- Disposable Writer receipts.' '- Verification boundary: provider' '' '## Documentation impact' 'None: disposable integration only.' '' '## Source and relationships' '- US-016 disposable live proof.' '- Native parent and blocker links are authoritative.' >"$work_body"

cd "$target"
epic=$("$binary" issue create --title 'EPIC — Disposable Quipu proof' --kind epic --area stable --approved-by "@$owner" --body-file "$epic_body" --apply)
epic_number=${epic##*/}
story=$("$binary" issue create --title 'US-PROOF — Exercise Writer lifecycle' --kind story --area core --parent "$epic_number" --body-file "$work_body" --apply)
story_number=${story##*/}

context_json=$("$binary" context --issue-number "$story_number" --json)
story_id=$(printf '%s' "$context_json" | jq -r '.issue_id')
fingerprint=$(printf '%s' "$context_json" | jq -r '.state_fingerprint')
"$binary" request status --issue-number "$story_number" --issue-id "$story_id" --fingerprint "$fingerprint" --actor "$owner" --status Ready --apply
"$binary" writer run --issue-number "$story_number" --apply --json | jq -e '.receipts_applied == 1' >/dev/null

context_json=$("$binary" context --issue-number "$story_number" --json)
story_id=$(printf '%s' "$context_json" | jq -r '.issue_id')
fingerprint=$(printf '%s' "$context_json" | jq -r '.state_fingerprint')
branch="$(printf '%03d' "$story_number")/disposable-proof"
branch="$(printf '%03d' "$story_number")/disposable-proof"
# Competing requests observe same canonical state. Writer serializes them: one
# claim succeeds and stale rival receives rejection receipt, never second lease.
"$binary" work claim --issue-number "$story_number" --issue-id "$story_id" --fingerprint "$fingerprint" --actor "$owner" --branch "$branch" --apply
"$binary" work claim --issue-number "$story_number" --issue-id "$story_id" --fingerprint "$fingerprint" --actor "$owner" --branch "$(printf '%03d' "$story_number")/rival-proof" --apply
"$binary" writer run --issue-number "$story_number" --apply --json | jq -e '.receipts_applied == 2' >/dev/null
context_json=$("$binary" context --issue-number "$story_number" --json)
test "$(printf '%s' "$context_json" | jq -r '.status')" = Claimed
test "$(printf '%s' "$context_json" | jq -r '.branch')" = "$branch"
git checkout -b "$branch"
printf '%s\n' 'Disposable Writer proof branch.' >>README.md
git add README.md
git commit -m 'test: exercise disposable Writer proof'
git push --set-upstream origin "$branch"

context_json=$("$binary" context --issue-number "$story_number" --json)
story_id=$(printf '%s' "$context_json" | jq -r '.issue_id')
fingerprint=$(printf '%s' "$context_json" | jq -r '.state_fingerprint')
"$binary" work start --issue-number "$story_number" --issue-id "$story_id" --fingerprint "$fingerprint" --actor "$owner" --apply
"$binary" writer run --issue-number "$story_number" --apply >/dev/null

pr=$(gh pr create --repo "$owner/$repo" --base main --head "$branch" --title 'Disposable Writer proof' --body "Refs #$story_number")
context_json=$("$binary" context --issue-number "$story_number" --json)
story_id=$(printf '%s' "$context_json" | jq -r '.issue_id')
fingerprint=$(printf '%s' "$context_json" | jq -r '.state_fingerprint')
"$binary" pr link --issue-number "$story_number" --issue-id "$story_id" --fingerprint "$fingerprint" --actor "$owner" --pr "$pr" --apply
"$binary" writer run --issue-number "$story_number" --apply >/dev/null
gh pr merge "$pr" --repo "$owner/$repo" --merge --delete-branch
final_sha=$(gh pr view "$pr" --repo "$owner/$repo" --json mergeCommit --jq .mergeCommit.oid)

context_json=$("$binary" context --issue-number "$story_number" --json)
story_id=$(printf '%s' "$context_json" | jq -r '.issue_id')
fingerprint=$(printf '%s' "$context_json" | jq -r '.state_fingerprint')
"$binary" evidence submit --issue-number "$story_number" --issue-id "$story_id" --fingerprint "$fingerprint" --actor "$owner" --pr "$pr" --final-sha "$final_sha" --ci-url "$pr" --commands 'scripts/live-proof.sh' --environments GitHub --criteria passed --artifacts 'None: disposable proof' --documentation 'None: disposable proof' --risks None --boundary provider --apply
"$binary" writer run --issue-number "$story_number" --apply >/dev/null

context_json=$("$binary" context --issue-number "$story_number" --json)
story_id=$(printf '%s' "$context_json" | jq -r '.issue_id')
fingerprint=$(printf '%s' "$context_json" | jq -r '.state_fingerprint')
"$binary" request status --issue-number "$story_number" --issue-id "$story_id" --fingerprint "$fingerprint" --actor "$owner" --status Done --apply
"$binary" writer run --issue-number "$story_number" --apply >/dev/null
test "$(gh issue view "$story_number" --repo "$owner/$repo" --json state --jq .state)" = CLOSED
printf '%s\n' "live Writer proof passed for $owner/$repo; cleanup follows"
