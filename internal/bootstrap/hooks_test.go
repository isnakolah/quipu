package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeGuardHookKeepsSettingsAndIsIdempotent(t *testing.T) {
	existing := []byte(`{"enabledPlugins":{"x@y":true},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"lint"}]}]}}`)
	merged, changed, err := MergeGuardHook(existing, "*")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
		Hooks          struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &settings); err != nil {
		t.Fatal(err)
	}
	pre := settings.Hooks.PreToolUse
	if !settings.EnabledPlugins["x@y"] || len(pre) != 2 || pre[0].Matcher != "*" || pre[0].Hooks[0].Command != GuardHookCommand || pre[1].Hooks[0].Command != "lint" {
		t.Fatalf("merged=%s", merged)
	}
	if _, changed, _ := MergeGuardHook(merged, "*"); changed {
		t.Fatal("second merge must be a no-op")
	}
	if _, _, err := MergeGuardHook([]byte("{not json"), "*"); err == nil {
		t.Fatal("broken settings must not be overwritten")
	}
}

func TestInstallHooksWritesBothProviders(t *testing.T) {
	root := t.TempDir()
	if changed, err := InstallHooks(root, false); err != nil || len(changed) != 2 {
		t.Fatalf("dry run: %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote files")
	}
	if _, err := InstallHooks(root, true); err != nil {
		t.Fatal(err)
	}
	for _, hook := range Hooks {
		raw, err := os.ReadFile(filepath.Join(root, hook.File))
		if err != nil || !strings.Contains(string(raw), "quipu guard") {
			t.Fatalf("%s: %v %s", hook.File, err, raw)
		}
	}
	if changed, _ := InstallHooks(root, true); len(changed) != 0 {
		t.Fatalf("reinstall changed %v", changed)
	}
}
