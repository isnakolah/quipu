package main

import (
	"flag"
	"reflect"
	"testing"

	"github.com/isnakolah/quipu/internal/model"
)

func TestCountIssuesSkipsPullRequestsAcrossPages(t *testing.T) {
	pages := `[{"number":1},{"number":2,"pull_request":{"url":"x"}}]
[{"number":3,"pull_request":null},{"number":4,"pull_request":{"url":"y"}}][]`
	count, err := countIssues([]byte(pages))
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if count, err := countIssues([]byte(`[{"number":9,"pull_request":{}}]`)); err != nil || count != 0 {
		t.Fatalf("a repository with only PRs: count=%d err=%v", count, err)
	}
	if _, err := countIssues([]byte(`{"message":"Not Found"}`)); err == nil {
		t.Fatal("expected an error for a non-list response")
	}
}

func TestParseProject(t *testing.T) {
	raw := `{"id":"PVT_1","number":4,"title":"Calla","items":{"totalCount":0},"owner":{"login":"o","type":"Organization"}}`
	info, err := parseProject([]byte(raw), 4)
	if err != nil || info != (projectInfo{ID: "PVT_1", Number: 4, Title: "Calla", Items: 0, OwnerType: "Organization"}) {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if _, err := parseProject([]byte(raw), 5); err == nil {
		t.Fatal("expected a number mismatch to fail")
	}
}

func fieldsWith(status []string, extra map[string]projectField) map[string]projectField {
	options := map[string]string{}
	for _, name := range status {
		options[name] = "id-" + name
	}
	fields := map[string]projectField{"Status": {ID: "status", Type: "ProjectV2SingleSelectField", Options: options}}
	for name, field := range extra {
		fields[name] = field
	}
	return fields
}

func TestPlanOptions(t *testing.T) {
	specs := projectFieldSpecs([]string{"stable", "web"})
	quipuStatus := []string{}
	for _, option := range statusOptions {
		quipuStatus = append(quipuStatus, option.name)
	}
	complete := map[string]projectField{}
	for _, spec := range specs {
		field := projectField{ID: spec.name, Type: "ProjectV2Field", Options: map[string]string{}}
		if spec.kind == "SINGLE_SELECT" {
			field.Type = "ProjectV2SingleSelectField"
			for _, option := range spec.options {
				field.Options[option] = option
			}
		}
		complete[spec.name] = field
	}
	plans, err := planOptions(fieldsWith(quipuStatus, complete), specs)
	if err != nil || len(plans) != 0 {
		t.Fatalf("a provisioned Project needs nothing: %+v err=%v", plans, err)
	}

	// A new Project: GitHub's default Status, and an Area missing "Web".
	area := complete["Area"]
	area.Options = map[string]string{"Stable": "s", "Docs": "d"}
	partial := map[string]projectField{}
	for name, field := range complete {
		partial[name] = field
	}
	partial["Area"] = area
	plans, err = planOptions(fieldsWith([]string{"Todo", "In Progress", "Done"}, partial), specs)
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	if plans[0].field != "Status" || !reflect.DeepEqual(plans[0].set, statusOptions) || len(plans[0].missing) != 9 {
		t.Fatalf("status plan = %+v", plans[0])
	}
	if plans[1].field != "Area" || !reflect.DeepEqual(plans[1].missing, []string{"Web"}) ||
		!reflect.DeepEqual(plans[1].set, []selectOption{{"Docs", "GRAY"}, {"Stable", "GRAY"}, {"Web", "GRAY"}}) {
		t.Fatalf("area plan keeps existing options and adds missing: %+v", plans[1])
	}

	text := complete["Priority"]
	text.Type = "ProjectV2Field"
	partial["Priority"] = text
	if _, err := planOptions(fieldsWith(quipuStatus, partial), specs); err == nil {
		t.Fatal("expected a Priority text field to be refused")
	}
}

func TestSetupFlagsChangeOnlyWhatIsGiven(t *testing.T) {
	c := newConfig("octo", "calla", projectInfo{ID: "P", Number: 4, Title: "Calla"})
	c.Workflows.QuipuVersion = "v0.2.0"
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	setup := addSetupFlags(fs)
	if err := fs.Parse([]string{"--runs-on", "self-hosted,macOS,calla-mac", "--schedule", "*/15 * * * *", "--writer-on-pr=false", "--required-checks", "build"}); err != nil {
		t.Fatal(err)
	}
	if err := setup.apply(fs, &c); err != nil {
		t.Fatal(err)
	}
	if c.WikiMode != "off" || c.DefaultBranch != "main" || !reflect.DeepEqual(c.Areas, []string{"stable"}) {
		t.Fatalf("flags not given must keep the config: %+v", c)
	}
	o := workflowOptions(c)
	if !reflect.DeepEqual(o.RunsOn["writer"], []string{"self-hosted", "macOS", "calla-mac"}) || o.Schedule != "*/15 * * * *" || o.WriterOnPullRequest || o.QuipuVersion != "v0.2.0" {
		t.Fatalf("options = %+v", o)
	}
	if !reflect.DeepEqual(c.Checks(), []string{"build"}) {
		t.Fatalf("checks = %v", c.Checks())
	}

	off := flag.NewFlagSet("adopt", flag.ContinueOnError)
	setup = addSetupFlags(off)
	_ = off.Parse([]string{"--schedule", "off"})
	if err := setup.apply(off, &c); err != nil || workflowOptions(c).Schedule != "" {
		t.Fatalf("schedule off: %v %q", err, workflowOptions(c).Schedule)
	}

	dev := newConfig("octo", "calla", projectInfo{ID: "P", Number: 4, Title: "Calla"})
	dev.Workflows.QuipuVersion = ""
	none := flag.NewFlagSet("adopt", flag.ContinueOnError)
	setup = addSetupFlags(none)
	_ = none.Parse(nil)
	if err := setup.apply(none, &dev); err == nil {
		t.Fatal("expected a development build without --quipu-version to be refused")
	}
	bad := flag.NewFlagSet("adopt", flag.ContinueOnError)
	setup = addSetupFlags(bad)
	_ = bad.Parse([]string{"--quipu-version", "main; rm -rf /"})
	if err := setup.apply(bad, &c); err == nil {
		t.Fatal("expected an invalid version to be refused")
	}
}

func TestConfigValidatesWorkflows(t *testing.T) {
	base := newConfig("octo", "calla", projectInfo{ID: "P", Number: 4, Title: "Calla"})
	for name, mutate := range map[string]func(*model.Config){
		"unknown job":     func(c *model.Config) { c.Workflows.RunsOn = map[string][]string{"deploy": {"x"}} },
		"empty labels":    func(c *model.Config) { c.Workflows.RunsOn = map[string][]string{"writer": {}} },
		"label injection": func(c *model.Config) { c.Workflows.RunsOn = map[string][]string{"writer": {"a]\nb"}} },
		"bad schedule":    func(c *model.Config) { c.Workflows.Schedule = "hourly" },
		"policy check":    func(c *model.Config) { c.RequiredChecks = []string{"policy"} },
	} {
		c := base
		c.Workflows.QuipuVersion = "v0.2.0"
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
