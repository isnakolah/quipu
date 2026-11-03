#!/usr/bin/env bash
# Disposable proof of workflows.writer_mode: serial, in real Actions, because what it
# depends on is GitHub's own behaviour: a job skipped by its condition stays out of
# the concurrency group, a 304 costs nothing, and how long a claim takes. Creates, then
# deletes, a private repository and a Project.
# QUIPU_PIN must be a commit pushed to GitHub, because the workflow installs it.
set -euo pipefail

owner=${QUIPU_PROOF_OWNER:-$(gh api user --jq .login)}
repo="quipu-writer-proof-$(date +%s)"
source_dir=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project_number=""
export QUIPU_STATE_DIR="$work/machine"

cleanup() {
  status=$?
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
pin=${QUIPU_PIN:-$(git -C "$source_dir" rev-parse HEAD)}
gh api "repos/isnakolah/quipu/commits/$pin" --silent || { echo "$pin is not on GitHub; push it or set QUIPU_PIN" >&2; exit 1; }
quipu=$work/quipu
(cd "$source_dir" && go build -o "$quipu" ./cmd/quipu)

step "adopt $owner/$repo, then migrate it to the serial Writer the documented way"
gh repo create "$owner/$repo" --private --add-readme >/dev/null
gh repo clone "$owner/$repo" "$work/repo" -- -q
cd "$work/repo"
project_number=$(gh project create --owner "$owner" --title "$repo" --format json --jq .number)
adopt=(adopt --owner "$owner" --repo "$repo" --project-number "$project_number" --apply --json --quipu-version "$pin" --areas app)
"$quipu" "${adopt[@]}" >/dev/null
grep -q 'quipu writer reconcile' .github/workflows/quipu-writer.yml
# Matches however the section is indented, which adopt decides.
perl -0pi -e 's/^workflows:\n(\s+)/workflows:\n$1writer_mode: serial\n$1/m' quipu.yml
grep -q 'writer_mode: serial' quipu.yml
# Doctor exits non-zero for any failing check, and this repository is half set up.
"$quipu" doctor --json >"$work/doctor.json" 2>"$work/doctor.err" || true
jq -e '.checks[] | select(.name == "writer mode" and .status == "warn")' "$work/doctor.json" >/dev/null || { echo "doctor must warn before regenerating:" >&2; cut -c1-300 "$work/doctor.err" >&2; exit 1; }
"$quipu" "${adopt[@]}" --replace >/dev/null
grep -q 'quipu writer serve --apply' .github/workflows/quipu-writer.yml
gh auth token | gh secret set QUIPU_WRITER_TOKEN --repo "$owner/$repo"
git add -A && git commit -qm "chore(quipu): adopt Quipu with the serial Writer" && git push -q origin main
echo "doctor warned, and regenerating gave the serial workflow"

story_body='      <!-- work:v1 -->
      ## Outcome
      Proof work.
      ## Scope
      Includes:
      - The proof.
      Excludes:
      - Everything else.
      ## Acceptance criteria
      - [ ] It is claimed.
      ## Required evidence
      - Proof log.
      - Verification boundary: provider
      ## Documentation impact
      None: disposable proof.
      ## Source and relationships
      - Native parent and blocker links are authoritative.'
{
  printf 'schema: quipu-import/v1\nitems:\n  - key: EPIC-1\n    kind: epic\n    title: Disposable Writer proof\n'
  for key in A B C; do
    printf '  - key: STORY-%s\n    kind: story\n    title: Change %s\n    parent: EPIC-1\n    status: Ready\n    body: |\n%s\n' "$key" "$key" "$story_body"
  done
} >"$work/backlog.yaml"
"$quipu" import --manifest "$work/backlog.yaml" --apply --json >/dev/null
story() { gh api "repos/$owner/$repo/issues?state=all" --jq ".[] | select(.body // \"\" | contains(\"key=STORY-$1 -->\")) | .number"; }
a=$(story A) b=$(story B) c=$(story C)

runs() { gh run list --repo "$owner/$repo" --workflow quipu-writer.yml --limit 50 --json databaseId,event,status,conclusion; }
active() { runs | jq '[.[] | select(.status != "completed" and .conclusion != "skipped")] | length'; }
quiet() { [ "$(active)" = 0 ]; }
serving() { runs | jq -e '[.[] | select(.event == "workflow_dispatch" and .status == "in_progress")] | length > 0' >/dev/null; }

step "a skipped job takes no place in the concurrency group"
wait_for 240 "the first runs to finish" quiet
gh workflow run quipu-writer.yml --repo "$owner/$repo"
wait_for 240 "the Writer to start" serving
# Earlier runs don't count: pending runs may replace each other when each is a real wake.
baseline=$(runs | jq '[.[].databaseId] | max')
gh issue comment "$a" --repo "$owner/$repo" --body '<!-- quipu-request:v1 a wake with nothing valid in it -->' >/dev/null
for n in 1 2 3; do gh issue comment "$a" --repo "$owner/$repo" --body "A person talking, $n." >/dev/null; done
comment_runs() { runs | jq --argjson after "$baseline" '[.[] | select(.event == "issue_comment" and .databaseId > $after)]'; }
skipped() { [ "$(comment_runs | jq '[.[] | select(.conclusion == "skipped")] | length')" -ge 3 ]; }
wait_for 120 "the plain comments to be skipped" skipped
comment_runs | jq -c 'group_by(.status + "/" + (.conclusion // "")) | map({(.[0].status + "/" + (.[0].conclusion // "")): length}) | add'
wait_for 300 "the pending run to finish" quiet
comment_runs | jq -c 'sort_by(.databaseId) | .[] | [.databaseId, .status, .conclusion]'
[ "$(comment_runs | jq '[.[] | select(.conclusion == "cancelled")] | length')" = 0 ] || { echo "a skipped job replaced the pending run" >&2; exit 1; }
[ "$(comment_runs | jq '[.[] | select(.conclusion == "success")] | length')" = 1 ] || { echo "the one wake must have run" >&2; exit 1; }
echo "the comments were skipped, and the one pending run survived them and ran"

step "an ETag makes the usual answer a 304 that costs nothing"
since=$(date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)
path="repos/$owner/$repo/issues/comments?per_page=100&sort=created&direction=asc&since=$since"
# Readers here take all their input: under pipefail, one that stops early fails the pipe.
header() { tr -d '\r' | awk -v name="$1" 'tolower($1) == tolower(name) ":" { found = $2 " " $3 } END { print found }' | sed 's/ *$//'; }
gh api -i "$path" >"$work/first" 2>/dev/null
etag=$(header etag <"$work/first")
first_left=$(header x-ratelimit-remaining <"$work/first")
gh api -i -H "If-None-Match: $etag" "$path" >"$work/second" 2>/dev/null || true
status_line=$(awk 'NR == 1' "$work/second")
case "$status_line" in *" 304"*) ;; *) echo "wanted a 304, got: $status_line" >&2; exit 1 ;; esac
second_left=$(header x-ratelimit-remaining <"$work/second")
echo "200 left $first_left requests; the 304 after it left $second_left"
[ "$second_left" -ge "$first_left" ] || { echo "the 304 was counted" >&2; exit 1; }

