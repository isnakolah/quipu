package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

func TestIssueCreateWarnsAboutLikelyDuplicates(t *testing.T) {
	config, err := os.ReadFile(filepath.Join("..", "..", configPath))
	if err != nil {
		t.Fatal(err)
	}
	fixtureOpenIssues = []model.OpenIssue{{Number: 9, Title: "Sync offline edits on reconnect"}, {Number: 10, Title: "Unrelated chore"}}
	t.Cleanup(func() { fixtureOpenIssues = nil })
	output, filed, err := issueCreateFixture(t, config, testContract)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if len(filed) != 1 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "open #9") {
		t.Fatalf("a likely duplicate warns and the Issue is still filed: filed %+v, %s", filed, output)
	}
}

func TestStaleReadyWorkWaitsOnTheOwner(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	items := []statusItem{
		{Number: 1, Title: "old", Status: "Ready", UpdatedAt: now.Add(-31 * 24 * time.Hour)},
		{Number: 2, Title: "fresh", Status: "Ready", UpdatedAt: now.Add(-29 * 24 * time.Hour)},
		{Number: 3, Title: "old but held", Status: "In progress", UpdatedAt: now.Add(-90 * 24 * time.Hour)},
	}
	c := model.Config{}
	queue := withStaleReady(nil, items, c.StaleReadyAfter(), now)
	if len(queue) != 1 || queue[0].Number != 1 || !strings.Contains(queue[0].Reason, "untouched for 31 days") {
		t.Fatalf("stale Ready work: %+v", queue)
	}
	off := 0
	c.StaleReadyDays = &off
	if queue := withStaleReady(nil, items, c.StaleReadyAfter(), now); len(queue) != 0 {
		t.Fatalf("stale_ready_days: 0 turns it off: %+v", queue)
	}
	d := buildDigest("octo/repo", digestInputs{OwnerQueue: withStaleReady(nil, items, model.Config{}.StaleReadyAfter(), now)}, now.Add(-24*time.Hour), now)
	if len(d.OwnerQueue) != 1 || d.OwnerQueue[0].Number != 1 {
		t.Fatalf("the digest carries stale Ready work: %+v", d.OwnerQueue)
	}
}

func TestValidateListsContractWarningsPerIssue(t *testing.T) {
	weak := strings.Replace(testContract, "Offline edits sync once the device reconnects.", "Implement sync so it works properly.", 1)
	if w := openContractWarnings("OPEN", "Story", "Sync offline edits", weak); len(w) == 0 {
		t.Fatal("a weak open contract draws no warnings")
	}
	if w := openContractWarnings("CLOSED", "Story", "Sync offline edits", weak); len(w) != 0 {
		t.Fatalf("closed Issues are left alone: %v", w)
	}
	if w := openContractWarnings("OPEN", "Story", "Sync offline edits", testContract); len(w) != 0 {
		t.Fatalf("a clean contract is quiet: %v", w)
	}
	out, _ := json.Marshal(issueWarnings{Issue: 7, Warnings: []string{"x"}})
	if string(out) != `{"issue":7,"warnings":["x"]}` {
		t.Fatalf("validate --json shape: %s", out)
	}
}
