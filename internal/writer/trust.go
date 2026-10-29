package writer

import (
	"strings"
	"time"

	gh "github.com/isnakolah/quipu/internal/github"
)

// WriterLogin authors trusted receipts. The Writer workflow posts receipts
// with the workflow's own GITHUB_TOKEN, which no agent or local tool holds,
// so a receipt-shaped comment from anyone else is ignored.
const WriterLogin = "github-actions[bot]"

// Trust decides which receipt comments count. Before LegacyBefore, receipts
// were posted with the Writer's personal token; those stay trusted so
// existing history keeps its meaning.
type Trust struct {
	LegacyBefore time.Time
}

// Trusted reports whether comment was written by the Writer.
func (t Trust) Trusted(comment gh.Comment) bool {
	if comment.User.Login == WriterLogin {
		return true
	}
	if t.LegacyBefore.IsZero() {
		return false
	}
	created, err := time.Parse(time.RFC3339, comment.CreatedAt)
	return err == nil && created.Before(t.LegacyBefore)
}

// Receipts returns the trusted receipts in comment order.
func (t Trust) Receipts(comments []gh.Comment) []Receipt {
	var receipts []Receipt
	for _, comment := range comments {
		if !HasReceiptMarker(comment.Body) || !t.Trusted(comment) {
			continue
		}
		if receipt, err := ParseReceipt(comment.Body); err == nil {
			receipts = append(receipts, receipt)
		}
	}
	return receipts
}

// ReceiptState is the lifecycle state a receipt left behind. The watchdog
// restores it when someone edits the Project by hand.
type ReceiptState struct {
	Status  string `json:"status"`
	Holder  string `json:"holder,omitempty"`
	Expires string `json:"expires,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// StateOf snapshots a work state for a receipt.
func StateOf(state WorkState) *ReceiptState {
	snapshot := &ReceiptState{Status: state.Status, Holder: state.Lease.Holder, Branch: state.Lease.Branch}
	if !state.Lease.Expires.IsZero() {
		snapshot.Expires = state.Lease.Expires.UTC().Format(time.RFC3339)
	}
	return snapshot
}

// WorkState turns a snapshot back into a state the Writer can apply.
func (s ReceiptState) WorkState() WorkState {
	state := WorkState{Status: s.Status, Lease: Lease{Holder: s.Holder, Branch: s.Branch}}
	state.Lease.Expires, _ = time.Parse(time.RFC3339, s.Expires)
	return state
}

// LastState is the state left by the latest accepted receipt that recorded one.
func LastState(receipts []Receipt) *ReceiptState {
	for i := len(receipts) - 1; i >= 0; i-- {
		if receipts[i].Result == "accepted" && receipts[i].State != nil {
			return receipts[i].State
		}
	}
	return nil
}

// CycleStart is when the current claim began: the latest accepted claim
// receipt. Evidence from an earlier cycle does not count for Done.
func CycleStart(receipts []Receipt) time.Time {
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if r.Result != "accepted" {
			continue
		}
		if (r.State != nil && r.State.Status == "Claimed") || r.Detail == "lifecycle state changed to Claimed" {
			return r.At
		}
	}
	return time.Time{}
}

// AcceptedEvidenceSince reports a valid accepted evidence receipt at or after since.
func AcceptedEvidenceSince(receipts []Receipt, since time.Time) bool {
	for _, r := range receipts {
		if r.Result == "accepted" && r.Evidence != nil && r.Evidence.Validate() == nil && !r.At.Before(since) {
			return true
		}
	}
	return false
}

// Closed reports whether the receipts end with the Writer closing the work.
func Closed(receipts []Receipt) bool {
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if r.Result != "accepted" {
			continue
		}
		if r.State != nil {
			return r.State.Status == "Done" || r.State.Status == "Cancelled"
		}
		if strings.Contains(r.Detail, "Done and Issue closed") {
			return true
		}
	}
	return false
}
