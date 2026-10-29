package writer

import (
	"strings"
	"testing"
)

func TestRequestRoundTripAndTamper(t *testing.T) {
	r := Request{ID: "r1", Action: "claim", IssueID: "I1", Actor: "agent", ExpectedFingerprint: "f"}
	body, err := RenderRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != r.ID {
		t.Fatalf("got %q", got.ID)
	}
	if _, err := ParseRequest(strings.Replace(body, "sha256=", "sha256=bad", 1)); err == nil {
		t.Fatal("expected tamper error")
	}
}
func TestTransitions(t *testing.T) {
	if !CanTransition("Ready", "Claimed") || CanTransition("Done", "Ready") {
		t.Fatal("bad transitions")
	}
}

func TestAcceptedEvidenceRequiresWriterReceipt(t *testing.T) {
	evidence := &Evidence{FinalSHA: "sha", CIURL: "https://ci", Commands: "go test", Environments: "CI", Criteria: "pass", Artifacts: "None: retained in CI", Documentation: "docs", Risks: "None", Boundary: "CI"}
	receipt, err := RenderReceipt(Receipt{RequestID: "e1", Result: "accepted", Evidence: evidence})
	if err != nil {
		t.Fatal(err)
	}
	if !HasAcceptedEvidence([]string{receipt}) {
		t.Fatal("accepted evidence receipt was not recognized")
	}
	if HasAcceptedEvidence([]string{"evidence: trust me"}) {
		t.Fatal("free-form evidence was accepted")
	}
}

func TestLegacyGfdMarkersStillParse(t *testing.T) {
	r := Request{ID: "r1", Action: "claim", IssueID: "I1", Actor: "agent", ExpectedFingerprint: "f"}
	body, err := RenderRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, RequestPrefix) {
		t.Fatalf("new request must use Quipu marker: %s", body)
	}
	legacy := strings.Replace(body, RequestPrefix, legacyRequestPrefix, 1)
	if got, err := ParseRequest(legacy); err != nil || got.ID != "r1" {
		t.Fatalf("legacy request: %+v %v", got, err)
	}
	receipt, err := RenderReceipt(Receipt{RequestID: "r1", Result: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	legacyReceipt := strings.Replace(receipt, ReceiptPrefix, legacyReceiptPrefix, 1)
	if got, err := ParseReceipt(legacyReceipt); err != nil || got.RequestID != "r1" {
		t.Fatalf("legacy receipt: %+v %v", got, err)
	}
	if !HasReceiptMarker(legacyReceipt) || !IsRequest(legacy) {
		t.Fatal("legacy markers not recognized")
	}
}
