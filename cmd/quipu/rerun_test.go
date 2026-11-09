package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

// fakeRerunGH answers gh like GitHub would for one PR whose head has runs, and
// records every rerun it is asked for.
func fakeRerunGH(runs string, reruns *[]string) func(...string) ([]byte, error) {
	return func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"url":"https://github.com/octo/repo/pull/8","headRefOid":"abc123"}]`), nil
		case args[0] == "api" && strings.Contains(args[1], "actions/runs?head_sha=abc123"):
			return []byte(`{"workflow_runs":` + runs + `}`), nil
		case args[0] == "run" && args[1] == "rerun":
			*reruns = append(*reruns, strings.Join(args[2:], " "))
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected gh %v", args)
	}
}

func TestRerunRerunsOnlyFailedRuns(t *testing.T) {
	c := model.Config{Owner: "octo", Repository: "repo"}
	runs := `[
		{"id":11,"name":"Quipu policy","status":"completed","conclusion":"failure","run_attempt":1,"html_url":"u11"},
		{"id":12,"name":"CI","status":"completed","conclusion":"success","run_attempt":1,"html_url":"u12"},
		{"id":13,"name":"Writer","status":"in_progress","conclusion":"","run_attempt":1,"html_url":"u13"},
		{"id":14,"name":"Flaky","status":"completed","conclusion":"failure","run_attempt":3,"html_url":"u14"}]`
	var reruns []string
	result, err := rerunFailedRuns(fakeRerunGH(runs, &reruns), c, 7, "007/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(reruns) != 1 || reruns[0] != "11 --failed --repo octo/repo" {
		t.Fatalf("reruns %q, want only run 11 with --failed", reruns)
	}
	if result.Status != "Rerunning" || len(result.Rerun) != 1 || !strings.Contains(result.Rerun[0], "attempt 2") {
		t.Fatalf("result %+v", result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "Flaky") || !strings.Contains(result.Warnings[0], "3 times") {
		t.Fatalf("the run at its limit is not reported: %q", result.Warnings)
	}
	if result.PR != "https://github.com/octo/repo/pull/8" {
		t.Fatalf("PR %q", result.PR)
	}
}

func TestRerunStopsAtTheAttemptLimit(t *testing.T) {
	c := model.Config{Owner: "octo", Repository: "repo"}
	runs := `[{"id":14,"name":"Flaky","status":"completed","conclusion":"failure","run_attempt":3,"html_url":"u14"}]`
	var reruns []string
	result, err := rerunFailedRuns(fakeRerunGH(runs, &reruns), c, 7, "007/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(reruns) != 0 || result.Status != "Nothing rerun" || !strings.Contains(result.Next, "fix the cause") {
		t.Fatalf("a run at its attempt limit: reruns %q, result %+v", reruns, result)
	}
}

func TestRerunNeedsTheLease(t *testing.T) {
	w := leasedWorktree(t, 7)
	if _, err := rerunLease(w, 7, machineNow); err != nil {
		t.Fatalf("the lease holder may rerun: %v", err)
	}
	_, err := rerunLease(w, 8, machineNow)
	var flow *flowError
	if !errors.As(err, &flow) || flow.Code != "rejected-holder" {
		t.Fatalf("another Issue's rerun: %v, want rejected-holder", err)
	}
	if _, err := rerunLease(w, 7, machineNow.Add(3*time.Hour)); !errors.As(err, &flow) {
		t.Fatalf("an expired lease may not rerun: %v", err)
	}
}

func TestGuardAllowsQuipuRerunNotGhRunRerun(t *testing.T) {
	f := newGuardFixture(t)
	if code := guardCode(t, f.a, "Bash", map[string]any{"command": "quipu rerun 1 --json"}); code != "" {
		t.Fatalf("quipu rerun: %q, want allowed", code)
	}
	if code := guardCode(t, f.a, "Bash", map[string]any{"command": "gh run rerun 1"}); code != "github-write" {
		t.Fatalf("gh run rerun: %q, want github-write", code)
	}
}
