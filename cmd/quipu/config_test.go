package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isnakolah/quipu/internal/model"
	"gopkg.in/yaml.v3"
)

// repoConfig is a valid body for fixtures: this repository's own config with
// the optional settings cleared, so tests can add them without duplicate keys
// whatever this repository has chosen to turn on.
func repoConfig(t *testing.T) []byte {
	t.Helper()
	c, err := loadConfigAt(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	c = c.WithoutRelaxations()
	c.Workflows = model.Workflows{}
	raw, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeRepoFile(t *testing.T, top, name string, raw []byte) {
	t.Helper()
	path := filepath.Join(top, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, top string) {
	t.Helper()
	gitIn(t, top, "add", "-A")
	gitIn(t, top, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "change")
}

// publishedRepo is a checkout whose origin's default branch holds config at
// name, with a linked worktree an agent could edit.
func publishedRepo(t *testing.T, name string, config []byte) (top string, agent worktree) {
	t.Helper()
	top = gitRepo(t)
	writeRepoFile(t, top, name, config)
	commitAll(t, top)
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, top, "init", "-q", "--bare", "-b", "main", bare)
	gitIn(t, top, "remote", "add", "origin", bare)
	gitIn(t, top, "push", "-q", "origin", "main")
	path := filepath.Join(filepath.Dir(top), "agent-worktree")
	gitIn(t, top, "worktree", "add", "-q", "-b", "001/a", path)
	agent, err := openWorktree(path)
	if err != nil {
		t.Fatal(err)
	}
	return top, agent
}

func TestConfigPathPrecedence(t *testing.T) {
	config := repoConfig(t)
	top := t.TempDir()
	if _, err := loadConfigAt(top); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no config is not-exist, so non-Quipu repositories stay unguarded: %v", err)
	}
	if hasConfig(top) {
		t.Fatal("an empty directory is not governed")
	}

	writeRepoFile(t, top, configPath, config)
	if name, err := configFile(top); err != nil || name != configPath {
		t.Fatalf("falls back to the old path: %q %v", name, err)
	}
	if _, err := loadConfigAt(top); err != nil {
		t.Fatalf("the old path keeps working: %v", err)
	}

	writeRepoFile(t, top, rootConfigPath, config)
	_, err := loadConfigAt(top)
	if err == nil || errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "keep only quipu.yml") {
		t.Fatalf("both files must fail clearly, not read as missing: %v", err)
	}
	if !hasConfig(top) {
		t.Fatal("two configs still mean Quipu governs this checkout")
	}

	if err := os.Remove(filepath.Join(top, configPath)); err != nil {
		t.Fatal(err)
	}
	if name, err := configFile(top); err != nil || name != rootConfigPath {
		t.Fatalf("quipu.yml alone: %q %v", name, err)
	}
	if _, err := loadConfigAt(top); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsUnknownKeys(t *testing.T) {
	config := repoConfig(t)
	if _, err := loadConfigAt(filepath.Join("..", "..")); err != nil {
		t.Fatalf("this repository's config must stay valid: %v", err)
	}
	for _, typo := range []string{"mergee: agent\n", "workflows:\n  schedual: off\n"} {
		top := t.TempDir()
		writeRepoFile(t, top, rootConfigPath, append(append([]byte{}, config...), typo...))
		_, err := loadConfigAt(top)
		if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), rootConfigPath) {
			t.Fatalf("%q must fail and name the file: %v", typo, err)
		}
	}
}

