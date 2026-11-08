package main

// Errors an agent can act on. Every failure of the everyday commands carries
// a stable code and the exact next step; with --json the CLI prints them as
// {"error": {"code", "message", "next"}}. The agent guide renders flowErrors
// as its troubleshooting table, so the docs always match the CLI.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type flowErrorKind struct {
	Code, When, Next string
}

var flowErrors = []flowErrorKind{
	{"held-by", "the Issue's lease belongs to another worktree or agent", "continue in the worktree that holds it (`quipu status --json` shows holder and branch), or take other work with `quipu start --next --json`"},
	{"not-ready", "the Issue is not Ready, so it cannot be started", "take Ready work with `quipu start --next --json`; only the owner makes work Ready"},
	{"branch-elsewhere", "the work branch is checked out in another worktree", "continue in that worktree, or remove it with `quipu worktree remove <path> --json` if nothing runs there"},
	{"git-busy", "the worktree is in the middle of a rebase, merge, or cherry-pick, so it cannot switch branch", "do what `next` says: finish it with `git <op> --continue` or back out with `git <op> --abort` (both pass the guard), then rerun the same command"},
	{"worktree-held", "that worktree still holds a live lease", "finish it with `quipu done N --json` first, or take other work"},
	{"worktree-dirty", "that worktree has changes or commits found nowhere else, and its Issue is not Done", "commit and push them, or `git restore` them from inside it, then rerun the same command"},
	{"worktree-missing", "no linked worktree of this clone matches", "check `git worktree list`; the main checkout is never removed"},
	{"no-receipt", "the Writer did not answer before the wait ran out", "the request stays queued and still applies when the Writer next runs, so rerun the same command later instead of taking other work; it is safe. If it keeps happening, tell the owner what the message says about the Writer and its runner"},
	{"rejected-stale", "the work changed while your request was in flight", "rerun the same command"},
	{"rejected-taken", "another agent claimed the work first", "run `quipu start --next --json` for other work"},
	{"rejected-blocked", "the work has an open blocker", "take other work with `quipu start --next --json`"},
	{"rejected-journal", "the wiki journal is not synchronized yet", "wait a minute, then rerun the same command"},
	{"rejected-unauthorized", "your GitHub login is not an authorized actor", "tell the owner to add your login to `authorized_actors`"},
	{"rejected-holder", "the request came from an identity that does not hold the lease", "run the command in the worktree that holds the work"},
	{"rejected", "the Writer refused the request", "read the message, rerun once, then tell the owner with the message"},
	{"pr-missing", "no PR exists from the work branch", "push the branch and run `gh pr create --base <default> --head <branch> --title ... --body \"Refs #N\"`, then rerun `quipu done N --json`"},
	{"pr-closed", "the PR was closed without merging", "open a new PR from the same branch with `Refs #N`, then rerun `quipu done N --json`"},
	{"pr-merged-early", "the PR merged before `quipu done` moved the work to review", "tell the owner; they recover it with `quipu pr link` and `quipu evidence submit`"},
	{"evidence-missing", "recording evidence after merge needs every evidence field", "rerun `quipu done N --json --evidence-file FILE` with every section filled in (`quipu done --evidence-template` prints them; add `--ci-url URL` if checks have not passed)"},
	{"checks-pending", "with `merge: agent`, a required check on the PR has not finished", "take other work, look once with `quipu status --json`, and when they have finished rerun the same `quipu done N --json ...`"},
	{"checks-no-runner", "with `merge: agent`, a required check is queued and no online runner has its labels, so it cannot start", "do not poll; take other work with `quipu start --next --json` and tell the owner what the message names. The owner starts that runner, registers one with those labels, changes the job's `runs-on`, or takes the check out of `required_checks`. Then rerun the same `quipu done N --json ...`"},
	{"checks-failing", "with `merge: agent`, a required check on the PR failed or never ran", "fix the cause, push to the work branch, then rerun the same `quipu done N --json ...`"},
	{"merge-blocked", "GitHub cannot merge the PR: it is a draft, behind the default branch, conflicting, or refused by a rule", "update the branch (`git fetch origin && git merge origin/<default>`), resolve conflicts, push, then rerun the same `quipu done N --json ...`"},
	{"commit-format", "a commit message breaks the repository's `commit_format`", "write it as `#NNN type(scope): summary` with a `Refs: #N` footer line (`git commit --amend` for the last commit), and title the PR `type(scope): summary`"},
	{"not-active", "the Issue is not in progress, in review, or awaiting evidence", "check `quipu status --json`; start work with `quipu start N --json`"},
	{"no-record", "no lease record matches, here or for that Issue in any worktree of this clone", "nothing to release; carry on, or take work with `quipu start --next --json`"},
	{"lease-unverified", "GitHub could not be read and the record has not expired, so the lease may still be live", "rerun the same command in a minute; a record clears itself once its lease expires"},
	{"not-leaf", "the Issue has sub-issues, so no PR may implement it", "start one of its leaf Issues instead; `next` names the Ready ones"},
	{"check-failed", "`quipu check` ran the repository's check and it failed or could not run", "fix the cause and rerun `quipu check --json`; a failure is never reused, so `quipu done` runs the check again"},
	{"no-check", "the default branch sets no `local_check` or `merge_queue.check`, so `quipu check` has nothing to run", "run the tests you need yourself; CI checks the PR"},
	{"not-implementable", "the Issue is closed, of a kind PRs do not implement, without a parent, without a valid work contract, or blocked", "take other work with `quipu start --next --json`; the message says what is wrong, and the owner fixes the Issue"},
	{"batch-off", "`quipu done` named several Issues, but this repository does not set `batches: epic`", "finish one Issue at a time: `quipu done N --json`"},
}

