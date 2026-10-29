package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/isnakolah/quipu/internal/model"
	"gopkg.in/yaml.v3"
)

func defectConfig() model.Config {
	return model.Config{Owner: "o", Repository: "r", DefaultBranch: "main", Areas: []string{"app"}}
}

func failedRun() runFacts {
	return runFacts{ID: 4242, Name: "CI", Conclusion: "failure", HeadBranch: "main", HeadSHA: "0123456789abcdef", Event: "push", URL: "https://github.com/o/r/actions/runs/4242", Attempt: 1}
}

func TestDefectSkip(t *testing.T) {
	c := defectConfig()
	if reason := defectSkip(c, failedRun()); reason != "" {
		t.Fatalf("a failed CI push to main files a Defect: %s", reason)
	}
	for name, mutate := range map[string]func(*runFacts){
		"success":        func(r *runFacts) { r.Conclusion = "success" },
		"cancelled":      func(r *runFacts) { r.Conclusion = "cancelled" },
		"other branch":   func(r *runFacts) { r.HeadBranch = "042/x" },
		"pull request":   func(r *runFacts) { r.Event = "pull_request" },
		"other workflow": func(r *runFacts) { r.Name = "Release" },
	} {
		run := failedRun()
		mutate(&run)
		if defectSkip(c, run) == "" {
			t.Errorf("%s: expected no Defect", name)
		}
	}
	off := []string{}
	c.Workflows.DefectsFrom = &off
	if defectSkip(c, failedRun()) == "" {
		t.Fatal("defects_from: [] turns Defects off")
	}
	timedOut := failedRun()
	timedOut.Conclusion = "timed_out"
	if defectSkip(defectConfig(), timedOut) != "" {
		t.Fatal("a timed-out run is a failure")
	}
}

func TestDefectParentWalksToTheEpic(t *testing.T) {
	graph := map[int]issueNode{
		10: {Number: 10, Kind: "task", Parent: 9},
		9:  {Number: 9, Kind: "story", Parent: 1},
		1:  {Number: 1, Kind: "epic"},
		20: {Number: 20, Kind: "task"},
		30: {Number: 30, Kind: "task", Parent: 31},
		31: {Number: 31, Kind: "story", Parent: 30},
	}
	load := func(n int) (issueNode, error) {
		node, ok := graph[n]
		if !ok {
			return issueNode{}, fmt.Errorf("#%d not found", n)
		}
		return node, nil
	}
	if epic, err := defectParent(10, load); err != nil || epic.Number != 1 {
		t.Fatalf("epic=%+v err=%v", epic, err)
	}
	if epic, err := defectParent(1, load); err != nil || epic.Number != 1 {
		t.Fatalf("an Epic is its own Epic: %+v %v", epic, err)
	}
	if _, err := defectParent(20, load); err == nil || !strings.Contains(err.Error(), "no Epic") {
		t.Fatalf("orphan: %v", err)
	}
	if _, err := defectParent(30, load); err == nil {
		t.Fatal("a parent cycle must end")
	}
	if _, err := defectParent(99, load); err == nil {
		t.Fatal("a missing Issue is an error")
	}
}

func TestPullIssue(t *testing.T) {
	pulls := []mergedPull{
		{Number: 5, Body: "Refs #3", Merged: "", SHA: "abc"},
		{Number: 6, Body: "Refs #4", Merged: "2026-10-29T10:00:00Z", SHA: "other"},
		{Number: 7, Body: "Refs #9\n\nWhy.", Merged: "2026-10-29T10:00:00Z", SHA: "abc", URL: "https://github.com/o/r/pull/7"},
	}
	pull, issue := pullIssue(pulls, "abc")
	if pull.Number != 7 || issue != 9 {
		t.Fatalf("pull=%+v issue=%d", pull, issue)
	}
	if _, issue := pullIssue([]mergedPull{{Body: "no reference", Merged: "x", SHA: "abc"}}, "abc"); issue != 0 {
		t.Fatal("a PR without Refs #N has no Issue")
	}
	if pull, _ := pullIssue(nil, "abc"); pull.Number != 0 {
		t.Fatal("a direct push has no PR")
	}
}

func TestDefectsFromSurvivesConfigRewrite(t *testing.T) {
	var c model.Config
	if got := c.Workflows.DefectWorkflows(); len(got) != 1 || got[0] != "CI" {
		t.Fatalf("default = %v", got)
	}
	off := []string{}
	c.Workflows.DefectsFrom = &off
	raw, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back model.Config
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Workflows.DefectWorkflows(); len(got) != 0 {
		t.Fatalf("defects_from: [] must stay off after a rewrite, got %v from\n%s", got, raw)
	}
	bad := []string{"CI\"]"}
	c = newConfig("o", "r", projectInfo{ID: "P", Number: 1, Title: "T"})
	c.Workflows.QuipuVersion = "v0.2.0"
	c.Workflows.DefectsFrom = &bad
	if err := c.Validate(); err == nil {
		t.Fatal("expected a quoted workflow name to be refused")
	}
}

func TestDefectBodyIsAContractAndDedupes(t *testing.T) {
	run := failedRun()
	body := defectBody(run, mergedPull{URL: "https://github.com/o/r/pull/7"}, 9)
	if err := model.ValidateWorkContract(body); err != nil {
		t.Fatalf("the Defect must be valid Ready work: %v\n%s", err, body)
	}
	for _, want := range []string{run.URL, "pull/7 for #9", "Verification boundary: CI"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	if title := defectTitle(run); title != "Defect: CI failed on main at 0123456" {
		t.Fatalf("title = %q", title)
	}
	pages := `[{"number":12,"body":` + quote(body) + `},{"number":13,"body":"<!-- quipu-defect:v1 run=1 -->","pull_request":{}}]`
	filed, err := markedIssues([]byte(pages), defectMarker)
	if err != nil || filed["4242"] != 12 || len(filed) != 1 {
		t.Fatalf("filed=%v err=%v", filed, err)
	}
}
