#!/usr/bin/env bash
# Disposable proof for scale and draining: 104 Ready Stories under one Epic
# (105 Issues, more than one page) are imported, and one
# `quipu writer reconcile --since-last-run` pass answers every request;
# status and validate then see all of them.
# Creates, then deletes, a private repository and a Project.
#
# The Writer runs locally, trusted by the disposable config. With no
# earlier successful Writer run, --since-last-run scans every Issue.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-drain-proof-$(date +%s)"
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""
stories=${QUIPU_PROOF_STORIES:-104}

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

quipu=${QUIPU_BIN:-$work/quipu}
[ -n "${QUIPU_BIN:-}" ] || (cd "$source_dir" && go build -o "$quipu" ./cmd/quipu)
pin=${QUIPU_PIN:-$(git -C "$source_dir" rev-parse HEAD)}

step "adopted private repository $owner/$repo"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" >/dev/null
printf '%s\n' 'receipts_trusted_before: "2100-01-01T00:00:00Z"' >>.github/quipu/config.yaml

{
  printf 'schema: quipu-import/v1\nitems:\n  - key: EPIC\n    kind: epic\n    title: Disposable drain proof\n'
  for i in $(seq 1 "$stories"); do
    printf '  - key: S%03d\n    kind: story\n    title: Story %d\n    parent: EPIC\n    status: Ready\n    body: |\n' "$i" "$i"
    printf '      %s\n' '<!-- work:v1 -->' '## Outcome' 'Proof work.' '## Scope' 'Includes:' '- The proof.' 'Excludes:' '- Everything else.' \
      '## Acceptance criteria' '- [ ] It drains.' '## Required evidence' '- Proof log.' '- Verification boundary: provider' \
      '## Documentation impact' 'None: disposable proof.' '## Source and relationships' '- Native parent and blocker links are authoritative.'
  done
} >"$work/backlog.yaml"

step "import $stories Ready Stories ($((stories + 1)) Issues)"
start=$(date +%s)
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >"$work/import.json" || { cat "$work/import.json" >&2; exit 1; }
jq -e "(.import_requests | length) == $stories" "$work/import.json" >/dev/null
echo "imported in $(( $(date +%s) - start ))s with $stories pending requests"

step "one Writer pass drains every request"
start=$(date +%s)
"$quipu" writer reconcile --since-last-run --apply --json | tee "$work/writer.json"
echo "drained in $(( $(date +%s) - start ))s"
jq -e ".requests_receipted == $stories and .issues_scanned == $((stories + 1)) and (.errors | length) == 0" "$work/writer.json" >/dev/null

step "status and validate see every Issue"
"$quipu" status --json >"$work/status.json"
jq -e "(.ready | length) == $stories" "$work/status.json" >/dev/null && echo "status: $stories Ready"
"$quipu" validate --json | tee "$work/validate.json"
jq -e ".valid == true and .open_issues == $((stories + 1))" "$work/validate.json" >/dev/null

step "a second pass has nothing left"
"$quipu" writer reconcile --since-last-run --apply --json | jq -e '.requests_receipted == 0' >/dev/null && echo "nothing pending"

printf '\n%s\n' "drain proof passed for $owner/$repo; cleanup follows"