// flagErrors are the failures only a flag can cause. The handbook lists them
// only when that flag is on.
var flagErrors = []flowErrorKind{
	{"batch-mixed", "with `batches: epic`, the Issues named are not all on one rolling Epic branch", "finish each branch's Issues with their own `quipu done A B ... --json`"},
	{"batch-empty", "with `batches: epic`, no commit in the PR references any of the Issues named", "commit with `Refs #N` in each message for the leaf it implements, push, then rerun"},
	{"local-check-failed", "with `merge_without_ci`, `local_check` did not pass, so nothing merged", "fix the cause, commit, push, then rerun the same `quipu done N --json ...`"},
	{"worktree-unclean", "with `merge_without_ci`, the worktree has changes or is not at the PR's head, so a check here would not be checking what merges", "commit and push, or `git restore` the changes, then rerun the same `quipu done N --json ...`"},
	{"policy-rules", "with `merge_without_ci` or `merge: queue`, the PR breaks a policy rule, and those are never waived", "fix each problem it lists, then rerun the same `quipu done N --json ...`; what only the owner can do, such as the `quipu:owner-approved` label, ask the owner for"},
	{"base-moved", "with `update_behind: auto`, the default branch moved during each run of `local_check`, so nothing merged", "take other work, then rerun the same `quipu done N --json ...` when the default branch is quieter"},
	{"machine-busy", "with `limits`, every build slot on this machine is in use, so `local_check` did not run and nothing merged", "take other work, or stop if you have none, then rerun the same `quipu done N --json ...` later"},
	{"merge-conflict", "with `update_behind: auto`, the work branch conflicts with the default branch, and the merge was undone", "run `git fetch origin && git merge origin/<default>`, resolve the conflicts, commit, push, then rerun the same `quipu done N --json ...`"},
}

// flowError is one coded failure.
type flowError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next"`
	cause   error
}

func (e *flowError) Error() string { return e.Message + ". Next: " + e.Next }
func (e *flowError) Unwrap() error { return e.cause }

// flowErr builds a coded error whose next step comes from flowErrors.
func flowErr(code, format string, args ...any) *flowError {
	e := &flowError{Code: code, Message: fmt.Sprintf(format, args...)}
	for _, kind := range append(append([]flowErrorKind{}, flowErrors...), flagErrors...) {
		if kind.Code == code {
			e.Next = kind.Next
		}
	}
	return e
}

// rejectionCode maps a Writer rejection detail to its flowErrors code.
func rejectionCode(detail string) string {
	switch lower := strings.ToLower(detail); {
	case strings.Contains(lower, "fingerprint"):
		return "rejected-stale"
	case strings.Contains(lower, "already leased"):
		return "rejected-taken"
	case strings.Contains(lower, "blocker"):
		return "rejected-blocked"
	case strings.Contains(lower, "journal"):
		return "rejected-journal"
	case strings.Contains(lower, "not authorized"):
		return "rejected-unauthorized"
	case strings.Contains(lower, "held by") || strings.Contains(lower, "active lease held"):
		return "rejected-holder"
	default:
		return "rejected"
	}
}

// printError reports a failed command. JSON callers get a parseable error
// object on stdout; everyone else gets one line on stderr.
func printError(err error, args []string, stdout, stderr io.Writer) {
	if errors.Is(err, errReported) {
		return
	}
	if d := (denial{}); errors.As(err, &d) {
		// Agents read this line as the hook's reason; keep it exactly as documented.
		fmt.Fprintln(stderr, d.Error())
		if wantsJSON(args) {
			message := d.Why
			if d.Detail != "" {
				message += " (" + d.Detail + ")"
			}
			raw, _ := json.MarshalIndent(map[string]*flowError{"error": {Code: d.Code, Message: message, Next: d.Instead}}, "", "  ")
			fmt.Fprintln(stdout, string(raw))
		}
		return
	}
	if !wantsJSON(args) {
		fmt.Fprintln(stderr, "quipu:", err)
		return
	}
	var coded *flowError
	if !errors.As(err, &coded) {
		coded = &flowError{Code: "error", Message: err.Error(), Next: "read the message; `quipu guide` explains every command"}
	}
	raw, _ := json.MarshalIndent(map[string]*flowError{"error": coded}, "", "  ")
	fmt.Fprintln(stdout, string(raw))
}

func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "-json" || arg == "--json=true" {
			return true
		}
	}
	return false
}
