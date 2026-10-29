package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

func TestBotPRsAreExemptOnlyWhenConfigured(t *testing.T) {
	c := watchConfig()
	bot := pullFacts{Author: "app/dependabot", IsBot: true, Body: "Bumps x from 1 to 2.", Base: "main", Head: "dependabot/go_modules/x-2", Files: []string{"go.mod"}}
	noWork := func(int) (liveWork, error) { t.Fatal("exempt PRs need no Issue"); return liveWork{}, nil }
	if v := prViolations(c, bot, func(int) (liveWork, error) { return liveWork{}, nil }, cutover); len(v) == 0 {
		t.Fatal("without config a bot PR is checked like any other")
	}
	c.PolicyExemptAuthors = []string{"dependabot[bot]"}
	if v := prViolations(c, bot, noWork, cutover); len(v) != 0 {
		t.Fatalf("exempt bot: %v", v)
	}
	bot.Files = []string{".github/workflows/ci.yml"}
	if v := prViolations(c, bot, noWork, cutover); len(v) != 1 || !strings.Contains(v[0], ownerApprovedLabel) {
		t.Fatalf("bots still need approval for Quipu's files: %v", v)
	}
	human := bot
	human.IsBot, human.Author = false, "dependabot"
	if v := prViolations(c, human, func(int) (liveWork, error) { return liveWork{}, nil }, cutover); len(v) == 0 {
		t.Fatal("a non-bot author with a bot's name is not exempt")
	}
}

func TestConfigOptionsValidate(t *testing.T) {
	base := model.Config{SchemaVersion: model.ConfigVersion, Owner: "o", Repository: "r", DefaultBranch: "main", WikiMode: "off", Areas: []string{"core"}, AuthorizedActors: []string{"o"}}
	ok := base
	ok.ReadyBy, ok.PolicyExemptAuthors = "agents", []string{"dependabot[bot]"}
	if err := ok.Validate(); err != nil || !ok.AgentsMakeReady() {
		t.Fatal(err)
	}
	bad := base
	bad.ReadyBy = "anyone"
	if bad.Validate() == nil {
		t.Fatal("ready_by must be owner or agents")
	}
	bad = base
	bad.PolicyExemptAuthors = []string{"isnakolah"}
	if bad.Validate() == nil {
		t.Fatal("a person's login cannot be exempt: agents share it")
	}
}

func TestStartNextSkipsOwnerDecidedKinds(t *testing.T) {
	now := time.Now()
	var candidates []workCandidate
	for i, kind := range []string{"Story", "Decision", "Gate", "Epic", "Task"} {
		candidates = append(candidates, workCandidate{readyWork: readyWork{Number: i + 1, Kind: kind, Priority: "P1"}, Status: "Ready", ParentID: "P", Body: validContract()})
	}
	var kinds []string
	for _, work := range selectReadyWork(candidates, now) {
		kinds = append(kinds, candidates[work.Number-1].Kind)
	}
	if strings.Join(kinds, ",") != "Story,Task" {
		t.Fatalf("picked %v", kinds)
	}
}

