#!/usr/bin/env bash
# Disposable proof that `quipu done` never merges code that wasn't tested as
# merged, on a repository with no protection at all, where GitHub itself would
# merge a behind PR without a word. Also proves a repeated Done and the removal
# of a finished worktree. Creates, then deletes, a private repository and a
# Project.
#
# The Writer runs locally in a loop, trusted by the disposable config. The
# required `test` check is a real Actions run; `policy` is posted as a commit
# status, since the policy workflow needs the Writer token.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-behind-proof-$(date +%s)"
visibility=${QUIPU_PROOF_VISIBILITY:-private} # private Actions need billing; public is free
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""
writer_pid=""

cleanup() {
  status=$?
  [ -z "$writer_pid" ] || kill "$writer_pid" 2>/dev/null || true
  if [ "$status" -ne 0 ] && [ -f "$work/writer.log" ]; then
    printf '%s\n' "last of the Writer's output:" >&2
    tail -20 "$work/writer.log" >&2
  fi
  if [ -n "$project_number" ]; then
    gh project delete "$project_number" --owner "$owner" >/dev/null 2>&1 || true
  fi
  if [ "$status" -ne 0 ] && [ -n "${QUIPU_PROOF_KEEP:-}" ]; then
    printf '%s\n' "kept $owner/$repo and Project $project_number for inspection" >&2
    rm -rf "$work"; exit "$status"
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

step "adopted $visibility repository $owner/$repo with merge: agent and no protection"
gh repo create "$owner/$repo" "--$visibility" --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app >/dev/null
printf '%s\n' 'receipts_trusted_before: "2100-01-01T00:00:00Z"' 'merge: agent' >>quipu.yml
rm .github/workflows/quipu-writer.yml .github/workflows/quipu-policy.yml
cat >.github/workflows/ci.yml <<'YAML'
name: CI
on:
  pull_request:
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
YAML
printf 'greet\n' >lib.txt
: >calls.txt
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
      - [ ] It merges only as tested.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.'
{
  printf 'schema: quipu-import/v1\nitems:\n  - key: EPIC-1\n    kind: epic\n    title: Disposable behind proof\n'
  for key in A B C D; do
    printf '  - key: STORY-%s\n    kind: story\n    title: Change %s\n    parent: EPIC-1\n    status: Ready\n    body: |\n%s\n' "$key" "$key" "$story_body"
  done
} >"$work/backlog.yaml"
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
story() { gh api "repos/$owner/$repo/issues?state=all" --jq ".[] | select(.body // \"\" | contains(\"key=STORY-$1 -->\")) | .number"; }
a=$(story A) b=$(story B) c=$(story C) d=$(story D)

( while :; do "$quipu" writer reconcile --apply >>"$work/writer.log" 2>&1 || true; sleep 3; done ) &
writer_pid=$!
ready() { [ "$("$quipu" context --issue-number "$1" --json | jq -r .status)" = Ready ]; }
for number in "$a" "$b" "$c" "$d"; do wait_for 180 "#$number Ready" ready "$number"; done

# start_pr NUMBER FILE CONTENT: claim in a new worktree, commit, open a PR, and
# leave the worktree's path in $wt and the PR's URL in $pr.
start_pr() {
  wt=$("$quipu" start "$1" --worktree --json | jq -r .worktree)
  case "$wt" in /*) ;; *) wt="$work/repo/$wt" ;; esac
  printf '%s\n' "$3" >"$wt/$2"
  git -C "$wt" add -A
  git -C "$wt" commit -qm "feat(proof): change for $1"
  git -C "$wt" push -q -u origin HEAD
  pr=$(cd "$wt" && gh pr create --base main --head "$(git branch --show-current)" --title "feat(proof): change for $1" --body "Refs #$1")
  echo "#$1: $pr in $wt"
}
policy_green() { gh api "repos/$owner/$repo/statuses/$1" -f state=success -f context=policy >/dev/null; }
test_passed() { [ "$(gh pr checks "$1" --json name,bucket,state --jq '[.[] | select(.name == "test")][0].bucket' 2>/dev/null)" = pass ]; }
evidence=(--criteria "merged only as tested" --commands "scripts/behind-proof.sh" --documentation "None: disposable proof" --risks None --boundary provider)
local_evidence=(--criteria "merged only as tested" --commands "scripts/behind-proof.sh" --documentation "None: disposable proof" --risks None --boundary local)

step "two PRs, both green against the same main"
start_pr "$a" a.txt a; wt_a=$wt pr_a=$pr
start_pr "$b" b.txt b; wt_b=$wt pr_b=$pr
policy_green "$(git -C "$wt_a" rev-parse HEAD)"
policy_green "$(git -C "$wt_b" rev-parse HEAD)"
wait_for 600 "#$a's test" test_passed "$pr_a"
wait_for 600 "#$b's test" test_passed "$pr_b"

step "the first one merges, which leaves the second behind"
(cd "$wt_a" && "$quipu" done "$a" --json "${evidence[@]}") | jq -c '{status, pr}'
[ "$(gh pr view "$pr_a" --json state --jq .state)" = MERGED ]

step "GitHub would merge the second: it doesn't report it as behind"
github_knows() {
  state=$(gh pr view "$pr_b" --json mergeStateStatus,mergeable --jq '.mergeStateStatus + " " + .mergeable')
  case "$state" in *UNKNOWN*) return 1 ;; esac
}
wait_for 120 "GitHub to judge the PR" github_knows
echo "GitHub says: $state"
case "$state" in
  "CLEAN MERGEABLE"|"UNSTABLE MERGEABLE"|"HAS_HOOKS MERGEABLE") ;;
  *) echo "the proof needs a repository where GitHub would merge this PR" >&2; exit 1 ;;
esac

step "done refuses it anyway"
if (cd "$wt_b" && "$quipu" done "$b" --json "${evidence[@]}") >"$work/behind.json" 2>&1; then
  echo "done merged a behind PR" >&2; cat "$work/behind.json" >&2; exit 1
fi
cat "$work/behind.json"
grep -q '"merge-blocked"' "$work/behind.json"
[ "$(gh pr view "$pr_b" --json state --jq .state)" = OPEN ]

step "updated and tested again, it merges, containing what it was behind"
git -C "$wt_b" fetch -q origin
main_tip=$(git -C "$wt_b" rev-parse origin/main)
git -C "$wt_b" merge -q --no-edit origin/main
git -C "$wt_b" push -q
head_b=$(git -C "$wt_b" rev-parse HEAD)
policy_green "$head_b"
new_head_tested() { [ "$(gh pr view "$pr_b" --json headRefOid --jq .headRefOid)" = "$head_b" ] && test_passed "$pr_b"; }
wait_for 600 "#$b's test on the new head" new_head_tested
(cd "$wt_b" && "$quipu" done "$b" --json "${evidence[@]}") | jq -c '{status, pr}'
[ "$(gh pr view "$pr_b" --json state --jq .state)" = MERGED ]
git -C "$wt_b" merge-base --is-ancestor "$main_tip" "$head_b"

step "a repeated done succeeds with the state as it is"
(cd "$wt_b" && "$quipu" done "$b" --json "${evidence[@]}") | tee "$work/repeat.json" | jq -c '{status, next}'
jq -e '.status == "Done"' "$work/repeat.json" >/dev/null

step "Done freed both worktrees, and each can be removed, even from inside it"
for lease in "$wt_a" "$wt_b"; do
  if [ -f "$(git -C "$lease" rev-parse --absolute-git-dir)/quipu-lease" ]; then
    echo "$lease kept its lease file" >&2; exit 1
  fi
done
(cd "$wt_a" && "$quipu" worktree remove "$wt_a" --json) | jq -c '{status, worktree}'
"$quipu" worktree remove "$b" --json | jq -c '{status, worktree}'
[ ! -e "$wt_a" ] && [ ! -e "$wt_b" ]

step "with update_behind and merge_without_ci on, one PR that uses what another renamed"
git switch -q main
git pull -q --no-edit origin main
# shellcheck disable=SC2016 # the Actions-free check runs in the worktree later
cat >>quipu.yml <<'YAML'
update_behind: auto
merge_without_ci: true
local_check: 'while read -r name; do grep -qx "$name" lib.txt || exit 1; done < calls.txt'
YAML
git add -A && git commit -qm "chore(quipu): test merged code locally" && git push -q origin main

start_pr "$c" calls.txt greet; wt_c=$wt pr_c=$pr
start_pr "$d" lib.txt hello; wt_d=$wt pr_d=$pr
for tree in "$wt_c" "$wt_d"; do
  policy_green "$(git -C "$tree" rev-parse HEAD)"
  (cd "$tree" && eval "$(grep '^local_check:' quipu.yml | sed "s/^local_check: '//; s/'\$//")") || { echo "$tree must pass alone" >&2; exit 1; }
done

(cd "$wt_d" && "$quipu" done "$d" --json "${local_evidence[@]}") | jq -c '{status, pr}'
[ "$(gh pr view "$pr_d" --json state --jq .state)" = MERGED ]

step "the combination fails its check, so main stays good"
if (cd "$wt_c" && "$quipu" done "$c" --json "${local_evidence[@]}") >"$work/semantic.json" 2>&1; then
  echo "done merged a broken combination" >&2; cat "$work/semantic.json" >&2; exit 1
fi
cat "$work/semantic.json"
grep -q '"local-check-failed"' "$work/semantic.json"
[ "$(gh pr view "$pr_c" --json state --jq .state)" = OPEN ]
git -C "$wt_c" merge-base --is-ancestor origin/main HEAD

step "fixed, it merges on the head it checked"
printf 'hello\n' >"$wt_c/calls.txt"
git -C "$wt_c" add -A
git -C "$wt_c" commit -qm "fix(proof): call the new name"
git -C "$wt_c" push -q
head_c=$(git -C "$wt_c" rev-parse HEAD)
policy_green "$head_c"
(cd "$wt_c" && "$quipu" done "$c" --json "${local_evidence[@]}") | jq -c '{status, pr}'
[ "$(gh pr view "$pr_c" --json state,headRefOid --jq '.state + " " + .headRefOid')" = "MERGED $head_c" ]
[ "$(gh issue view "$c" --repo "$owner/$repo" --json state --jq .state)" = CLOSED ]

printf '\n%s\n' "behind proof passed for $owner/$repo; cleanup follows"
