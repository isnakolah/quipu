package model

import (
	"strings"
	"testing"
)

func TestSimilarIssuesFindsTheSameWork(t *testing.T) {
	open := []OpenIssue{
		{Number: 1, Title: "Sync offline edits on reconnect"},
		{Number: 2, Title: "Render the weekly review on Friday"},
		{Number: 3, Title: "Something else", Body: "<!-- work:v1 -->\n## Outcome\n\nOffline edits sync to the server once the device reconnects.\n\n## Scope\n"},
	}
	got := SimilarIssues("Sync the offline edits when reconnecting on reconnect", "", open)
	if len(got) != 1 || got[0].Number != 1 || got[0].Score < DuplicateThreshold {
		t.Fatalf("title match: %+v", got)
	}
	body := "<!-- work:v1 -->\n## Outcome\n\nOffline edits sync to the server once the device reconnects.\n"
	got = SimilarIssues("A different title entirely", body, open)
	if len(got) != 1 || got[0].Number != 3 {
		t.Fatalf("Outcome match: %+v", got)
	}
	if got := SimilarIssues("Readable Quipu comments", "", open); len(got) != 0 {
		t.Fatalf("unrelated work matched: %+v", got)
	}
	w := DuplicateWarnings([]Similar{{Number: 1, Title: "Sync offline edits on reconnect", Score: 0.75}})
	if len(w) != 1 || !strings.Contains(w[0], "#1") || !strings.Contains(w[0], "75%") {
		t.Fatalf("warning: %v", w)
	}
}