func TestGuardReadyFollowsConfig(t *testing.T) {
	f := newGuardFixture(t)
	if code := guardCode(t, f.a, "Bash", map[string]any{"command": "quipu ready 5"}); code != "quipu-admin" {
		t.Fatalf("default owner-only: %q", code)
	}
	path := filepath.Join(f.a, configPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("ready_by: agents\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	if code := guardCode(t, f.a, "Bash", map[string]any{"command": "quipu ready 5"}); code != "" {
		t.Fatalf("ready_by agents: %q", code)
	}
}

func TestMergeGate(t *testing.T) {
	required := []string{"test", "policy"}
	run := func(name, status, conclusion string) prCheck {
		return prCheck{Name: name, Status: status, Conclusion: conclusion, DetailsURL: "https://x/" + name}
	}
	green := []prCheck{run("test", "COMPLETED", "SUCCESS"), run("policy", "COMPLETED", "SUCCESS"), run("writer", "COMPLETED", "FAILURE")}
	if err := mergeGate(green, required); err != nil {
		t.Fatalf("required checks green, a non-required failure ignored: %v", err)
	}
	for name, tc := range map[string]struct {
		checks []prCheck
		code   string
	}{
		"failing":        {[]prCheck{run("test", "COMPLETED", "FAILURE"), run("policy", "COMPLETED", "SUCCESS")}, "checks-failing"},
		"missing":        {[]prCheck{run("test", "COMPLETED", "SUCCESS")}, "checks-failing"},
		"running":        {[]prCheck{run("test", "IN_PROGRESS", ""), run("policy", "COMPLETED", "SUCCESS")}, "checks-pending"},
		"queued":         {[]prCheck{run("test", "QUEUED", ""), run("policy", "COMPLETED", "SUCCESS")}, "checks-pending"},
		"skipped only":   {[]prCheck{run("test", "COMPLETED", "SKIPPED"), run("policy", "COMPLETED", "SUCCESS")}, "checks-failing"},
		"status context": {[]prCheck{{Context: "test", State: "PENDING"}, run("policy", "COMPLETED", "SUCCESS")}, "checks-pending"},
		"rerun failed":   {[]prCheck{run("test", "COMPLETED", "SUCCESS"), run("test", "COMPLETED", "FAILURE"), run("policy", "COMPLETED", "SUCCESS")}, "checks-failing"},
	} {
		err := mergeGate(tc.checks, required)
		var flow *flowError
		if !errors.As(err, &flow) || flow.Code != tc.code {
			t.Errorf("%s: got %v, want %s", name, err, tc.code)
		}
	}
	if err := mergeGate([]prCheck{{Context: "test", State: "SUCCESS"}, run("policy", "COMPLETED", "SUCCESS")}, required); err != nil {
		t.Fatalf("a commit status counts like a check run: %v", err)
	}
}

func TestPolicyChecksCommitFormat(t *testing.T) {
	c := watchConfig()
	c.CommitFormat = "issue-prefix"
	work := liveWork{IssueState: "OPEN", Kind: "Story", ParentID: "P", Body: validContract(), Status: "In progress",
		Holder: "claude/owner/w1", Branch: "042/fix-it", Expiry: cutover.Add(time.Hour).Format(time.RFC3339)}
	load := func(int) (liveWork, error) { return work, nil }
	pull := pullFacts{Title: "fix(x): it", Body: "Refs #42", Author: "owner", Base: "main", Head: "042/fix-it",
		Commits: []string{"#042 fix(x): it\n\nRefs: #42", "Merge branch 'main' into 042/fix-it\n"}, Files: []string{"main.go"}}
	if v := prViolations(c, pull, load, cutover); len(v) != 0 {
		t.Fatalf("well-formed commits: %v", v)
	}
	pull.Commits = append(pull.Commits, "fix: forgot the prefix\n")
	pull.Title = "Fix it"
	v := strings.Join(prViolations(c, pull, load, cutover), "\n")
	if !strings.Contains(v, "is not `#NNN type(scope): summary`") || !strings.Contains(v, `PR title "Fix it"`) {
		t.Fatalf("violations = %s", v)
	}
	pull.Commits = []string{"#041 fix(x): it\n\nRefs: #41"}
	pull.Title = "fix(x): it"
	if v := strings.Join(prViolations(c, pull, load, cutover), "\n"); !strings.Contains(v, "names #41, but the work is #42") {
		t.Fatalf("a commit for another Issue: %s", v)
	}
	c.CommitFormat = ""
	pull.Title = "Fix it"
	if v := prViolations(c, pull, load, cutover); len(v) != 0 {
		t.Fatalf("commit_format off checks nothing: %v", v)
	}
}

func TestMergeAndCommitFormatConfig(t *testing.T) {
	base := model.Config{SchemaVersion: model.ConfigVersion, Owner: "o", Repository: "r", DefaultBranch: "main", WikiMode: "off", Areas: []string{"a"}, AuthorizedActors: []string{"o"}}
	c := base
	if c.AgentsMerge() {
		t.Fatal("agents never merge by default")
	}
	c.Merge = "agent"
	if err := c.Validate(); err != nil || !c.AgentsMerge() {
		t.Fatalf("merge agent: %v", err)
	}
	for _, bad := range []func(*model.Config){
		func(c *model.Config) { c.Merge = "anyone" },
		func(c *model.Config) { c.CommitFormat = "conventional" },
	} {
		c := base
		bad(&c)
		if err := c.Validate(); err == nil {
			t.Error("expected a validation error")
		}
	}
}

func TestVerifyCommit(t *testing.T) {
	f := newGuardFixture(t)
	t.Chdir(f.a)
	message := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	write := func(text string) {
		if err := os.WriteFile(message, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("anything goes\n")
	if err := verifyCommitCommand([]string{message}); err != nil {
		t.Fatalf("without commit_format every message passes: %v", err)
	}
	config := filepath.Join(f.a, configPath)
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(raw, []byte("commit_format: issue-prefix\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyCommitCommand([]string{message}); err == nil || !strings.Contains(err.Error(), "#001 feat(scope): summary") {
		t.Fatalf("expected the leased Issue in the example: %v", err)
	}
	write("#001 feat(x): thing\n\nRefs: #1\n# Please enter the commit message\n")
	if err := verifyCommitCommand([]string{message}); err != nil {
		t.Fatal(err)
	}
	write("#002 feat(x): thing\n\nRefs: #2\n")
	if err := verifyCommitCommand([]string{message}); err == nil {
		t.Fatal("a commit for another Issue than the lease must fail")
	}
}
