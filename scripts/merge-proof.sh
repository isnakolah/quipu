#!/usr/bin/env bash
# Disposable proof for `merge: agent` and `commit_format: issue-prefix`:
# `quipu done` refuses to merge while a required check fails, and merges
# with an issue-prefixed merge commit once every required check is green.
# Creates, then deletes, a private repository and a Project.
#
# The Writer runs locally in a loop, trusted by the disposable config. The
# required `test` check is a real Actions run that fails until proof-ok
# exists; `policy` is posted as a commit status, since the policy workflow
# needs the Writer token.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-merge-proof-$(date +%s)"
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""
writer_pid=""

cleanup() {
  status=$?
  [ -z "$writer_pid" ] || kill "$writer_pid" 2>/dev/null || true
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

step "adopted private repository $owner/$repo with merge: agent and commit_format: issue-prefix"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app >/dev/null
printf '%s\n' 'receipts_trusted_before: "2100-01-01T00:00:00Z"' 'merge: agent' 'commit_format: issue-prefix' >>.github/quipu/config.yaml
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
      - uses: actions/checkout@v4
      - run: test -f proof-ok
YAML
git add -A && git commit -qm "chore(quipu): adopt Quipu" && git push -q origin main

cat >"$work/backlog.yaml" <<'YAML'
schema: quipu-import/v1
items:
  - key: EPIC-1
    kind: epic
    title: Disposable merge proof
  - key: STORY-1
    kind: story
    title: Agent merge
    parent: EPIC-1
    status: Ready
    body: |
      <!-- work:v1 -->
      ## Outcome
      Proof work.
      ## Scope
      Includes:
      - The proof.
      Excludes:
      - Everything else.
      ## Acceptance criteria
      - [ ] It merges.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.
YAML
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
story=$(gh api "repos/$owner/$repo/issues?state=all" --jq '.[] | select(.body // "" | contains("key=STORY-1 -->")) | .number')

( while :; do "$quipu" writer reconcile --apply >/dev/null 2>&1 || true; sleep 3; done ) &
writer_pid=$!
ready() { [ "$("$quipu" context --issue-number "$story" --json | jq -r .status)" = Ready ]; }
wait_for 180 "#$story Ready" ready

step "agent starts #$story and opens a PR"
"$quipu" start "$story" --json | jq -c '{issue, status, branch}'
branch=$(git branch --show-current)
commit() {
  printf '#%03d %s\n\nRefs: #%d\n' "$story" "$1" "$story" >"$work/msg"
  "$quipu" verify-commit "$work/msg"
  git commit -qF "$work/msg"
}
echo "agent change" >change.txt && git add change.txt && commit "feat(proof): agent change"
git push -q -u origin "$branch"
pr=$(gh pr create --base main --head "$branch" --title "feat(proof): agent merge" --body "Refs #$story")
echo "$pr"
policy_green() { gh api "repos/$owner/$repo/statuses/$(git rev-parse HEAD)" -f state=success -f context=policy >/dev/null; }
test_done() { gh pr checks "$pr" --json name,bucket --jq '.[] | select(.name == "test") | .bucket' | grep -qE '^(pass|fail)$'; }
evidence=(--criteria "merge gated on checks" --commands "scripts/merge-proof.sh" --documentation "None: disposable proof" --risks None --boundary provider)
policy_green
wait_for 600 "the test check" test_done

step "done refuses while test fails"
if "$quipu" done "$story" --json "${evidence[@]}" >"$work/done1.json" 2>&1; then
  echo "done merged with a failing check" >&2; cat "$work/done1.json" >&2; exit 1
fi
cat "$work/done1.json"
grep -q '"checks-failing"' "$work/done1.json"
[ "$(gh pr view "$pr" --json state --jq .state)" = OPEN ]

step "fix, push, and done merges once green"
touch proof-ok && git add proof-ok && commit "fix(proof): satisfy the check"
git push -q
policy_green
test_passed() { gh pr checks "$pr" --json name,bucket --jq '.[] | select(.name == "test") | .bucket' | grep -qx pass; }
wait_for 600 "the test check to pass" test_passed
"$quipu" done "$story" --json "${evidence[@]}" | tee "$work/done2.json"
jq -e '.status == "Done"' "$work/done2.json" >/dev/null
sha=$(gh pr view "$pr" --json state,mergeCommit --jq 'select(.state == "MERGED") | .mergeCommit.oid')
subject=$(gh api "repos/$owner/$repo/commits/$sha" --jq '.commit.message')
printf 'merge commit %s:\n%s\n' "$sha" "$subject"
expected=$(printf '#%03d feat(proof): agent merge' "$story")
[ "$(printf '%s\n' "$subject" | head -1)" = "$expected" ]
printf '%s\n' "$subject" | grep -qx "Refs: #$story"
[ "$(gh issue view "$story" --repo "$owner/$repo" --json state --jq .state)" = CLOSED ]

printf '\n%s\n' "merge proof passed for $owner/$repo; cleanup follows"
