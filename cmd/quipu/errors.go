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
	{"branch-elsewhere", "the work branch is checked out in another worktree", "continue in that worktree, or remove it with `git worktree remove <path>` if nothing runs there"},
	{"no-receipt", "the Writer did not answer before the wait ran out", "rerun the same command, it is safe; if it keeps happening, tell the owner the Quipu Writer workflow is not running"},
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
	{"evidence-missing", "recording evidence after merge needs every evidence field", "rerun `quipu done N --json --criteria ... --commands ... --documentation ... --risks ... --boundary ...` (add `--ci-url URL` if checks have not passed)"},
	{"not-active", "the Issue is not in progress, in review, or awaiting evidence", "check `quipu status --json`; start work with `quipu start N --json`"},
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
	for _, kind := range flowErrors {
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
