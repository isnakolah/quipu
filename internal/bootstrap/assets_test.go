package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func options() Options {
	return Options{Owner: "octo", ProjectNumber: 7, DefaultBranch: "trunk", QuipuVersion: "v0.2.0", Schedule: "*/5 * * * *", WriterOnPullRequest: true}
}

func TestInstallWritesCompleteDynamicContract(t *testing.T) {
	root := t.TempDir()
	result, err := Install(root, options(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Written) != len(workflows)+len(forms)+1 || len(result.Unchanged)+len(result.Kept) != 0 {
		t.Fatalf("first install = %+v", result)
	}
	for path := range workflows {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var parsed map[string]any
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("%s is not YAML: %v\n%s", path, err, raw)
		}
		if strings.Contains(string(raw), "[[") || strings.Contains(string(raw), "@main") {
			t.Fatalf("%s left a placeholder or installs @main:\n%s", path, raw)
		}
	}
	writer, _ := os.ReadFile(filepath.Join(root, ".github/workflows/quipu-writer.yml"))
	for _, want := range []string{"ref: trunk", "cmd/quipu@v0.2.0", "command -v quipu", "runs-on: ubuntu-latest", "cron: '*/5 * * * *'", "pull_request:", "${{ secrets.QUIPU_WRITER_TOKEN }}"} {
		if !strings.Contains(string(writer), want) {
			t.Fatalf("writer lacks %q:\n%s", want, writer)
		}
	}
	ci, _ := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if strings.Contains(string(ci), "./cmd/quipu") || !strings.Contains(string(ci), "if [ -f go.mod ]") || !strings.Contains(string(ci), "branches: [ trunk ]") {
		t.Fatalf("target CI must not require Quipu source layout:\n%s", ci)
	}
	config, _ := os.ReadFile(filepath.Join(root, ".github/ISSUE_TEMPLATE/config.yml"))
	if !strings.Contains(string(config), "https://github.com/users/octo/projects/7") {
		t.Fatalf("config=%q", config)
	}
}

func TestInstallIsIdempotentAndKeepsOwnFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(root, options(), false); err != nil {
		t.Fatal(err)
	}
	again, err := Install(root, options(), false)
	if err != nil || len(again.Written) != 0 || len(again.Kept) != 0 {
		t.Fatalf("second install changed files: %+v err=%v", again, err)
	}
	own := filepath.Join(root, ".github/workflows/ci.yml")
	if err := os.WriteFile(own, []byte("name: their CI\n"), 0644); err != nil {
		t.Fatal(err)
	}
	kept, err := Install(root, options(), false)
	if err != nil || !reflect.DeepEqual(kept.Kept, []string{".github/workflows/ci.yml"}) || len(kept.Written) != 0 {
		t.Fatalf("kept=%+v err=%v", kept, err)
	}
	if raw, _ := os.ReadFile(own); string(raw) != "name: their CI\n" {
		t.Fatal("an existing file was overwritten without replace")
	}
	replaced, err := Install(root, options(), true)
	if err != nil || !reflect.DeepEqual(replaced.Written, []string{".github/workflows/ci.yml"}) {
		t.Fatalf("replaced=%+v err=%v", replaced, err)
	}
}

func TestRenderFollowsWorkflowOptions(t *testing.T) {
	o := options()
	o.RunsOn = map[string][]string{"writer": {"self-hosted", "macOS", "calla-mac"}, "policy": {"self-hosted"}}
	o.QuipuVersion = "0123abc"
	o.Schedule = ""
	o.WriterOnPullRequest = false
	files, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	writer := files[".github/workflows/quipu-writer.yml"]
	for _, want := range []string{"runs-on: [self-hosted, macOS, calla-mac]", "cmd/quipu@0123abc"} {
		if !strings.Contains(writer, want) {
			t.Fatalf("writer lacks %q:\n%s", want, writer)
		}
	}
	for _, unwanted := range []string{"schedule:", "pull_request:"} {
		if strings.Contains(writer, unwanted) {
			t.Fatalf("writer still has %q:\n%s", unwanted, writer)
		}
	}
	var parsed struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			RunsOn any `yaml:"runs-on"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(writer), &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.On["workflow_dispatch"]; !ok || len(parsed.On) != 3 {
		t.Fatalf("writer triggers = %v", parsed.On)
	}
	if !reflect.DeepEqual(parsed.Jobs["writer"].RunsOn, []any{"self-hosted", "macOS", "calla-mac"}) {
		t.Fatalf("runs-on = %#v", parsed.Jobs["writer"].RunsOn)
	}
	if !strings.Contains(files[".github/workflows/quipu-policy.yml"], "runs-on: self-hosted\n") || !strings.Contains(files[".github/workflows/ci.yml"], "runs-on: ubuntu-latest") {
		t.Fatal("policy and test runners not rendered per job")
	}
	if _, err := Render(Options{DefaultBranch: "main"}); err == nil {
		t.Fatal("expected an unpinned version to be refused")
	}
}

func TestWriterWatchesDefectWorkflows(t *testing.T) {
	o := options()
	o.DefectsFrom = []string{"CI", "Source install proof"}
	files, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	writer := files[".github/workflows/quipu-writer.yml"]
	var parsed struct {
		On struct {
			WorkflowRun struct {
				Workflows []string `yaml:"workflows"`
				Types     []string `yaml:"types"`
			} `yaml:"workflow_run"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(writer), &parsed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.On.WorkflowRun.Workflows, o.DefectsFrom) || !reflect.DeepEqual(parsed.On.WorkflowRun.Types, []string{"completed"}) {
		t.Fatalf("workflow_run = %+v", parsed.On.WorkflowRun)
	}
	if !strings.Contains(parsed.Jobs["writer"].If, "workflow_run.head_branch == 'trunk'") || !strings.Contains(writer, `quipu writer defect --run-id "$RUN_ID" --apply --json`) {
		t.Fatalf("writer job does not file Defects for trunk failures:\n%s", writer)
	}
	o.DefectsFrom = nil
	files, _ = Render(o)
	if strings.Contains(files[".github/workflows/quipu-writer.yml"], "workflow_run:") {
		t.Fatal("no watched workflows, no workflow_run trigger")
	}
}
