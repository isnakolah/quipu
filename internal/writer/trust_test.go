package writer

import (
	"testing"
	"time"

	gh "github.com/isnakolah/quipu/internal/github"
)

func receiptComment(t *testing.T, r Receipt, author, created string) gh.Comment {
	t.Helper()
	body, err := RenderReceipt(r)
	if err != nil {
		t.Fatal(err)
	}
	return gh.Comment{Body: body, User: login(author), CreatedAt: created}
}

func TestTrustOnlyCountsWriterReceipts(t *testing.T) {
	cutover := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	trust := Trust{LegacyBefore: cutover}
	comments := []gh.Comment{
		receiptComment(t, Receipt{RequestID: "legacy", Result: "accepted"}, "owner", "2026-10-01T00:00:00Z"),
		receiptComment(t, Receipt{RequestID: "forged", Result: "accepted"}, "owner", "2026-10-30T00:00:00Z"),
		receiptComment(t, Receipt{RequestID: "bot", Result: "accepted"}, WriterLogin, "2026-10-30T00:00:00Z"),
	}
	receipts := trust.Receipts(comments)
	if len(receipts) != 2 || receipts[0].RequestID != "legacy" || receipts[1].RequestID != "bot" {
		t.Fatalf("receipts=%+v", receipts)
	}
	if got := (Trust{}).Receipts(comments); len(got) != 1 || got[0].RequestID != "bot" {
		t.Fatalf("without a cutover only the Writer counts: %+v", got)
	}
}

func TestForgedReceiptLeavesRequestPending(t *testing.T) {
	r := Request{ID: "request-1", Action: "claim", IssueID: "I1", Actor: "agent", ExpectedFingerprint: "f"}
	body, err := RenderRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	forged := receiptComment(t, Receipt{RequestID: r.ID, Result: "accepted"}, "agent", "2026-10-30T00:00:00Z")
	pending, _ := Pending([]gh.Comment{{ID: 1, Body: body, CreatedAt: "x", UpdatedAt: "x", User: login("agent")}, forged}, Trust{})
	if len(pending) != 1 {
		t.Fatalf("forged receipt must not answer a request: pending=%d", len(pending))
	}
}

func TestCycleEvidenceAndSnapshots(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	evidence := &Evidence{"sha", "ci", "cmd", "env", "crit", "art", "doc", "risk", "boundary"}
	receipts := []Receipt{
		{RequestID: "c1", Result: "accepted", Detail: "lifecycle state changed to Claimed", At: t0},
		{RequestID: "e1", Result: "accepted", Evidence: evidence, At: t0.Add(time.Hour)},
		{RequestID: "c2", Result: "accepted", At: t0.Add(2 * time.Hour), State: &ReceiptState{Status: "Claimed", Holder: "a"}},
	}
	if AcceptedEvidenceSince(receipts, CycleStart(receipts)) {
		t.Fatal("evidence from an earlier claim must not complete a new one")
	}
	receipts = append(receipts, Receipt{RequestID: "e2", Result: "accepted", Evidence: evidence, At: t0.Add(3 * time.Hour)})
	if !AcceptedEvidenceSince(receipts, CycleStart(receipts)) {
		t.Fatal("evidence from the current claim counts")
	}
	if last := LastState(receipts); last == nil || last.Holder != "a" {
		t.Fatalf("last=%+v", last)
	}
	if Closed(receipts) {
		t.Fatal("not closed")
	}
	receipts = append(receipts, Receipt{Result: "accepted", State: &ReceiptState{Status: "Done"}})
	if !Closed(receipts) {
		t.Fatal("closed by Done snapshot")
	}
	state := WorkState{Status: "In progress", Lease: Lease{Holder: "h", Branch: "001/x", Expires: t0}}
	if back := StateOf(state).WorkState(); back.Lease.Expires != t0 || back.Status != state.Status || back.Lease.Branch != "001/x" {
		t.Fatalf("snapshot round trip: %+v", back)
	}
}
