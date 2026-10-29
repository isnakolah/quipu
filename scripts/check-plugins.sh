#!/bin/sh
set -eu

for plugin in plugins/codex/quipu plugins/claude/quipu; do
  if [ "$plugin" = "plugins/codex/quipu" ]; then
    test -f "$plugin/.codex-plugin/plugin.json"
    jq empty "$plugin/.codex-plugin/plugin.json"
  else
    test -f "$plugin/.claude-plugin/plugin.json"
    jq empty "$plugin/.claude-plugin/plugin.json"
  fi
  test -f "$plugin/hooks/hooks.json"
  jq empty "$plugin/hooks/hooks.json"
  test -f "$plugin/skills/quipu/SKILL.md"
  sh -n "$plugin/scripts/pre-write-guard"
  printf '%s' '{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}' | PATH="$PWD:$PATH" "$plugin/scripts/pre-write-guard"
  if printf '%s' '{"tool_name":"Bash","tool_input":{"command":"git push origin main"}}' | PATH="$PWD:$PATH" "$plugin/scripts/pre-write-guard"; then
    echo "plugin guard allowed state-changing Bash command: $plugin" >&2
    exit 1
  else
    status=$?
    test "$status" -eq 2
  fi
done
