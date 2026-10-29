#!/usr/bin/env bash
# Disposable proof for Defects from failed runs: a PR for a Story merges and
# breaks CI on main; `quipu writer defect` files exactly one Ready P0
# Defect under the Story's Epic, and running it again files nothing.
# Creates, then deletes, a private repository and a Project.
#
# The Writer runs locally, trusted by the disposable config; in a real
# repository the workflow_run trigger runs the same command.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-defect-proof-$(date +%s)"
visibility=${QUIPU_PROOF_VISIBILITY:-private} # private Actions need billing; public is free
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""

cleanup() {
  status=$?
  if [ -n "$project_number" ]; then
    gh project delete "$project_number" --owner "$owner" >/dev/null 2>&1 || true
  fi
  gh repo delete "$owner/$repo" --yes >/dev/null 2>&1 || printf '%s\n' "cleanup blocked; delete $owner/$repo by hand" >&2
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n== %s\n' "$*"; }
wait_for() { # seconds, description, command...
  local deadline=$(( $(date +%s) + $1 )) what=$2; shift 2
  until "$@"; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "timed out waiting for $what" >&2; exit 1; }
    sleep 5
  done
}

quipu=${QUIPU_BIN:-$work/quipu}
[ -n "${QUIPU_BIN:-}" ] || (cd "$source_dir" && go build -o "$quipu" ./cmd/quipu)
pin=${QUIPU_PIN:-$(git -C "$source_dir" rev-parse HEAD)}

step "adopted $visibility repository $owner/$repo"
gh repo create "$owner/$repo" "--$visibility" --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app,infra >/dev/null
printf '%s\n' 'receipts_trusted_before: "2100-01-01T00:00:00Z"' >>.github/quipu/config.yaml
rm .github/workflows/quipu-writer.yml .github/workflows/quipu-policy.yml
cat >.github/workflows/ci.yml <<'YAML'
name: CI
on:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: test ! -f broken
YAML
git add -A && git commit -qm "chore(quipu): adopt Quipu" && git push -q origin main

contract='      <!-- work:v1 -->
      ## Outcome
      Proof work.
      ## Scope
      Includes:
      - The proof.
      Excludes:
      - Everything else.
      ## Acceptance criteria
      - [ ] It breaks main.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.'
cat >"$work/backlog.yaml" <<YAML
schema: quipu-import/v1
items:
  - key: EPIC-1
    kind: epic
    title: Disposable defect proof
  - key: STORY-1
    kind: story
    title: Change that breaks main
    parent: EPIC-1
    area: infra
    status: Ready
    body: |
$contract
YAML
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
"$quipu" writer reconcile --apply >/dev/null
key() { gh api "repos/$owner/$repo/issues?state=all" --jq ".[] | select(.body // \"\" | contains(\"key=$1 -->\")) | .number"; }
epic=$(key EPIC-1); story=$(key STORY-1)
echo "Epic #$epic, Story #$story"

step "a PR for #$story merges and breaks CI on main"
branch=$(printf '%03d/break-main' "$story")
git switch -q -c "$branch"
touch broken && git add broken && git commit -qm "feat: break main" && git push -q -u origin "$branch"
pr=$(gh pr create --base main --head "$branch" --title "feat: break main" --body "Refs #$story")
gh pr merge "$pr" --merge >/dev/null
sha=$(gh pr view "$pr" --json mergeCommit --jq .mergeCommit.oid)
run_done() { gh run list --repo "$owner/$repo" --workflow CI --branch main --json headSha,status --jq ".[] | select(.headSha == \"$sha\") | .status" | grep -qx completed; }
wait_for 600 "CI on $sha" run_done
run=$(gh run list --repo "$owner/$repo" --workflow CI --branch main --json databaseId,headSha,conclusion --jq ".[] | select(.headSha == \"$sha\") | \"\(.databaseId) \(.conclusion)\"")
echo "run $run"
run_id=${run%% *}
[ "${run#* }" = failure ]

step "the Writer files one Ready P0 Defect"
"$quipu" writer defect --run-id "$run_id" --json | tee "$work/plan.json"
jq -e ".issue == $story and .epic == $epic" "$work/plan.json" >/dev/null
"$quipu" writer defect --run-id "$run_id" --apply --json | tee "$work/defect.json"
jq -e '.status == "Ready"' "$work/defect.json" >/dev/null
defect=$(jq -r .defect "$work/defect.json")
gh issue view "$defect" --repo "$owner/$repo" --json title,state,labels --jq '{title, state, labels: [.labels[].name]}'
[ "$(gh issue view "$defect" --repo "$owner/$repo" --json labels --jq '[.labels[].name] | sort | join(",")')" = "area:infra,kind:defect" ]
parent=$(gh api graphql -f query="{repository(owner:\"$owner\",name:\"$repo\"){issue(number:$defect){parent{number}}}}" --jq .data.repository.issue.parent.number)
[ "$parent" = "$epic" ]
fields=$(gh project item-list "$project_number" --owner "$owner" --format json --jq ".items[] | select(.content.number == $defect) | \"\(.status) \(.priority)\"")
echo "board: $fields"
[ "$fields" = "Ready P0" ]

step "the same run files nothing more"
"$quipu" writer defect --run-id "$run_id" --apply --json | tee "$work/again.json"
jq -e ".defect == $defect and (.skipped | startswith(\"run already has Defect\"))" "$work/again.json" >/dev/null
count=$(gh api "repos/$owner/$repo/issues?state=all&per_page=100" --jq "[.[] | select(.body // \"\" | contains(\"quipu-defect:v1 run=$run_id \"))] | length")
[ "$count" = 1 ]

step "the watchdog accepts the Defect as it is"
"$quipu" writer reconcile --apply --json | tee "$work/watchdog.json"
jq -e '.drift_restored == 0 and .issues_triaged == 0 and .issues_reopened == 0' "$work/watchdog.json" >/dev/null
"$quipu" status --json | jq -e ".ready[0].number == $defect and .ready[0].priority == \"P0\"" >/dev/null
echo "Defect #$defect heads the Ready queue"

printf '\n%s\n' "defect proof passed for $owner/$repo; cleanup follows"
