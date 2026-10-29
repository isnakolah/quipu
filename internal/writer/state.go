package writer

import (
	"fmt"
	"time"
)

var transitions = map[string]map[string]bool{
	"Backlog": {"Ready": true}, "Ready": {"Claimed": true, "Blocked": true, "Backlog": true},
	"Claimed":          {"In progress": true, "Blocked": true, "Ready": true},
	"In progress":      {"In review": true, "Blocked": true, "Ready": true},
	"In review":        {"Evidence pending": true, "In progress": true, "Blocked": true},
	"Evidence pending": {"Done": true, "In progress": true, "Blocked": true},
	"Blocked":          {"Ready": true, "In progress": true, "Backlog": true},
	"Done":             {"Archived": true},
	"Cancelled":        {"Archived": true},
}

func CanTransition(from, to string) bool { return transitions[from][to] }
func RequireTransition(from, to string) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid lifecycle transition %s -> %s", from, to)
	}
	return nil
}
func LeaseExpired(expiry string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, expiry)
	return err == nil && !t.After(now.UTC())
}
