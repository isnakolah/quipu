package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/isnakolah/quipu/internal/writer"
)

// statusReceiptPending is what done reports when the PR merged but the Writer
// had not answered before the wait ran out. Nothing is lost: the request stays
// queued and the Writer closes the Issue when it runs.
const statusReceiptPending = "Merged, receipt pending"

// receiptPending turns a no-receipt after a merge into a result, not an error.
// The merge is the agent's last act; recording it is the Writer's job, and an
// agent told "rerun later" by a failing command stalls its whole loop.
func receiptPending(number int, pr string, err error) (flowResult, bool) {
	if !errors.Is(err, errNoReceipt) {
		return flowResult{}, false
	}
	return flowResult{
		Issue:  number,
		Status: statusReceiptPending,
		PR:     pr,
		Next: fmt.Sprintf("The PR merged and your request waits for the Writer, which closes #%d when it runs; nothing to redo. "+
			"Take other work: quipu start --next --worktree --json. Later, quipu done %d --json frees this worktree.", number, number),
	}, true
}

// doneAfterEvidence is how long the Writer leaves accepted evidence for the
// agent's own Done request before closing the work itself.
const doneAfterEvidence = 10 * time.Minute

// latestEvidence is the newest accepted evidence receipt of the current claim.
func latestEvidence(receipts []writer.Receipt) (writer.Receipt, bool) {
	since := writer.CycleStart(receipts)
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if r.Result == "accepted" && r.Evidence != nil && r.Evidence.Validate() == nil && !r.At.Before(since) {
			return r, true
		}
	}
	return writer.Receipt{}, false
}

// closeEvidenced closes work the Writer already holds evidence for when the
// agent's Done never arrived: its quipu done ran out of wait after the merge.
// Accepted evidence was checked against the merged PR, so Done adds nothing to
// verify. A queued merge closes its own work.
func (w *watch) closeEvidenced(state *liveWork) (bool, error) {
	if state.Status != "Evidence pending" || state.Holder == "" {
		return false, nil
	}
	if _, queued := writer.LatestQueuedMerge(w.receipts); queued {
		return false, nil
	}
	evidence, ok := latestEvidence(w.receipts)
	if !ok || w.now.Sub(evidence.At) < doneAfterEvidence {
		return false, nil
	}
	if !w.apply {
		w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("#%d has accepted evidence and no Done; the Writer closes it", w.number))
		return false, nil
	}
	fingerprint, err := fingerprintLiveWork(*state)
	if err != nil {
		return false, err
	}
	request := writer.Request{ID: "evidence-done-" + evidence.RequestID, Action: "status", Status: "Done", IssueID: state.IssueID, Actor: state.Holder, ExpectedFingerprint: fingerprint}
	receipt := w.ops.apply(w.number, request, w.receipts)
	if receipt.Result != "accepted" {
		w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("close #%d after its evidence: %s", w.number, receipt.Detail))
		return false, nil
	}
	return true, w.post(receipt)
}
