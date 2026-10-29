#!/usr/bin/env bash
# Disposable proof for `quipu adopt`: an existing private repository with a
# PR and no Issues is adopted, and adopting it again changes nothing.
# Creates, then deletes, a private repository and a Project.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-adopt-proof-$(date +%s)"
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

quipu=${QUIPU_BIN:-$work/quipu}
[ -n "${QUIPU_BIN:-}" ] || (cd "$source_dir" && go build -o "$quipu" ./cmd/quipu)
pin=${QUIPU_PIN:-$(git -C "$source_dir" rev-parse HEAD)}

step "existing private repository $owner/$repo with a PR and no Issues"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
git switch -q -c 001/existing-work
echo "existing work" > existing.txt
git add existing.txt && git commit -qm "feat: existing work" && git push -q origin 001/existing-work
gh pr create --base main --head 001/existing-work --title "Existing work" --body "Opened before Quipu." >/dev/null
git switch -q main
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
echo "Project #$project_number"

step "audit: PRs are not Issues"
"$quipu" adopt --owner "$owner" --repo "$repo" --json | tee audit.json
jq -e '.issues_found == 0 and .safe_to_adopt == true' audit.json >/dev/null

step "first adoption"
"$quipu" adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json \
  --quipu-version "$pin" --runs-on self-hosted,macOS --schedule off --writer-on-pr=false | tee first.json
jq -e '.changed == true' first.json >/dev/null
grep -q 'runs-on: \[self-hosted, macOS\]' .github/workflows/quipu-writer.yml
grep -q "cmd/quipu@$pin" .github/workflows/quipu-writer.yml
! grep -q 'schedule:' .github/workflows/quipu-writer.yml
! grep -q 'pull_request:' .github/workflows/quipu-writer.yml

step "second adoption changes nothing"
"$quipu" adopt --apply --json | tee second.json
jq -e '.changed == false and ((.changes // []) | length == 0) and ((.kept // []) | length == 0)' second.json >/dev/null
test -z "$(git status --porcelain -- .github/quipu)" || git status --porcelain

step "commit the adoption, then protect"
git add -A && git commit -qm "chore(quipu): adopt Quipu" && git push -q origin main
if "$quipu" protect --apply --json >protect.json 2>protect.err; then
  gh api "repos/$owner/$repo/rulesets" --jq '[.[].name] | sort | join(", ")'
else
  printf 'protect: %s\n' "$(cat protect.err)"
fi
"$quipu" doctor || true

printf '\n%s\n' "adopt proof passed for $owner/$repo; cleanup follows"
