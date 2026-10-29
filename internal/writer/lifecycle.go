package writer

import (
	"fmt"
	"time"
)

// ApplyLifecycle turns one durable request into a complete Project-state value.
// Network code applies this value only after fresh fingerprint verification.
func ApplyLifecycle(current WorkState, request Request, now time.Time) (WorkState, error) {
	now = now.UTC()
	if request.Action == "status" && current.Status == "" {
		if request.Status != "Backlog" {
			return WorkState{}, fmt.Errorf("initial status must be Backlog")
		}
		current.Status = "Backlog"
		return current, nil
	}
	if current.Status == "" {
		current.Status = "Backlog"
	}
	switch request.Action {
	case "pr.link":
		if request.PR == "" {
			return WorkState{}, fmt.Errorf("PR link requires pull request URL")
		}
		if err := activeHolder(current, request.Actor, now); err != nil {
			return WorkState{}, err
		}
		if err := RequireTransition(current.Status, "In review"); err != nil {
			return WorkState{}, err
		}
		current.Status = "In review"
		return current, nil
	case "evidence.submit":
		if request.Evidence == nil {
			return WorkState{}, fmt.Errorf("evidence request requires evidence payload")
		}
		if err := request.Evidence.Validate(); err != nil {
			return WorkState{}, err
		}
		if request.PR == "" {
			return WorkState{}, fmt.Errorf("evidence request requires pull request URL")
		}
		if err := sameHolder(current, request.Actor); err != nil {
			return WorkState{}, err
		}
		if err := RequireTransition(current.Status, "Evidence pending"); err != nil {
			return WorkState{}, err
		}
		current.Status = "Evidence pending"
		return current, nil
	case "status":
		if request.Status == "" {
			return WorkState{}, fmt.Errorf("status request requires status")
		}
		// Claiming and starting set the lease, so they have their own actions.
		if request.Status == "Claimed" || request.Status == "In progress" && current.Status == "Claimed" {
			return WorkState{}, fmt.Errorf("use the claim and start actions to take work, not status %q", request.Status)
		}
		if err := sameHolder(current, request.Actor); err != nil {
			return WorkState{}, err
		}
		if err := RequireTransition(current.Status, request.Status); err != nil {
			return WorkState{}, err
		}
		current.Status = request.Status
		return current, nil
	case "claim":
		if err := RequireTransition(current.Status, "Claimed"); err != nil {
			return WorkState{}, err
		}
		if current.Lease.Holder != "" && current.Lease.Expires.After(now) {
			return WorkState{}, fmt.Errorf("issue already leased by %s until %s", current.Lease.Holder, current.Lease.Expires.UTC().Format(time.RFC3339))
		}
		if request.Actor == "" || request.Branch == "" {
			return WorkState{}, fmt.Errorf("claim requires actor and branch")
		}
		expires, err := requestExpiry(request.LeaseExpiresAt, now)
		if err != nil {
			return WorkState{}, err
		}
		return WorkState{Status: "Claimed", Lease: Lease{Holder: request.Actor, Expires: expires, Branch: request.Branch}}, nil
	case "start":
		if err := activeHolder(current, request.Actor, now); err != nil {
			return WorkState{}, err
		}
		if err := RequireTransition(current.Status, "In progress"); err != nil {
			return WorkState{}, err
		}
		current.Status = "In progress"
		return current, nil
	case "renew":
		// The holder may re-take an expired lease until reconcile returns the
		// work to Ready, and work in review keeps its holder indefinitely.
		if err := sameHolder(current, request.Actor); err != nil {
			return WorkState{}, err
		}
		if current.Status != "Claimed" && current.Status != "In progress" && current.Status != "In review" && current.Status != "Evidence pending" {
			return WorkState{}, fmt.Errorf("renew applies to active work, not %q", current.Status)
		}
		expires, err := requestExpiry(request.LeaseExpiresAt, now)
		if err != nil {
			return WorkState{}, err
		}
		current.Lease.Expires = expires
		return current, nil
	case "release":
		if err := activeHolder(current, request.Actor, now); err != nil {
			return WorkState{}, err
		}
		if err := RequireTransition(current.Status, "Ready"); err != nil {
			return WorkState{}, err
		}
		return WorkState{Status: "Ready", Lease: Lease{Branch: current.Lease.Branch}}, nil
	case "block":
		if current.Lease.Holder != "" {
			if err := activeHolder(current, request.Actor, now); err != nil {
				return WorkState{}, err
			}
		}
		if err := RequireTransition(current.Status, "Blocked"); err != nil {
			return WorkState{}, err
		}
		return WorkState{Status: "Blocked", Lease: Lease{Branch: current.Lease.Branch}}, nil
	case "import":
		return applyImport(current, request)
	case "resume":
		if err := RequireTransition(current.Status, "Ready"); err != nil {
			return WorkState{}, err
		}
		return WorkState{Status: "Ready", Lease: Lease{Branch: current.Lease.Branch}}, nil
	default:
		return WorkState{}, fmt.Errorf("unsupported lifecycle action %q", request.Action)
	}
}

