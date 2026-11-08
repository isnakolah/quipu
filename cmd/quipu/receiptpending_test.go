package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

func TestDoneAfterAMergeNeverFailsOnASlowWriter(t *testing.T) {
	slow := flowErr("no-receipt", "#7 evidence.submit request 1 has no Writer receipt after 3m0s")
	slow.cause = errNoReceipt
	result, ok := receiptPending(7, "https://github.com/o/r/pull/9", slow)
	if !ok || result.Status != statusReceiptPending || result.PR == "" {
		t.Fatalf("a merged PR with no receipt yet is a result: %+v %v", result, ok)
	}
	if !strings.Contains(result.Next, "quipu start --next") || !strings.Contains(result.Next, "nothing to redo") {
		t.Fatalf("next sends the agent to other work: %q", result.Next)
	}
	rejected := flowErr("rejected", "no")
	rejected.cause = errRejected
	for _, err := range []error{rejected, errors.New("network down")} {
		if _, ok := receiptPending(7, "", err); ok {
			t.Fatalf("only a missing receipt is pending, not %v", err)
		}
	}
}

// evidenceFixture is Work In Evidence pending whose evidence the Writer
// accepted at, with no Done after it.
func evidenceFixture(t *testing.T, at time.Time) *queueFixture {
	f := newQueueFixture(t, backgroundMerge, greenView())
	f.state.Status = "Evidence pending"
	f.w.receipts = []writer.Receipt{{RequestID: "e1", Result: "accepted", At: at,
		Evidence: &writer.Evidence{FinalSHA: "merge-sha", CIURL: "https://github.com/o/r/actions/runs/1", Commands: "go test", Environments: "CI",
			Criteria: "met", Artifacts: "None", Documentation: "None: x", Risks: "None", Boundary: "CI"}}}
	return f
}

func TestTheWriterClosesWorkItsEvidenceProved(t *testing.T) {
	f := evidenceFixture(t, time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC))
	closed, err := f.w.closeEvidenced(f.state)
	if err != nil || !closed {
		t.Fatalf("%v %v", closed, err)
	}
	if len(f.requests) != 1 || f.requests[0].Status != "Done" || f.requests[0].Actor != "claude/o/1" || f.requests[0].ID != "evidence-done-e1" {
		t.Fatalf("Done as the holder, keyed to the evidence: %+v", f.requests)
	}
	if len(f.posted) != 1 {
		t.Fatalf("the receipt is posted: %v", f.posted)
	}
}

func TestTheWriterLeavesFreshEvidenceToTheAgent(t *testing.T) {
	f := evidenceFixture(t, time.Date(2026, 11, 2, 11, 55, 0, 0, time.UTC))
	if closed, _ := f.w.closeEvidenced(f.state); closed || len(f.requests) != 0 {
		t.Fatalf("evidence five minutes old waits for the agent's own Done: %+v", f.requests)
	}

	f = evidenceFixture(t, time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC))
	f.state.Status = "In review"
	if closed, _ := f.w.closeEvidenced(f.state); closed {
		t.Fatal("only Evidence pending work is closed")
	}

	f = evidenceFixture(t, time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC))
	f.w.receipts = append(f.w.receipts, writer.Receipt{RequestID: "c2", Result: "accepted", At: time.Date(2026, 11, 2, 11, 30, 0, 0, time.UTC),
		State: &writer.ReceiptState{Status: "Claimed"}})
	if closed, _ := f.w.closeEvidenced(f.state); closed {
		t.Fatal("evidence from an earlier claim never closes the current one")
	}

	f = evidenceFixture(t, time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC))
	f.w.apply = false
	if closed, _ := f.w.closeEvidenced(f.state); closed || len(f.requests) != 0 || len(f.w.report.Warnings) != 1 {
		t.Fatalf("a dry run only reports: %+v %v", f.requests, f.w.report.Warnings)
	}
}

func TestDoctorPointsTheWriterOffASharedMachine(t *testing.T) {
	if _, warn := writerPlacementCheck(model.Workflows{}); warn {
		t.Fatal("a hosted Writer is fine")
	}
	shared := model.Workflows{RunsOn: map[string][]string{"default": {"self-hosted", "macOS"}}}
	check, warn := writerPlacementCheck(shared)
	if !warn || !strings.Contains(check.Detail, "shares the runner with the test job") || !strings.Contains(check.Fix, "runs_on.writer: [ubuntu-latest]") || !strings.Contains(check.Fix, "writer_mode: serial") {
		t.Fatalf("a Writer sharing the build machine is flagged: %+v", check)
	}
	alone := model.Workflows{WriterMode: "serial", RunsOn: map[string][]string{"default": {"self-hosted", "macOS"}, "writer": {"Self-Hosted", "linux"}}}
	check, warn = writerPlacementCheck(alone)
	if !warn || strings.Contains(check.Detail, "shares") || strings.Contains(check.Fix, "writer_mode") {
		t.Fatalf("a self-hosted Writer on its own runner only warns about sleep: %+v", check)
	}
	moved := model.Workflows{RunsOn: map[string][]string{"default": {"self-hosted"}, "writer": {"ubuntu-latest"}}}
	if _, warn := writerPlacementCheck(moved); warn {
		t.Fatal("runs_on.writer: ubuntu-latest clears the warning")
	}
}
