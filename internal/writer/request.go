package writer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const RequestPrefix = "<!-- quipu-request:v1"
const ReceiptPrefix = "<!-- quipu-receipt:v1"

// Legacy prefixes were written before the project was renamed from gfd to
// Quipu. Parsers accept both so existing Issue history stays valid; new
// comments always use the Quipu prefixes.
const legacyRequestPrefix = "<!-- gfd-request:v1"
const legacyReceiptPrefix = "<!-- gfd-receipt:v1"

// IsRequest reports whether a comment starts with a current or legacy request marker.
func IsRequest(comment string) bool {
	comment = strings.TrimSpace(comment)
	return strings.HasPrefix(comment, RequestPrefix) || strings.HasPrefix(comment, legacyRequestPrefix)
}

// HasReceiptMarker reports whether a comment contains a current or legacy receipt marker.
func HasReceiptMarker(comment string) bool {
	return strings.Contains(comment, ReceiptPrefix) || strings.Contains(comment, legacyReceiptPrefix)
}

type Request struct {
	ID                  string    `json:"id"`
	Action              string    `json:"action"`
	IssueID             string    `json:"issue_id"`
	Actor               string    `json:"actor"`
	ExpectedFingerprint string    `json:"expected_fingerprint"`
	LeaseExpiresAt      string    `json:"lease_expires_at,omitempty"`
	Branch              string    `json:"branch,omitempty"`
	Status              string    `json:"status,omitempty"`
	PR                  string    `json:"pr,omitempty"`
	Evidence            *Evidence `json:"evidence,omitempty"`
	Import              *Import   `json:"import,omitempty"`
}

// Import records where an imported Issue stood before Quipu. The Writer
// applies it once, to an Issue that has not entered the flow yet.
type Import struct {
	Key     string   `json:"key"`
	Status  string   `json:"status"`
	Source  string   `json:"source,omitempty"`
	Commits []string `json:"commits,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// HistoricalBoundary marks evidence for work finished before Quipu.
const HistoricalBoundary = "local, before Quipu"

// HistoricalEvidence is the evidence an imported Done Issue closes with:
// where the record came from and the commits that did the work.
func HistoricalEvidence(i Import) *Evidence {
	sha := "None: before Quipu"
	if len(i.Commits) > 0 {
		sha = strings.Join(i.Commits, ", ")
	}
	source := i.Source
	if source == "" {
		source = "import manifest"
	}
	return &Evidence{
		FinalSHA: sha, CIURL: "None: before Quipu", Commands: "None: before Quipu", Environments: HistoricalBoundary,
		Criteria: "Imported as Done from " + source, Artifacts: source, Documentation: "None: imported history",
		Risks: "Not verified by Quipu: finished before the repository adopted it", Boundary: HistoricalBoundary,
	}
}

type Evidence struct{ FinalSHA, CIURL, Commands, Environments, Criteria, Artifacts, Documentation, Risks, Boundary string }

func (e Evidence) Validate() error {
	if e.FinalSHA == "" || e.CIURL == "" || e.Commands == "" || e.Environments == "" || e.Criteria == "" || e.Artifacts == "" || e.Documentation == "" || e.Risks == "" || e.Boundary == "" {
		return fmt.Errorf("evidence requires final SHA, CI URL, commands, environments, criteria, artifacts, documentation, risks, and proof boundary")
	}
	return nil
}

type Receipt struct {
	RequestID   string        `json:"request_id"`
	Fingerprint string        `json:"fingerprint"`
	Result      string        `json:"result"`
	Detail      string        `json:"detail"`
	Evidence    *Evidence     `json:"evidence,omitempty"`
	At          time.Time     `json:"at"`
	State       *ReceiptState `json:"state,omitempty"`
}

func HashRequest(r Request) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func RenderRequest(r Request) (string, error) {
	hash, err := HashRequest(r)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("<!-- quipu-request:v1 id=%s sha256=%s -->\n```json\n%s\n```", r.ID, hash, payload), nil
}
func RenderReceipt(r Receipt) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("<!-- quipu-receipt:v1 request=%s fingerprint=%s -->\n```json\n%s\n```", r.RequestID, r.Fingerprint, raw), nil
}

func ParseRequest(comment string) (Request, error) {
	var r Request
	if !IsRequest(comment) {
		return r, fmt.Errorf("not a quipu request")
	}
	start, end := strings.Index(comment, "```json"), strings.LastIndex(comment, "```")
	if start < 0 || end <= start {
		return r, fmt.Errorf("request JSON missing")
	}
	payload := strings.TrimSpace(comment[start+len("```json") : end])
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		return r, err
	}
	if r.ID == "" || r.Action == "" || r.IssueID == "" || r.Actor == "" || r.ExpectedFingerprint == "" {
		return r, fmt.Errorf("request has required fields missing")
	}
	hash, err := HashRequest(r)
	if err != nil {
		return r, err
	}
	marker := fmt.Sprintf("id=%s sha256=%s", r.ID, hash)
	if !strings.Contains(comment, marker) {
		return r, fmt.Errorf("request hash mismatch")
	}
	return r, nil
}

// ParseReceipt decodes a Writer receipt without treating free-form comments as
// evidence. Completion may rely only on a prior accepted evidence receipt.
func ParseReceipt(comment string) (Receipt, error) {
	var receipt Receipt
	if !strings.HasPrefix(strings.TrimSpace(comment), ReceiptPrefix) && !strings.HasPrefix(strings.TrimSpace(comment), legacyReceiptPrefix) {
		return receipt, fmt.Errorf("not a quipu receipt")
	}
	start, end := strings.Index(comment, "```json"), strings.LastIndex(comment, "```")
	if start < 0 || end <= start {
		return receipt, fmt.Errorf("receipt JSON missing")
	}
	payload := strings.TrimSpace(comment[start+len("```json") : end])
	if err := json.Unmarshal([]byte(payload), &receipt); err != nil {
		return receipt, err
	}
	if receipt.RequestID == "" {
		return receipt, fmt.Errorf("receipt request id missing")
	}
	if !strings.Contains(comment, "request="+receipt.RequestID) {
		return receipt, fmt.Errorf("receipt request id marker mismatch")
	}
	return receipt, nil
}

// HasAcceptedEvidence accepts only receipts emitted for validated evidence.
// Edited or free-form Issue comments can never satisfy completion.
func HasAcceptedEvidence(comments []string) bool {
	for _, comment := range comments {
		receipt, err := ParseReceipt(comment)
		if err == nil && receipt.Result == "accepted" && receipt.Evidence != nil && receipt.Evidence.Validate() == nil {
			return true
		}
	}
	return false
}
