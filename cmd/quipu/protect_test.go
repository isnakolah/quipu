package main

import "testing"

func TestJudgeBranchRules(t *testing.T) {
	full := `[{"type":"deletion"},{"type":"non_fast_forward"},{"type":"pull_request","parameters":{}},
{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"},{"context":"policy"}]}}]`
	if check := judgeBranchRules("main", []byte(full)); check.Status != "ok" {
		t.Fatalf("full rules: %+v", check)
	}
	partial := `[{"type":"pull_request","parameters":{}},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`
	check := judgeBranchRules("main", []byte(partial))
	if check.Status != "fail" || check.Detail != "main rules lack non_fast_forward, deletion, check:policy" {
		t.Fatalf("partial rules: %+v", check)
	}
	if check := judgeBranchRules("main", []byte(`[]`)); check.Status != "fail" {
		t.Fatalf("no rules: %+v", check)
	}
}
