package writer

import (
	"strings"
	"testing"
	"time"
)

func TestCheckChore(t *testing.T) {
	free := WorkState{Status: "Ready"}
	held := WorkState{Status: "In progress", Lease: Lease{Holder: "claude/me/a", Expires: time.Now().Add(time.Hour)}}
	for name, tc := range map[string]struct {
		state WorkState
		r     Request
		want  string
	}{
		"assign":           {free, Request{Action: "assign", Assignee: "octo-cat"}, ""},
		"assign bad login": {free, Request{Action: "assign", Assignee: "-x"}, "GitHub login"},
		"assign no login":  {free, Request{Action: "assign"}, "GitHub login"},
		"assign spaces":    {free, Request{Action: "assign", Assignee: "a b"}, "GitHub login"},
		"edit":             {free, Request{Action: "edit", Body: "body"}, ""},
		"edit empty":       {free, Request{Action: "edit", Body: " \n"}, "new Issue body"},
		"edit marker":      {free, Request{Action: "edit", Body: "x " + ReceiptPrefix}, "markers"},
		"edit request":     {free, Request{Action: "edit", Body: "x " + RequestPrefix}, "markers"},
		"edit held":        {held, Request{Action: "edit", Body: "body", Actor: "claude/me/b"}, "held by"},
		"edit own":         {held, Request{Action: "edit", Body: "body", Actor: "claude/me/a"}, ""},
		"pr.close":         {free, Request{Action: "pr.close", PR: "https://github.com/o/r/pull/1"}, ""},
		"pr.close no PR":   {free, Request{Action: "pr.close"}, "pull request URL"},
		"not a chore":      {free, Request{Action: "status"}, "not a chore"},
	} {
		err := CheckChore(tc.state, tc.r)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	if !IsChore("assign") || !IsChore("edit") || !IsChore("pr.close") || IsChore("status") || IsChore("claim") {
		t.Fatal("chores are assign, edit and pr.close only")
	}
}

func TestChoreRequestHashesKeepOldRequestsValid(t *testing.T) {
	// The new fields are omitted when empty, so a request posted before them
	// hashes the same and still parses.
	old := Request{ID: "1", Action: "status", IssueID: "I", Actor: "a", ExpectedFingerprint: "f", Status: "Ready"}
	body, err := RenderRequest(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "assignee") || strings.Contains(body, `"body"`) {
		t.Fatalf("empty chore fields leak into %s", body)
	}
	if _, err := ParseRequest(body); err != nil {
		t.Fatal(err)
	}
}
