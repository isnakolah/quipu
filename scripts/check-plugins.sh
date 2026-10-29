#!/bin/sh
# Checks both agent plugins: manifests and hooks parse, the two plugins stay
# identical apart from the provider name, and the guard really denies
# off-flow calls in the hook-input shapes Claude Code and Codex send.
set -eu

expect() {
  want=$1 plugin=$2 input=$3
  if printf '%s' "$input" | PATH="$PWD:$PATH" "$plugin/scripts/pre-write-guard" 2>/dev/null; then
    status=0
  else
    status=$?
  fi
  if [ "$status" -ne "$want" ]; then
    echo "$plugin: guard exited $status, want $want, for $input" >&2
    exit 1
  fi
}

for plugin in plugins/codex/quipu plugins/claude/quipu; do
  if [ "$plugin" = "plugins/codex/quipu" ]; then
    jq empty "$plugin/.codex-plugin/plugin.json"
  else
    jq empty "$plugin/.claude-plugin/plugin.json"
  fi
  jq empty "$plugin/hooks/hooks.json"
  test -f "$plugin/skills/quipu/SKILL.md"
  for script in "$plugin"/scripts/*; do sh -n "$script"; done
  expect 0 "$plugin" '{"tool_name":"Bash","cwd":".","tool_input":{"command":"go test ./..."}}'
  expect 2 "$plugin" '{"tool_name":"Bash","cwd":".","tool_input":{"command":"git push origin main"}}'
  expect 2 "$plugin" '{"tool_name":"Edit","cwd":".","tool_input":{"file_path":".git/quipu-lease"}}'
  expect 2 "$plugin" '{"tool_name":"apply_patch","cwd":".","tool_input":{"command":"*** Begin Patch\n*** Update File: README.md\n@@\n-a\n+b\n*** End Patch"}}'
  expect 2 "$plugin" '{"tool_name":"Bash","cwd":".","tool_input":{"command":"gh pr merge 1"}}'
  expect 2 "$plugin" 'not json'
done

# The guard scripts are identical; SKILL.md copies are checked by the
# Go test TestGuidesInSync, which generates them.
cmp plugins/claude/quipu/scripts/pre-write-guard plugins/codex/quipu/scripts/pre-write-guard
grep -q '"matcher": "\*"' plugins/claude/quipu/hooks/hooks.json
grep -q 'PLUGIN_ROOT' plugins/codex/quipu/hooks/hooks.json
! grep -q 'CODEX_PLUGIN_ROOT' plugins/codex/quipu/hooks/hooks.json
echo "plugins ok"
