#!/usr/bin/env bash
# Disposable proof of merge: queue: a clean batch lands with one check and [skip ci]
# on all but its last merge, a bad PR goes back alone and is taken over from a new
# worktree, and a worker killed during a check neither loses nor double-merges a PR.
# Where GitHub allows rulesets on this repository, it lands under one too. Creates,
# then deletes, a private repository and a Project. The Writer runs locally in a loop,
# trusted by the disposable config.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-queue-proof-$(date +%s)"
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""
writer_pid=""
export QUIPU_STATE_DIR="$work/machine"

cleanup() {
  status=$?
  [ -z "$writer_pid" ] || kill "$writer_pid" 2>/dev/null || true
  pkill -f "$work/quipu queue run" 2>/dev/null || true
  if [ "$status" -ne 0 ]; then
    [ ! -f "$work/writer.log" ] || { printf '%s\n' "last of the Writer's output:" >&2; tail -8 "$work/writer.log" >&2; }
    [ ! -f "$work/repo/.git/quipu-queue.log" ] || { printf '%s\n' "the worker's output:" >&2; tail -30 "$work/repo/.git/quipu-queue.log" >&2; }
    [ ! -f "$work/repo/.git/quipu-queue.json" ] || { printf '%s\n' "the journal:" >&2; cat "$work/repo/.git/quipu-queue.json" >&2; }
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
    sleep 20
  done
}

points=$(gh api rate_limit --jq .resources.graphql.remaining)
[ "$points" -ge 3500 ] || { echo "only $points GraphQL points left this hour; this proof needs about 3500" >&2; exit 1; }
quipu=$work/quipu
(cd "$source_dir" && go build -o "$quipu" ./cmd/quipu)
pin=${QUIPU_PIN:-$(git -C "$source_dir" rev-parse HEAD)}

step "adopted private repository $owner/$repo with merge: queue"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app >/dev/null
cat >>quipu.yml <<YAML
receipts_trusted_before: "2100-01-01T00:00:00Z"
merge: queue
merge_bypass_rulesets: true
merge_queue:
  batch_size: 2
  check: 'echo run >> $work/check-runs; test ! -f bad.txt && { test ! -f slow.txt || sleep 40; }'
YAML
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
      - run: "true"
YAML
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
      - [ ] It lands as tested.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.'
{
  printf 'schema: quipu-import/v1\nitems:\n  - key: EPIC-1\n    kind: epic\n    title: Disposable queue proof\n'
  for key in A B C D E; do
    printf '  - key: STORY-%s\n    kind: story\n    title: Change %s\n    parent: EPIC-1\n    status: Ready\n    body: |\n%s\n' "$key" "$key" "$story_body"
  done
} >"$work/backlog.yaml"
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
story() { gh api "repos/$owner/$repo/issues?state=all" --jq ".[] | select(.body // \"\" | contains(\"key=STORY-$1 -->\")) | .number"; }
a=$(story A) b=$(story B) c=$(story C) d=$(story D) e=$(story E)

# Windowed and unhurried: 5,000 GraphQL points an hour are all this login gets, and
# a pass over every Issue every few seconds spends them in minutes.
( while :; do "$quipu" writer reconcile --since 15m --apply >>"$work/writer.log" 2>&1 || true; sleep 5; done ) &
writer_pid=$!
status_of() { "$quipu" context --issue-number "$1" --json | jq -r .status; }
ready() { [ "$(status_of "$1")" = Ready ]; }
done_now() { [ "$(status_of "$1")" = Done ]; }
for number in "$a" "$b" "$c" "$d" "$e"; do wait_for 180 "#$number Ready" ready "$number"; done

evidence=(--criteria "lands as tested" --commands "scripts/queue-proof.sh" --documentation "None: disposable proof" --risks None --boundary local)