// importedStates maps a manifest status to the state the Writer records.
// In progress work has no lease yet, so it waits in Ready on its branch
// until an agent resumes it with quipu start.
var importedStates = map[string]string{
	"Ready": "Ready", "Blocked": "Blocked", "In progress": "Ready", "Done": "Done", "Cancelled": "Cancelled",
}

func applyImport(current WorkState, request Request) (WorkState, error) {
	if request.Import == nil {
		return WorkState{}, fmt.Errorf("import request requires an import record")
	}
	if current.Status != "Backlog" || current.Lease.Holder != "" {
		return WorkState{}, fmt.Errorf("import applies only to new Backlog work, not %q", current.Status)
	}
	status, ok := importedStates[request.Import.Status]
	if !ok {
		return WorkState{}, fmt.Errorf("cannot import status %q", request.Import.Status)
	}
	if request.Import.Status == "Cancelled" && request.Import.Reason == "" {
		return WorkState{}, fmt.Errorf("imported Cancelled work needs a reason")
	}
	next := WorkState{Status: status}
	if request.Import.Status == "In progress" {
		next.Lease.Branch = request.Branch
	}
	return next, nil
}

func activeHolder(current WorkState, actor string, now time.Time) error {
	if actor == "" || current.Lease.Holder != actor || !current.Lease.Expires.After(now) {
		return fmt.Errorf("active lease held by %q required", actor)
	}
	return nil
}

// sameHolder requires the actor to hold the work's lease, expired or not.
// Work without a holder is open to any authorized actor.
func sameHolder(current WorkState, actor string) error {
	if current.Lease.Holder != "" && current.Lease.Holder != actor {
		return fmt.Errorf("work is held by %q, not %q", current.Lease.Holder, actor)
	}
	return nil
}

func requestExpiry(value string, now time.Time) (time.Time, error) {
	if value == "" {
		return now.Add(DefaultLeaseTTL), nil
	}
	expires, err := time.Parse(time.RFC3339, value)
	if err != nil || !expires.After(now) || expires.After(now.Add(DefaultLeaseTTL)) {
		return time.Time{}, fmt.Errorf("lease expiry must be after now and within %s", DefaultLeaseTTL)
	}
	return expires.UTC(), nil
}

// ReclaimExpired clears an expired active lease while preserving its branch for
// deliberate takeover. It never changes non-active lifecycle states.
func ReclaimExpired(current WorkState, now time.Time) (WorkState, bool) {
	if (current.Status != "Claimed" && current.Status != "In progress") || current.Lease.Expires.IsZero() || current.Lease.Expires.After(now.UTC()) {
		return current, false
	}
	return WorkState{Status: "Ready", Lease: Lease{Branch: current.Lease.Branch}}, true
}
