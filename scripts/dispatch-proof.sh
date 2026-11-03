#!/usr/bin/env bash
# Disposable proof of parking, auto_ready and the daemon against real GitHub: an agent
# parks on a Decision and its lease goes; a non-Decision is refused; closing the
# Decision makes the work Ready; and quipu daemon --once takes it and starts the
# configured command, never the Decision or the Gate. Creates, then deletes, a private
# repository and a Project. The Writer runs locally in a loop, trusted by the
# disposable config.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-dispatch-proof-$(date +%s)"
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""
writer_pid=""
export QUIPU_STATE_DIR="$work/machine"

cleanup() {
  status=$?
  [ -z "$writer_pid" ] || kill "$writer_pid" 2>/dev/null || true
  if [ "$status" -ne 0 ] && [ -f "$work/writer.log" ]; then
    printf '%s\n' "last of the Writer's output:" >&2
    tail -5 "$work/writer.log" | cut -c1-300 >&2
  fi
  if [ -n "$project_number" ]; then
    gh project delete "$project_number" --owner "$owner" >/dev/null 2>&1 || printf '%s\n' "cleanup blocked; delete Project $project_number of $owner by hand" >&2
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
    sleep 10
  done
}

points=$(gh api rate_limit --jq .resources.graphql.remaining)
[ "$points" -ge 1500 ] || { echo "only $points GraphQL points left this hour; this proof needs about 1500" >&2; exit 1; }
quipu=$work/quipu
(cd "$source_dir" && go build -o "$quipu" ./cmd/quipu)
pin=${QUIPU_PIN:-$(git -C "$source_dir" rev-parse HEAD)}

step "adopted private repository $owner/$repo with auto_ready and dispatch"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app >/dev/null
cat >>quipu.yml <<YAML
receipts_trusted_before: "2100-01-01T00:00:00Z"
auto_ready: true
dispatch:
  agent_command: 'echo "\$QUIPU_ISSUE \$QUIPU_BRANCH" >> $work/agents'
  max_agents: 1
YAML
rm .github/workflows/quipu-writer.yml .github/workflows/quipu-policy.yml
git add -A && git commit -qm "chore(quipu): adopt Quipu" && git push -q origin main

story_body='      <!-- work:v1 -->
      ## Outcome
      Proof work.
      ## Scope
      Includes:
      - The proof.
      Excludes:
      - Everything else.
      ## Acceptance criteria
      - [ ] It moves.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.'
{
  printf 'schema: quipu-import/v1\nitems:\n  - key: EPIC-1\n    kind: epic\n    title: Disposable dispatch proof\n'
  for key in A B; do
    printf '  - key: STORY-%s\n    kind: story\n    title: Change %s\n    parent: EPIC-1\n    status: Ready\n    body: |\n%s\n' "$key" "$key" "$story_body"
  done
  printf '  - key: DECISION-1\n    kind: decision\n    title: Which way\n    parent: EPIC-1\n    status: Ready\n    body: |\n%s\n' "$story_body"
  printf '  - key: GATE-1\n    kind: gate\n    title: Ship it\n    parent: EPIC-1\n    status: Ready\n    body: |\n%s\n' "$story_body"
} >"$work/backlog.yaml"
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
item() { gh api "repos/$owner/$repo/issues?state=all" --jq ".[] | select(.body // \"\" | contains(\"key=$1 -->\")) | .number"; }
a=$(item STORY-A) b=$(item STORY-B) d=$(item DECISION-1) g=$(item GATE-1)

( while :; do "$quipu" writer reconcile --apply >>"$work/writer.log" 2>&1 || true; sleep 15; done ) &
writer_pid=$!
status_of() { "$quipu" context --issue-number "$1" --json | jq -r .status; }
is_status() { [ "$(status_of "$1")" = "$2" ]; }
for number in "$a" "$b"; do wait_for 240 "#$number Ready" is_status "$number" Ready; done

step "parking refuses anything but a Decision"
wt=$("$quipu" start "$a" --worktree --json 2>/dev/null | jq -r .worktree)
case "$wt" in /*) ;; *) wt="$work/repo/$wt" ;; esac
if (cd "$wt" && "$quipu" park "$a" --on "$b" --json >"$work/refused" 2>&1); then echo "parking on a Story must fail" >&2; exit 1; fi
grep 'not a Decision' "$work/refused" >/dev/null || { cut -c1-300 "$work/refused" >&2; exit 1; }
is_status "$a" "In progress"
echo "refused: not a Decision"

step "parking on a Decision frees the lease and shows in the digest"
(cd "$wt" && "$quipu" park "$a" --on "$d" --json 2>/dev/null) | jq -c '{issue, status}'
is_status "$a" Blocked
[ ! -f "$(git -C "$wt" rev-parse --absolute-git-dir)/quipu-lease" ] || { echo "the lease is still there" >&2; exit 1; }
"$quipu" context --issue-number "$a" --json | jq -e '(.holder // .lease_holder // "") == ""' >/dev/null
"$quipu" status --json | jq -c --argjson a "$a" '[.owner_queue[] | select(.number == $a) | .reason]' | grep 'parked, waiting on' >/dev/null || { echo "parked work is missing from the digest" >&2; exit 1; }
echo "blocked, lease gone, in owner_queue as parked"

step "closing the Decision makes the work Ready by itself"
gh issue close "$d" --repo "$owner/$repo" --comment "Decided." >/dev/null
wait_for 240 "#$a Ready" is_status "$a" Ready
gh api "repos/$owner/$repo/issues/$a/comments" --jq '.[].body' | grep 'last blocker closed' >/dev/null || { echo "no receipt for the move" >&2; exit 1; }
echo "Ready, with a receipt"

step "the daemon starts the top story and its command, never the Gate"
"$quipu" worktree remove "$wt" --json | jq -c '{status}'
"$quipu" daemon --once --json 2>/dev/null | jq -c '{started, waiting, warnings}'
read_agent() { [ -s "$work/agents" ]; }
wait_for 30 "the agent command" read_agent
cut -c1-120 "$work/agents"
[ "$(wc -l <"$work/agents" | tr -d ' ')" = 1 ] || { echo "max_agents is 1" >&2; exit 1; }
started=$(cut -d' ' -f1 "$work/agents")
[ "$started" = "$a" ] || [ "$started" = "$b" ] || { echo "it started #$started" >&2; exit 1; }
is_status "$g" Ready
is_status "$started" "In progress"

printf '\n%s\n' "dispatch proof passed for $owner/$repo; cleanup follows"
