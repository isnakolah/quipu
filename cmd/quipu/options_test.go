package main

import (
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
