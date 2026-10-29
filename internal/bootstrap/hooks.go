package bootstrap

// Project-level agent hooks. Claude Code and Codex run the hooks committed in
// a repository for every session there, with or without the Quipu plugin, so
// writing them makes the guard part of the repository itself.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GuardHookCommand fails closed: without quipu on PATH every guarded tool
// call is denied rather than allowed unchecked.
const GuardHookCommand = `command -v quipu >/dev/null 2>&1 || { echo 'QUIPU DENIED [quipu-missing]: quipu is not installed, so this repository cannot check the call. Do instead: install quipu (docs/source-install.md), then retry.' >&2; exit 2; }; quipu guard`

// Hook is one project hook file and the tool matcher its guard entry uses.
type Hook struct {
	File    string
	Matcher string
}

// Hooks are the project hook files. Claude's "*" covers every
// tool; Codex names its shell, patch, and MCP tools.
var Hooks = []Hook{
	{".claude/settings.json", "*"},
	{".codex/hooks.json", "Bash|shell|exec_command|apply_patch|mcp__.*"},
}

// MergeGuardHook adds the PreToolUse guard to a hooks settings file, keeping
// everything else in it. It reports false when the guard is already there.
func MergeGuardHook(raw []byte, matcher string) ([]byte, bool, error) {
	if strings.Contains(string(raw), "quipu guard") {
		return raw, false, nil
	}
	settings := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return nil, false, err
		}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	pre, _ := hooks["PreToolUse"].([]any)
	entry := map[string]any{"matcher": matcher, "hooks": []any{map[string]any{"type": "command", "command": GuardHookCommand}}}
	hooks["PreToolUse"] = append([]any{entry}, pre...)
	settings["hooks"] = hooks
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
}

// InstallHooks merges the guard into each hook file under root and returns
// the files it changed. With write false it only reports them.
func InstallHooks(root string, write bool) ([]string, error) {
	var changed []string
	for _, hook := range Hooks {
		path := filepath.Join(root, hook.File)
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return changed, err
		}
		merged, updated, err := MergeGuardHook(raw, hook.Matcher)
		if err != nil {
			return changed, fmt.Errorf("%s: %w", hook.File, err)
		}
		if !updated {
			continue
		}
		changed = append(changed, hook.File)
		if !write {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(path, merged, 0o644); err != nil {
			return changed, err
		}
	}
	return changed, nil
}
