package writer

import (
	"fmt"
	"strings"
	"time"

	gh "github.com/isnakolah/quipu/internal/github"
)

// PendingRequest is a validated request comment eligible for exactly one receipt.
// This layer owns durable parsing, tamper rejection, edit rejection, and idempotency
// detection before canonical Writer lifecycle mutation.
type PendingRequest struct {
	Request   Request
	CommentID int64
	Author    string
}

// Pending returns the requests without a trusted receipt, and receipts for
// requests that are malformed, edited, or posted under someone else's name.
func Pending(comments []gh.Comment, trust Trust) ([]PendingRequest, []Receipt) {
	receipted := make(map[string]bool)
	for _, comment := range comments {
		if HasReceiptMarker(comment.Body) && trust.Trusted(comment) {
			for _, id := range receiptRequestIDs(comment.Body) {
				receipted[id] = true
			}
		}
	}
	var pending []PendingRequest
	var rejected []Receipt
	for _, comment := range comments {
		if !IsRequest(comment.Body) {
			continue
		}
		if receipted[requestID(comment.Body)] {
			continue
		}
		r, err := ParseRequest(comment.Body)
		if err != nil {
			rejected = append(rejected, Receipt{RequestID: requestID(comment.Body), Result: "rejected", Detail: "invalid request: " + err.Error(), At: time.Now().UTC()})
			continue
		}
		if comment.CreatedAt != "" && comment.UpdatedAt != "" && comment.CreatedAt != comment.UpdatedAt {
			rejected = append(rejected, Receipt{RequestID: r.ID, Result: "rejected", Detail: "request comment was edited; submit a new request", At: time.Now().UTC()})
			continue
		}
		if !actorMatches(r.Actor, comment.User.Login) {
			rejected = append(rejected, Receipt{RequestID: r.ID, Result: "rejected", Detail: "request actor does not match comment author", At: time.Now().UTC()})
			continue
		}
		if !receipted[r.ID] {
			pending = append(pending, PendingRequest{Request: r, CommentID: comment.ID, Author: comment.User.Login})
		}
	}
	return pending, rejected
}

// ActorMatchesLogin reports whether a request actor may be posted by login:
// either the login itself or a provider/login[/session] agent identity.
func ActorMatchesLogin(actor, login string) bool { return actorMatches(actor, login) }

func actorMatches(actor, login string) bool {
	if actor == "" || login == "" {
		return false
	}
	return actor == login || strings.Contains(actor, "/"+login+"/") || strings.HasSuffix(actor, "/"+login)
}

// AcceptanceReceipt records one validated request. State-changing lifecycle work is
// intentionally delegated to the later lease/fingerprint Writer stories.
func AcceptanceReceipt(request Request) Receipt {
	return Receipt{RequestID: request.ID, Fingerprint: request.ExpectedFingerprint, Result: "accepted", Detail: "durable request recorded; lifecycle mutation pending Writer support", At: time.Now().UTC()}
}

func receiptRequestIDs(body string) []string {
	const key = "request="
	var ids []string
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, key); i >= 0 {
			value := strings.Fields(line[i+len(key):])
			if len(value) > 0 {
				ids = append(ids, strings.Trim(value[0], "->"))
			}
		}
	}
	return ids
}

func requestID(body string) string {
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "id=") {
			return strings.Trim(strings.TrimPrefix(field, "id="), "->")
		}
	}
	return "unknown"
}

func RejectionComment(r Receipt) (string, error) {
	if r.RequestID == "" {
		return "", fmt.Errorf("receipt request id required")
	}
	return RenderReceipt(r)
}
