package writer

import (
	"testing"

	gh "github.com/isnakolah/quipu/internal/github"
)

func TestPendingRejectsEditedAndSkipsReceipted(t *testing.T) {
	r := Request{ID: "request-1", Action: "claim", IssueID: "I1", Actor: "agent", ExpectedFingerprint: "f"}
	body, err := RenderRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := RenderReceipt(Receipt{RequestID: r.ID, Fingerprint: "f", Result: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	comments := []gh.Comment{{ID: 1, Body: body, CreatedAt: "x", UpdatedAt: "x", User: login("agent")}, {ID: 2, Body: receipt, User: login(WriterLogin)}}
	pending, rejected := Pending(comments, Trust{})
	if len(pending) != 0 || len(rejected) != 0 {
		t.Fatalf("pending=%d rejected=%d", len(pending), len(rejected))
	}
	comments = []gh.Comment{{ID: 1, Body: body, CreatedAt: "x", UpdatedAt: "y", User: login("agent")}}
	pending, rejected = Pending(comments, Trust{})
	if len(pending) != 0 || len(rejected) != 1 {
		t.Fatalf("pending=%d rejected=%d", len(pending), len(rejected))
	}
}

func TestPendingRequiresCommentActorAssociation(t *testing.T) {
	r := Request{ID: "request-1", Action: "claim", IssueID: "I1", Actor: "codex/other/session", ExpectedFingerprint: "f"}
	body, err := RenderRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	pending, rejected := Pending([]gh.Comment{{ID: 1, Body: body, CreatedAt: "x", UpdatedAt: "x", User: login("agent")}}, Trust{})
	if len(pending) != 0 || len(rejected) != 1 {
		t.Fatalf("pending=%d rejected=%d", len(pending), len(rejected))
	}
}

func login(name string) struct {
	Login string `json:"login"`
} {
	return struct {
		Login string `json:"login"`
	}{Login: name}
}