func TestQuipuYmlIsProtected(t *testing.T) {
	f := newGuardFixture(t)
	for _, tc := range []struct {
		tool  string
		input map[string]any
	}{
		{"Write", map[string]any{"file_path": filepath.Join(f.a, rootConfigPath)}},
		{"Edit", map[string]any{"file_path": filepath.Join(f.a, rootConfigPath)}},
		{"Bash", map[string]any{"command": "echo merge: agent >> quipu.yml"}},
		{"Bash", map[string]any{"command": "sed -i '' s/owner/agents/ quipu.yml"}},
		{"Bash", map[string]any{"command": "rm quipu.yml"}},
	} {
		if code := guardCode(t, f.a, tc.tool, tc.input); code != "quipu-config" {
			t.Errorf("%s %v: a leased agent must not change quipu.yml, got %q", tc.tool, tc.input, code)
		}
	}
	if code := guardCode(t, f.a, "Write", map[string]any{"file_path": filepath.Join(f.a, "testdata", rootConfigPath)}); code != "" {
		t.Errorf("only the root file is the config: %q", code)
	}
	if code := guardCode(t, f.b, "Bash", map[string]any{"command": "quipu config --json"}); code != "" {
		t.Errorf("reading the config needs no lease: %q", code)
	}

	if !protectedFile(rootConfigPath) || protectedFile("testdata/"+rootConfigPath) {
		t.Fatal("policy protects the root quipu.yml only")
	}
	pull := pullFacts{Files: []string{"README.md", rootConfigPath}}
	if got := unapprovedProtectedChange(pull); got != rootConfigPath {
		t.Fatalf("changing quipu.yml needs the owner's label, got %q", got)
	}
	pull.Labels = []string{ownerApprovedLabel}
	if got := unapprovedProtectedChange(pull); got != "" {
		t.Fatalf("the owner's label allows it, got %q", got)
	}
}

func TestWorktreeCopyCannotTurnAFlagOn(t *testing.T) {
	config := repoConfig(t)
	for _, name := range []string{rootConfigPath, configPath} {
		t.Run(name, func(t *testing.T) {
			top, agent := publishedRepo(t, name, config)
			tampered := append(append([]byte{}, config...), "merge: agent\nready_by: agents\n"...)
			writeRepoFile(t, agent.Top, name, tampered)

			local, err := loadConfigAt(agent.Top)
			if err != nil || !local.AgentsMerge() {
				t.Fatalf("fixture: the worktree copy has the flag on: %v", err)
			}
			for _, refresh := range []bool{false, true} {
				c, source, err := loadEnforcedConfig(agent, refresh)
				if err != nil {
					t.Fatal(err)
				}
				if c.AgentsMerge() || c.AgentsMakeReady() {
					t.Fatalf("refresh %v: a worktree copy turned a flag on", refresh)
				}
				if source != "origin/main:"+name {
					t.Fatalf("source %q", source)
				}
			}
			if code := guardCode(t, agent.Top, "Bash", map[string]any{"command": "quipu ready 5"}); code != "quipu-admin" {
				t.Fatalf("guard read the flag from the worktree: %q", code)
			}

			// A flag the owner merged takes effect, including after a refresh.
			writeRepoFile(t, top, name, tampered)
			commitAll(t, top)
			gitIn(t, top, "push", "-q", "origin", "main")
			c, _, err := loadEnforcedConfig(agent, true)
			if err != nil || !c.AgentsMerge() {
				t.Fatalf("default branch has merge: agent: %v %v", c.Merge, err)
			}
			if code := guardCode(t, agent.Top, "Bash", map[string]any{"command": "quipu ready 5"}); code != "" {
				t.Fatalf("ready_by on the default branch: %q", code)
			}
		})
	}
}

