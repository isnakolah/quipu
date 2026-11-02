#!/usr/bin/env bash
# Disposable proof of machine limits against real claims: with one build slot, a
# second build is Busy and stays Ready, --next takes work that needs no slot, and
# moving work to review frees its slot. Creates, then deletes, a private repository
# and a Project. The Writer runs locally in a loop, trusted by the disposable config.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-limits-proof-$(date +%s)"
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
    tail -20 "$work/writer.log" >&2
  fi
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

step "adopted private repository $owner/$repo with one build slot, for area app"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app,docs >/dev/null
printf '%s\n' 'receipts_trusted_before: "2100-01-01T00:00:00Z"' 'limits:' '  build_slots: 1' '  build_areas: [app]' >>quipu.yml
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
      - [ ] It waits for a slot.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.'
{
  printf 'schema: quipu-import/v1\nitems:\n  - key: EPIC-1\n    kind: epic\n    title: Disposable limits proof\n'
  for spec in A:app B:app C:docs; do
    printf '  - key: STORY-%s\n    kind: story\n    title: Change %s\n    parent: EPIC-1\n    status: Ready\n    area: %s\n    body: |\n%s\n' "${spec%:*}" "${spec%:*}" "${spec#*:}" "$story_body"
  done
} >"$work/backlog.yaml"
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
story() { gh api "repos/$owner/$repo/issues?state=all" --jq ".[] | select(.body // \"\" | contains(\"key=STORY-$1 -->\")) | .number"; }
a=$(story A) b=$(story B) c=$(story C)

( while :; do "$quipu" writer reconcile --apply >>"$work/writer.log" 2>&1 || true; sleep 3; done ) &
writer_pid=$!
status_of() { "$quipu" context --issue-number "$1" --json | jq -r .status; }
ready() { [ "$(status_of "$1")" = Ready ]; }
for number in "$a" "$b" "$c"; do wait_for 180 "#$number Ready" ready "$number"; done

step "the first build takes the slot"
start_a=$("$quipu" start "$a" --worktree --json)
wt_a=$(printf '%s' "$start_a" | jq -r .worktree)
case "$wt_a" in /*) ;; *) wt_a="$work/repo/$wt_a" ;; esac
printf '%s\n' "$start_a" | jq -c '{issue, status}'
"$quipu" status --json | jq -c .machine
[ "$("$quipu" status --json | jq -r .machine.used)" = 1 ]

step "a second build is Busy and stays Ready"
"$quipu" start "$b" --worktree --json | tee "$work/busy.json" | jq -c '{issue, status, reason}'
jq -e '.status == "Busy" and (.reason | contains("build slots"))' "$work/busy.json" >/dev/null
[ "$(status_of "$b")" = Ready ]

step "--next passes over the build for work that needs no slot"
start_c=$("$quipu" start --next --worktree --json)
printf '%s\n' "$start_c" | jq -c '{issue, status}'
[ "$(printf '%s' "$start_c" | jq -r .issue)" = "$c" ]
[ "$("$quipu" status --json | jq -r .machine.used)" = 1 ]

step "with only builds left, --next is Busy too"
"$quipu" start --next --worktree --json | tee "$work/next.json" | jq -c '{status, reason}'
jq -e '.status == "Busy"' "$work/next.json" >/dev/null
[ "$(status_of "$b")" = Ready ]

step "work already held resumes, and moving it to review frees its slot"
(cd "$wt_a" && "$quipu" start "$a" --json) | jq -c '{issue, status}'
printf 'a\n' >"$wt_a/a.txt"
git -C "$wt_a" add -A
git -C "$wt_a" commit -qm "feat(proof): change for $a"
git -C "$wt_a" push -q -u origin HEAD
(cd "$wt_a" && gh pr create --base main --head "$(git branch --show-current)" --title "feat(proof): change for $a" --body "Refs #$a" >/dev/null)
(cd "$wt_a" && "$quipu" done "$a" --json) | jq -c '{status}'
[ "$("$quipu" status --json | jq -r .machine.used)" = 0 ]

step "so the build that waited can start"
"$quipu" start "$b" --worktree --json | tee "$work/started.json" | jq -c '{issue, status}'
jq -e '.status == "In progress"' "$work/started.json" >/dev/null
"$quipu" status --json | jq -c .machine

printf '\n%s\n' "limits proof passed for $owner/$repo; cleanup follows"