step "claim latency"
claim() { # label, args...
  local label=$1 began took; shift
  began=$(date +%s)
  "$quipu" start "$@" --worktree --json 2>/dev/null | jq -c '{issue, status}'
  took=$(( $(date +%s) - began ))
  echo "$label: claim and start took ${took}s"
  eval "took_$label=\$took"
}
wait_for 240 "the Writer to go idle" quiet
claim cold "$a"
claim warm "$b"
claim next --next
[ "$took_warm" -le 30 ] || { echo "a claim with the Writer running took ${took_warm}s" >&2; exit 1; }
"$quipu" status --json | jq -c '{in_progress: [.in_progress[].number]}'

step "the Writer stops by itself when idle, having polled with ETags"
wait_for 240 "the Writer to go idle" quiet
id=$(runs | jq -r '[.[] | select(.event == "issue_comment" and .conclusion == "success")][0].databaseId')
gh run view "$id" --repo "$owner/$repo" --log >"$work/run.log"
grep -E '"(reason|polls|polls_unchanged)"' "$work/run.log" | sed 's/.*Z //' | tr -s ' ' | sort -u
grep '"reason": "idle"' "$work/run.log" >/dev/null || { echo "the run did not stop for being idle" >&2; exit 1; }
cancelled=$(runs | jq '[.[] | select(.conclusion == "cancelled")] | length')
total=$(runs | jq 'length')
echo "$cancelled of $total Writer runs cancelled"

printf '\n%s\n' "writer proof passed for $owner/$repo; cleanup follows"