func TestWorktreeCannotRedirectTheDefaultBranch(t *testing.T) {
	config := repoConfig(t)
	_, agent := publishedRepo(t, rootConfigPath, config)
	// The agent publishes a relaxed config on its own branch and names that
	// branch the default in its worktree copy.
	relaxed := strings.ReplaceAll(string(config), "default_branch: main", "default_branch: 001/a") + "merge: agent\n"
	writeRepoFile(t, agent.Top, rootConfigPath, []byte(relaxed))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	c, source, err := loadEnforcedConfig(agent, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentsMerge() || c.DefaultBranch != "main" || source != "origin/main:"+rootConfigPath {
		t.Fatalf("merge %q default %q from %q", c.Merge, c.DefaultBranch, source)
	}
}

func TestEnforcedConfigFailsClosed(t *testing.T) {
	config := repoConfig(t)
	relaxed := append(append([]byte{}, config...), "merge: agent\nready_by: agents\npolicy_exempt_authors:\n  - dependabot[bot]\n"...)

	// Nothing to compare against: no origin at all.
	top := gitRepo(t)
	writeRepoFile(t, top, rootConfigPath, relaxed)
	w, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	if c, source, err := loadEnforcedConfig(w, false); err != nil || !c.AgentsMerge() || source != configSourceWorktree {
		t.Fatalf("no origin uses the worktree: %v %q %v", c.Merge, source, err)
	}

	// An origin whose default branch has no config, as during adoption.
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, top, "init", "-q", "--bare", "-b", "main", bare)
	gitIn(t, top, "remote", "add", "origin", bare)
	gitIn(t, top, "push", "-q", "origin", "main")
	c, source, err := loadEnforcedConfig(w, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentsMerge() || c.AgentsMakeReady() || len(c.PolicyExemptAuthors) != 0 || !strings.Contains(source, "relaxing settings off") {
		t.Fatalf("unpublished config must relax nothing: %+v from %q", c, source)
	}

	// An origin that cannot be reached.
	gitIn(t, top, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	if c, _, err := loadEnforcedConfig(w, true); err != nil || c.AgentsMerge() {
		t.Fatalf("unreachable origin must relax nothing: %v %v", c.Merge, err)
	}

	// A broken config on the default branch is an error, not a fallback.
	broken, agent := publishedRepo(t, rootConfigPath, config)
	writeRepoFile(t, broken, rootConfigPath, append(append([]byte{}, config...), "mergee: agent\n"...))
	commitAll(t, broken)
	gitIn(t, broken, "push", "-q", "origin", "main")
	if _, _, err := loadEnforcedConfig(agent, true); err == nil || !strings.Contains(err.Error(), "origin/main:quipu.yml") {
		t.Fatalf("broken default-branch config: %v", err)
	}
}

func TestRepositoryWithOnlyQuipuYmlIsGuarded(t *testing.T) {
	top := gitRepo(t)
	writeRepoFile(t, top, rootConfigPath, repoConfig(t))
	commitAll(t, top)
	if code := guardCode(t, top, "Write", map[string]any{"file_path": filepath.Join(top, "main.go")}); code != "no-lease" {
		t.Fatalf("edit without a lease: %q", code)
	}
	if code := guardCode(t, top, "Bash", map[string]any{"command": "eval $x"}); code != "unparsable" {
		t.Fatalf("unparsable command: %q", code)
	}
}

func TestRemovingConfigFromAWorktreeKeepsTheGuard(t *testing.T) {
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	gitIn(t, top, "remote", "set-head", "origin", "main")
	if err := os.Remove(filepath.Join(agent.Top, rootConfigPath)); err != nil {
		t.Fatal(err)
	}
	if code := guardCode(t, agent.Top, "Write", map[string]any{"file_path": filepath.Join(agent.Top, "main.go")}); code != "no-lease" {
		t.Fatalf("deleting the config turned the guard off: %q", code)
	}
}

func TestConfigSettingsNameTheirSource(t *testing.T) {
	config := repoConfig(t)
	top := t.TempDir()
	writeRepoFile(t, top, rootConfigPath, append(append([]byte{}, config...), "merge: agent\nworkflows:\n  schedule: off\n"...))
	c, err := loadConfigAt(top)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := configSettings(c, "origin/main:quipu.yml")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]configSetting{}
	for _, setting := range settings {
		got[setting.Key] = setting
	}
	if s := got["merge"]; s.Value != "agent" || s.Source != "origin/main:quipu.yml" {
		t.Fatalf("set value: %+v", s)
	}
	if s := got["ready_by"]; s.Value != "owner" || s.Source != configSourceDefault {
		t.Fatalf("default value: %+v", s)
	}
	if s := got["workflows.schedule"]; s.Value != "off" || s.Source != "origin/main:quipu.yml" {
		t.Fatalf("nested set value: %+v", s)
	}
	if s := got["workflows.defects_from"]; s.Source != configSourceDefault {
		t.Fatalf("nested default: %+v", s)
	}

	base, err := decodeConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	differs, err := configDifferences(base, c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(differs, ",") != "merge,workflows.schedule" {
		t.Fatalf("differs: %v", differs)
	}
}

func TestWriteConfigKeepsTheExistingFile(t *testing.T) {
	c, err := decodeConfig(repoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	fresh := t.TempDir()
	written, err := writeConfig(fresh, c)
	if err != nil || len(written) != 1 || written[0] != rootConfigPath {
		t.Fatalf("new repositories get quipu.yml: %v %v", written, err)
	}
	adopted := t.TempDir()
	writeRepoFile(t, adopted, configPath, repoConfig(t))
	written, err = writeConfig(adopted, c)
	if err != nil || len(written) != 1 || written[0] != configPath {
		t.Fatalf("an adopted repository keeps its file: %v %v", written, err)
	}
	if _, err := os.Stat(filepath.Join(adopted, rootConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("no second config may appear: %v", err)
	}
}
