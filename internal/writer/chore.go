package writer

// Chores change an Issue or a PR without moving its lifecycle: assigning an
// Issue, replacing its body, closing a PR Quipu stranded. Each is one request
// with one receipt, like any other change, but its receipt carries no state.

import (
	"fmt"
	"regexp"
	"strings"
)

var choreActions = map[string]bool{"assign": true, "edit": true, "pr.close": true, "tick": true, "untick": true}

// IsChore reports whether action is a chore rather than a lifecycle move.
func IsChore(action string) bool { return choreActions[action] }

// TickableStatus reports whether work in this status may have its criteria ticked.
func TickableStatus(status string) bool {
	return status == "Claimed" || status == "In progress" || status == "In review" || status == "Evidence pending"
}

// Login is a GitHub login: letters, digits and single inner hyphens.
var Login = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// CheckChore validates what a chore request carries, and refuses an edit of work
// another agent holds, whose contract it is working to.
func CheckChore(current WorkState, r Request) error {
	switch r.Action {
	case "assign":
		if !Login.MatchString(r.Assignee) {
			return fmt.Errorf("assign needs a GitHub login, not %q", r.Assignee)
		}
	case "edit":
		if strings.TrimSpace(r.Body) == "" {
			return fmt.Errorf("edit needs the new Issue body")
		}
		if strings.Contains(r.Body, RequestPrefix) || strings.Contains(r.Body, legacyRequestPrefix) || HasReceiptMarker(r.Body) {
			return fmt.Errorf("an Issue body cannot contain Quipu request or receipt markers")
		}
		if err := sameHolder(current, r.Actor); err != nil {
			return fmt.Errorf("%w; edit the contract once it is no longer held", err)
		}
	case "tick", "untick":
		if r.Criterion < 1 {
			return fmt.Errorf("%s needs the number of an acceptance criterion, counting from 1", r.Action)
		}
		if current.Lease.Holder == "" || current.Lease.Holder != r.Actor {
			return fmt.Errorf("only the lease holder ticks criteria, and %q does not hold this work", r.Actor)
		}
		if !TickableStatus(current.Status) {
			return fmt.Errorf("criteria are ticked while work is in progress or in review, not %s", current.Status)
		}
	case "pr.close":
		if r.PR == "" {
			return fmt.Errorf("pr.close needs the pull request URL")
		}
	default:
		return fmt.Errorf("%q is not a chore", r.Action)
	}
	return nil
}