# queue_pr NUMBER FILE: claim in a new worktree, open a PR, and queue it. It must
# return at once, with no check run.
queue_pr() {
  wt=$("$quipu" start "$1" --worktree --json | jq -r .worktree)
  case "$wt" in /*) ;; *) wt="$work/repo/$wt" ;; esac
  printf 'x\n' >"$wt/$2"
  git -C "$wt" add -A
  git -C "$wt" commit -qm "feat(proof): change for $1"
  git -C "$wt" push -q -u origin HEAD
  (cd "$wt" && gh pr create --base main --head "$(git branch --show-current)" --title "feat(proof): change for $1" --body "Refs #$1" >/dev/null)
  began=$(date +%s)
  (cd "$wt" && "$quipu" done "$1" --json "${evidence[@]}") | jq -c '{issue, status}'
  [ $(( $(date +%s) - began )) -lt 30 ] || { echo "done must not wait for the batch" >&2; exit 1; }
  [ ! -f "$(git -C "$wt" rev-parse --absolute-git-dir)/quipu-lease" ]
  eval "wt_$1=\$wt"
}
runs() { wc -l <"$work/check-runs" | tr -d ' '; }

step "a clean batch of two runs the check once and lands both"
: >"$work/check-runs"
queue_pr "$a" a.txt
queue_pr "$b" b.txt
wait_for 300 "#$a Done" done_now "$a"
wait_for 300 "#$b Done" done_now "$b"
[ "$(runs)" = 1 ] || { echo "runs: $(runs)" >&2; exit 1; }
pr_commit() { gh pr list --repo "$owner/$repo" --state merged --json number,body,mergeCommit --jq ".[] | select(.body | contains(\"Refs #$1\")) | .mergeCommit.oid"; }
sha_a=$(pr_commit "$a") sha_b=$(pr_commit "$b")
git fetch -q origin main
[ "$(git rev-parse "$sha_b^1")" = "$sha_a" ] || { echo "the second must land on the first" >&2; exit 1; }
gh api "repos/$owner/$repo/commits/$sha_a" --jq .commit.message | grep -qF '[skip ci]'
if gh api "repos/$owner/$repo/commits/$sha_b" --jq .commit.message | grep -qF '[skip ci]'; then echo "the last merge must run CI" >&2; exit 1; fi
gh api "repos/$owner/$repo/issues/$a/comments" --jq '.[].body' | grep -q 'merge queue at' || { echo "the evidence must say what ran" >&2; exit 1; }

step "GitHub honours [skip ci] on the first merge and runs CI on the last"
ci_ran() { [ -n "$(gh run list --repo "$owner/$repo" --workflow ci.yml --commit "$sha_b" --json databaseId --jq '.[0].databaseId')" ]; }
wait_for 120 "CI on the last merge" ci_ran
[ -z "$(gh run list --repo "$owner/$repo" --workflow ci.yml --commit "$sha_a" --json databaseId --jq '.[0].databaseId')" ] || { echo "CI ran on a [skip ci] merge" >&2; exit 1; }
echo "CI ran on $sha_b and not on $sha_a"

step "a bad PR goes back alone, and the good one lands"
: >"$work/check-runs"
queue_pr "$c" bad.txt
queue_pr "$d" d.txt
wait_for 300 "#$d Done" done_now "$d"
returned() { "$quipu" status --json | jq -e --argjson n "$c" '.queue.returned | map(.issue) | index($n) != null' >/dev/null; }
wait_for 120 "#$c returned" returned
[ "$(status_of "$c")" = "In review" ]
echo "check runs: $(runs)"
"$quipu" status --json | jq -c '.queue | {worker, returned: [.returned[].issue]}'

step "a new worktree takes it over, fixes it, and it lands"
eval "old=\$wt_$c"
"$quipu" worktree remove "$old" --json | jq -c '{status}'
wt=$("$quipu" start "$c" --worktree --json | jq -r .worktree)
case "$wt" in /*) ;; *) wt="$work/repo/$wt" ;; esac
git -C "$wt" rm -q bad.txt
git -C "$wt" commit -qm "fix(proof): drop the bad file"
git -C "$wt" push -q origin HEAD
(cd "$wt" && "$quipu" done "$c" --json "${evidence[@]}") | jq -c '{issue, status}'

# The fixed PR and this one make the next batch of two, so neither waits on its timer.
step "a worker killed during a check neither loses nor double-merges a PR"
: >"$work/check-runs"
queue_pr "$e" slow.txt
started() { [ "$(runs)" -ge 1 ]; }
wait_for 300 "the check to start" started
pkill -9 -f "$quipu queue run"
echo "killed the worker during the check"
sleep 2
"$quipu" status --json | jq -c '.queue.worker'
for number in "$c" "$e"; do wait_for 400 "#$number Done" done_now "$number"; done
for number in "$c" "$e"; do
  [ "$(gh api "repos/$owner/$repo/issues/$number" --jq .state)" = closed ]
done
merges=$(git -C "$work/repo" fetch -q origin main && git -C "$work/repo" log --merges --format=%s origin/main | wc -l | tr -d ' ')
[ "$merges" = 5 ] || { echo "$merges merge commits for 5 PRs" >&2; exit 1; }

step "under a ruleset, where this repository may have one"
if "$quipu" protect --apply --json >"$work/protect.json" 2>&1; then
  jq -c . "$work/protect.json" | cut -c1-200
  echo "ruleset applied; landing under it is proved only if a later run needs the bypass"
else
  echo "NOT PROVEN: GitHub refused rulesets on this repository:"
  head -3 "$work/protect.json" | cut -c1-200
fi

printf '\n%s\n' "queue proof passed for $owner/$repo; cleanup follows"
