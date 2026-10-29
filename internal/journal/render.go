package journal

import (
	"fmt"
	"strings"
)

type Entry struct{ RequestID, Date, Issue, PR, Outcome, Proof, Boundary, NextBlocker string }

func Render(entry Entry) string {
	return fmt.Sprintf("<!-- quipu-journal:v1 request=%s -->\n## %s — %s\n\n- Issue: %s\n- PR: %s\n- Outcome: %s\n- Proof: %s\n- Boundary: %s\n- Next blocker: %s\n", entry.RequestID, entry.Date, entry.Issue, entry.Issue, entry.PR, entry.Outcome, entry.Proof, entry.Boundary, entry.NextBlocker)
}
func HasRequest(log, requestID string) bool {
	// Entries written before the rename from gfd keep their legacy marker.
	return strings.Contains(log, "<!-- quipu-journal:v1 request="+requestID+" -->") || strings.Contains(log, "<!-- gfd-journal:v1 request="+requestID+" -->")
}

// Append keeps generated journal idempotent. It never derives delivery state.
func Append(log string, entry Entry) string {
	if HasRequest(log, entry.RequestID) {
		return log
	}
	if log != "" && !strings.HasSuffix(log, "\n") {
		log += "\n"
	}
	return log + Render(entry)
}
