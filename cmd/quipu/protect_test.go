package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/isnakolah/quipu/internal/model"
)

func TestJudgeBranchRules(t *testing.T) {
	full := `[{"type":"deletion"},{"type":"non_fast_forward"},{"type":"pull_request","parameters":{}},
{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"},{"context":"policy"}]}}]`
	if check := judgeBranchRules("main", []byte(full), []string{"test"}); check.Status != "ok" {
		t.Fatalf("full rules: %+v", check)
	}
	partial := `[{"type":"pull_request","parameters":{}},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`
	check := judgeBranchRules("main", []byte(partial), []string{"test"})
	if check.Status != "fail" || check.Detail != "main rules lack non_fast_forward, deletion, check:policy" {
		t.Fatalf("partial rules: %+v", check)
	}
	if check := judgeBranchRules("main", []byte(full), []string{"build", "lint"}); check.Status != "fail" || check.Detail != "main rules lack check:build, check:lint" {
		t.Fatalf("configured checks: %+v", check)
	}
	if check := judgeBranchRules("main", []byte(`[]`), []string{"test"}); check.Status != "fail" {
		t.Fatalf("no rules: %+v", check)
	}
}

// roundTrip turns a payload into what GitHub receives.
func roundTrip(t *testing.T, payload any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMainRulesetPayload(t *testing.T) {
	c := model.Config{DefaultBranch: "main"}
	got := roundTrip(t, mainRuleset(c, false))
	if got["name"] != "quipu-main" || got["enforcement"] != "active" || len(got["bypass_actors"].([]any)) != 0 {
		t.Fatalf("ruleset = %v", got)
	}
	var types []string
	var checks []any
	for _, rule := range got["rules"].([]any) {
		rule := rule.(map[string]any)
		types = append(types, rule["type"].(string))
		if rule["type"] == "required_status_checks" {
			params := rule["parameters"].(map[string]any)
			if params["strict_required_status_checks_policy"] != true {
				t.Fatal("checks must be strict")
			}
			checks = params["required_status_checks"].([]any)
		}
		if rule["type"] == "pull_request" && rule["parameters"].(map[string]any)["required_review_thread_resolution"] != true {
			t.Fatal("conversations must be resolved")
		}
	}
	if !reflect.DeepEqual(types, []string{"deletion", "non_fast_forward", "pull_request", "required_status_checks"}) {
		t.Fatalf("rules = %v", types)
	}
	want := []any{map[string]any{"context": "test", "integration_id": float64(actionsAppID)}, map[string]any{"context": "policy", "integration_id": float64(actionsAppID)}}
	if !reflect.DeepEqual(checks, want) {
		t.Fatalf("checks = %v", checks)
	}
	include := got["conditions"].(map[string]any)["ref_name"].(map[string]any)["include"]
	if !reflect.DeepEqual(include, []any{"~DEFAULT_BRANCH"}) {
		t.Fatalf("include = %v", include)
	}

	c.RequiredChecks = []string{"build", "unit"}
	exempt := roundTrip(t, mainRuleset(c, true))
	bypass := exempt["bypass_actors"].([]any)
	if len(bypass) != 1 || bypass[0].(map[string]any)["actor_type"] != "RepositoryRole" || bypass[0].(map[string]any)["actor_id"] != float64(adminRoleID) {
		t.Fatalf("bypass = %v", bypass)
	}
	rules := exempt["rules"].([]any)
	contexts := []string{}
	for _, check := range rules[3].(map[string]any)["parameters"].(map[string]any)["required_status_checks"].([]any) {
		contexts = append(contexts, check.(map[string]any)["context"].(string))
	}
	if !reflect.DeepEqual(contexts, []string{"build", "unit", "policy"}) {
		t.Fatalf("contexts = %v", contexts)
	}
	if !reflect.DeepEqual(c.RequiredChecks, []string{"build", "unit"}) {
		t.Fatal("building the payload changed the config")
	}
}

func TestBranchRulesetPayload(t *testing.T) {
	got := roundTrip(t, branchRuleset("trunk"))
	exclude := got["conditions"].(map[string]any)["ref_name"].(map[string]any)["exclude"]
	if !reflect.DeepEqual(exclude, []any{"refs/heads/trunk", "refs/heads/[0-9][0-9][0-9]*/*", "refs/heads/dependabot/**"}) {
		t.Fatalf("exclude = %v", exclude)
	}
}
