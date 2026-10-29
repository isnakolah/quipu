#!/usr/bin/env bash
# Disposable proof for `quipu import`: a manifest becomes Issues, the
# Writer records each status, importing again changes nothing, and the
# watchdog leaves imported Done and Cancelled work closed.
# Creates, then deletes, a private repository and a Project.
#
# The Writer runs locally here, so its receipts are authored by you, not
# github-actions[bot]; the disposable config trusts them. Unit tests cover
# that only the bot's receipts count.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-import-proof-$(date +%s)"
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
check() { jq -e "$1" "$2" >/dev/null || { printf 'check failed: %s on %s\n' "$1" "$2" >&2; cat "$2" >&2; exit 1; }; }

quipu=${QUIPU_BIN:-$work/quipu}
[ -n "${QUIPU_BIN:-}" ] || (cd "$source_dir" && go build -o "$quipu" ./cmd/quipu)
pin=${QUIPU_PIN:-$(git -C "$source_dir" rev-parse HEAD)}

step "adopted private repository $owner/$repo"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json \
  --quipu-version "$pin" --areas app,infra >"$work/adopt.json"
check '.changed == true' "$work/adopt.json"
printf '%s\n' 'receipts_trusted_before: "2100-01-01T00:00:00Z"' >>.github/quipu/config.yaml

contract='      <!-- work:v1 -->
      ## Outcome
      Proof work.
      ## Scope
      Includes:
      - The proof.
      Excludes:
      - Everything else.
      ## Acceptance criteria
      - [ ] It imports.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.'
cat >"$work/backlog.yaml" <<YAML
schema: quipu-import/v1
source: quipu-import/backlog.yaml
items:
  - key: EPIC-1
    kind: epic
    title: Disposable import proof
    priority: P1
  - key: STORY-1
    kind: story
    title: Sync engine (finished before Quipu)
    parent: EPIC-1
    status: Done
    commits: [$(git rev-parse HEAD)]
    source: docs/plan.md:10-40
  - key: STORY-2
    kind: story
    title: Offline mode
    parent: EPIC-1
    status: Ready
    blockers: [STORY-1]
    body: |
$contract
  - key: TASK-3
    kind: task
    title: Old spike
    parent: STORY-2
    status: Cancelled
    reason: Replaced by the sync engine
  - key: TASK-4
    kind: task
    title: Widgets
    parent: STORY-2
    status: In progress
    branch: feature/widgets
    area: infra
    body: |
$contract
  - key: TASK-5
    kind: task
    title: Later
    parent: STORY-2
    body: |
$contract
YAML

step "plan"
"$quipu" import --manifest "$work/backlog.yaml" --json | tee "$work/plan.json"
check '(.create | length) == 6' "$work/plan.json"

step "import"
"$quipu" import --manifest "$work/backlog.yaml" --apply --json | tee "$work/import.json"
check '.changed == true and (.import_requests | length) == 4' "$work/import.json"

step "the Writer records the statuses"
"$quipu" writer reconcile --apply --json | tee "$work/writer.json"
check '.requests_receipted == 4 and (.errors // [] | length) == 0' "$work/writer.json"

number() { gh issue list --repo "$owner/$repo" --state all --search "\"key=$1\" in:body" --json number --jq '.[0].number'; }
issue() { gh issue view "$(number "$1")" --repo "$owner/$repo" --json state,stateReason --jq '.state + " " + .stateReason'; }
board() { gh project item-list "$project_number" --owner "$owner" --format json --jq ".items[] | select(.content.number == $(number "$1")) | .status"; }
for expected in "STORY-1|CLOSED COMPLETED|Done" "TASK-3|CLOSED NOT_PLANNED|Cancelled" "STORY-2|OPEN |Ready" "TASK-4|OPEN |Ready" "TASK-5|OPEN |Backlog" "EPIC-1|OPEN |Backlog"; do
  IFS='|' read -r key want_issue want_board <<<"$expected"
  got_issue=$(issue "$key"); got_board=$(board "$key")
  printf '%-8s issue=%-18s board=%s\n' "$key" "$got_issue" "$got_board"
  [ "$got_issue" = "$want_issue" ] && [ "$got_board" = "$want_board" ] || { echo "expected $want_issue / $want_board" >&2; exit 1; }
done
gh issue view "$(number STORY-1)" --repo "$owner/$repo" --comments | grep -q 'local, before Quipu'

step "importing again changes nothing"
"$quipu" import --manifest "$work/backlog.yaml" --apply --json | tee "$work/again.json"
check '.changed == false and ((.import_requests // []) | length) == 0' "$work/again.json"

step "the watchdog leaves imported work alone"
"$quipu" writer reconcile --apply --json | tee "$work/watchdog.json"
check '.issues_reopened == 0 and .drift_restored == 0 and .requests_receipted == 0' "$work/watchdog.json"
[ "$(issue STORY-1)" = "CLOSED COMPLETED" ] && [ "$(issue TASK-3)" = "CLOSED NOT_PLANNED" ]

printf '\n%s\n' "import proof passed for $owner/$repo; cleanup follows"
