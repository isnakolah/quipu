#!/usr/bin/env bash
# Disposable proof for `workflows.runner_start` and `merge_wait: background`:
# the runner is stopped, work is queued, `quipu done` starts the runner and
# returns, and the Writer merges with no agent waiting.
# Creates, then deletes, a repository and a Project, and runs a real self-hosted
# runner on this machine, for that repository only, while it runs.
#
# The Writer and policy are the generated workflows on GitHub-hosted runners, so
# receipts are real and the checks-finished trigger is what merges. The required
# `test` check needs this machine's runner and only sleeps.
#
# QUIPU_PIN must be a commit pushed to GitHub, because the workflows install it.
# gh's token becomes QUIPU_WRITER_TOKEN unless QUIPU_PROOF_WRITER_TOKEN is set; it
# needs the project and repo scopes, and delete_repo for the cleanup.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
stamp=$(date +%s)
repo="quipu-runner-proof-$stamp"
label="quipu-proof-$stamp"
visibility=${QUIPU_PROOF_VISIBILITY:-private} # private Actions need billing; public is free
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""

stop_runner() { pkill -f "$work/runner/" 2>/dev/null || true; }

cleanup() {
  status=$?
  stop_runner
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
gh api "repos/isnakolah/quipu/commits/$pin" --silent || { echo "$pin is not on GitHub; push it or set QUIPU_PIN" >&2; exit 1; }

case "$(uname -s)" in Darwin) runner_os=osx ;; Linux) runner_os=linux ;; *) echo "unsupported OS" >&2; exit 1 ;; esac
case "$(uname -m)" in arm64|aarch64) runner_arch=arm64 ;; x86_64) runner_arch=x64 ;; *) echo "unsupported CPU" >&2; exit 1 ;; esac

step "adopted $visibility repository $owner/$repo with merge_wait: background and runner_start"
gh repo create "$owner/$repo" "--$visibility" --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app >/dev/null
printf '%s\n' 'merge: agent' 'merge_wait: background' >>quipu.yml
# Each start adds a line to the starts file, so this proof can count them.
runner_start="nohup '$work/runner/run.sh' >>'$work/runner.log' 2>&1 & echo started >>'$work/starts'"
# Quipu writes its config with four-space indents.
awk -v line="    runner_start: \"$runner_start\"" '{ print } /^workflows:$/ { print line }' quipu.yml >"$work/quipu.yml"
cp "$work/quipu.yml" quipu.yml
grep -q runner_start quipu.yml
"$quipu" adopt --apply --replace --json >"$work/adopt.json" || { cat "$work/adopt.json" >&2; exit 1; }
cat >.github/workflows/ci.yml <<YAML
name: CI
on:
  pull_request:
permissions:
  contents: read
jobs:
  test:
    runs-on: [self-hosted, $label]
    steps:
      - run: sleep 240
YAML
grep -q '"Quipu policy"' .github/workflows/quipu-writer.yml
grep -q 'head_repository.full_name == github.repository' .github/workflows/quipu-writer.yml
# Piped, never expanded, so tracing this script with bash -x can't log the token.
if [ -n "${QUIPU_PROOF_WRITER_TOKEN:-}" ]; then printenv QUIPU_PROOF_WRITER_TOKEN; else gh auth token; fi | gh secret set QUIPU_WRITER_TOKEN --repo "$owner/$repo"
git add -A && git commit -qm "chore(quipu): adopt Quipu" && git push -q origin main

step "a runner is registered for $label and left stopped"
url=$(gh api repos/actions/runner/releases/latest --jq ".assets[] | select(.name | test(\"^actions-runner-$runner_os-$runner_arch-[0-9.]+\\\\.tar\\\\.gz\$\")) | .browser_download_url")
mkdir "$work/runner"
curl -fsSL "$url" | tar -xz -C "$work/runner"
token=$(gh api -X POST "repos/$owner/$repo/actions/runners/registration-token" --jq .token)
(cd "$work/runner" && ./config.sh --unattended --url "https://github.com/$owner/$repo" --token "$token" --name "$label" --labels "$label" >/dev/null)
runner_is() { [ "$(gh api "repos/$owner/$repo/actions/runners" --jq '.runners[0].status')" = "$1" ]; }
offline() { runner_is offline; }
online() { runner_is online; }
wait_for 60 "the runner listed offline" offline

cat >"$work/backlog.yaml" <<'YAML'
schema: quipu-import/v1
items:
  - key: EPIC-1
    kind: epic
    title: Disposable runner proof
  - key: STORY-1
    kind: story
    title: Runner on demand
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
      - Verification boundary: target host
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.
YAML
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
story=$(gh api "repos/$owner/$repo/issues?state=all" --jq '.[] | select(.body // "" | contains("key=STORY-1 -->")) | .number')
ready() { [ "$("$quipu" context --issue-number "$story" --json | jq -r .status)" = Ready ]; }
wait_for 600 "#$story Ready" ready

step "quipu start finds the runner down, starts it, and carries on"
"$quipu" start "$story" --json --wait 10m | tee "$work/start.json"
jq -e '.runner | contains("Ran runner_start")' "$work/start.json" >/dev/null
wait_for 180 "the runner online" online
branch=$(git branch --show-current)

step "the runner is stopped again, and work is queued for it"
stop_runner
wait_for 300 "the runner offline" offline
rm -f .git/quipu-runner-start
echo "agent change" >change.txt && git add change.txt && git commit -qm "feat(proof): agent change"
git push -q -u origin "$branch"
pr=$(gh pr create --base main --head "$branch" --title "feat(proof): runner on demand" --body "Refs #$story")
echo "$pr"
test_queued() { "$quipu" runner status --json >"$work/status.json" && jq -e '.queued[] | select(.name == "test" and .verdict == "cannot start")' "$work/status.json" >/dev/null; }
wait_for 300 "the test job to queue" test_queued
jq '{state, runners, queued, writer_last_success, runner_start}' "$work/status.json"
[ "$(wc -l <"$work/starts")" -eq 1 ]

step "quipu done starts the runner, queues the merge, and returns"
evidence=(--criteria "runner started on demand; the Writer merged" --commands "scripts/runner-proof.sh" --documentation "None: disposable proof" --risks "None" --boundary "target host")
"$quipu" done "$story" --json "${evidence[@]}" | tee "$work/done.json"
jq -e '.status == "In review"' "$work/done.json" >/dev/null
jq -e '.runner | contains("Ran runner_start")' "$work/done.json" >/dev/null
[ "$(wc -l <"$work/starts")" -eq 2 ]
[ "$(gh pr view "$pr" --json state --jq .state)" = OPEN ] || { echo "merged before the check could finish" >&2; exit 1; }

step "with nobody waiting: quipu status reports the PR, then the Writer merges"
"$quipu" status --json | jq '.pull_requests'
wait_for 180 "the runner online" online
closed() { [ "$(gh issue view "$story" --repo "$owner/$repo" --json state --jq .state)" = CLOSED ]; }
wait_for 1200 "the Writer to merge and close #$story" closed
[ "$(gh pr view "$pr" --json state --jq .state)" = MERGED ]
gh run list --repo "$owner/$repo" --workflow quipu-writer.yml --event workflow_run --json conclusion,createdAt --limit 5
author=$(gh api "repos/$owner/$repo/issues/$story/comments?per_page=100" --jq '[.[] | select(.body | contains("quipu-receipt"))] | last | .user.login')
[ "$author" = "github-actions[bot]" ]

printf '\n%s\n' "runner proof passed for $owner/$repo; cleanup follows"
