package writer

// Summaries are what a person reads on GitHub and in its notification emails.
// The record a request or receipt carries sits hidden in its marker; these
// lines say the same thing in words. Nothing parses them.

import (
	"fmt"
	"strings"
	"time"
)

// requestSummary says what a request asks the Writer to do.
func requestSummary(r Request) string {
	who := code(r.Actor)
	var line string
	switch r.Action {
	case "claim":
		line = fmt.Sprintf("**Claim**: %s asks to take this Issue on %s.", who, code(r.Branch))
	case "start":
		line = fmt.Sprintf("**Start**: %s asks to begin work.", who)
	case "renew":
		line = fmt.Sprintf("**Renew**: %s asks to extend its lease.", who)
	case "resume":
		line = fmt.Sprintf("**Resume**: %s asks to pick this work up again.", who)
	case "takeover":
		line = fmt.Sprintf("**Take over**: %s asks to take over this work on %s.", who, code(r.Branch))
	case "release":
		line = fmt.Sprintf("**Release**: %s gives this work back.", who)
	case "status":
		line = fmt.Sprintf("**Move**: %s asks to move this Issue to %s.", who, plain(r.Status))
	case "pr.link":
		line = fmt.Sprintf("**Review**: %s links %s and asks for review.", who, plain(r.PR))
	case "evidence.submit":
		line = fmt.Sprintf("**Evidence**: %s submits evidence for %s.", who, plain(r.PR))
	case "merge.queue", "merge.batch":
		line = fmt.Sprintf("**Merge**: %s asks Quipu to merge %s at %s once its checks pass.", who, plain(r.PR), code(short(r.Head)))
	case "queue.return":
		line = fmt.Sprintf("**Back from the merge queue**: %s.", plain(r.Reason))
	case "park":
		line = fmt.Sprintf("**Park**: %s waits for Decision #%d.", who, r.On)
	case "block":
		line = fmt.Sprintf("**Block**: %s marks this work blocked.", who)
	case "import":
		line = fmt.Sprintf("**Import**: %s brings this Issue in from before Quipu.", who)
	case "assign":
		line = fmt.Sprintf("**Assign**: %s asks to assign %s.", who, code(r.Assignee))
	case "edit":
		line = fmt.Sprintf("**Edit**: %s asks to replace this Issue's contract.", who)
	case "tick":
		line = fmt.Sprintf("**Tick**: %s marks acceptance criterion %d met.", who, r.Criterion)
	case "untick":
		line = fmt.Sprintf("**Untick**: %s marks acceptance criterion %d not met.", who, r.Criterion)
	case "pr.close":
		line = fmt.Sprintf("**Close PR**: %s asks to close %s.", who, plain(r.PR))
	default:
		line = fmt.Sprintf("**%s**: request from %s.", plain(r.Action), who)
	}
	if note := plain(r.Note); note != "" {
		line += "\n\n" + note
	}
	if r.Evidence != nil {
		line += "\n\n" + evidenceSummary(*r.Evidence)
	}
	return line + "\n"
}

// receiptSummary says what the Writer did with a request.
func receiptSummary(r Receipt) string {
	lines := []string{receiptHeadline(r)}
	if s := r.State; s != nil && s.Holder != "" {
		held := "Held by " + code(s.Holder)
		if s.Branch != "" {
			held += " on " + code(s.Branch)
		}
		if t, err := time.Parse(time.RFC3339, s.Expires); err == nil {
			held += ", lease until " + t.UTC().Format("2006-01-02 15:04 UTC")
		}
		lines = append(lines, held+".")
	}
	if m := r.Merge; m != nil {
		lines = append(lines, fmt.Sprintf("Quipu merges %s once its checks pass.", plain(m.PR)))
	}
	if r.Evidence != nil {
		lines = append(lines, evidenceSummary(*r.Evidence))
	}
	return strings.Join(lines, "\n\n") + "\n"
}

// receiptHeadline is the first line of a receipt: what an email preview shows.
func receiptHeadline(r Receipt) string {
	detail := plain(r.Detail)
	if r.Result != "accepted" {
		return "**Rejected**: " + detail
	}
	head := "Accepted"
	if r.State != nil && r.State.Status != "" {
		head = plain(r.State.Status)
	}
	if detail == "" {
		return "**" + head + "**"
	}
	return "**" + head + "**: " + detail
}

// evidenceSummary lists each evidence field by name.
func evidenceSummary(e Evidence) string {
	fields := []struct{ name, value string }{
		{"Criteria", e.Criteria},
		{"Commands", e.Commands},
		{"Documentation", e.Documentation},
		{"Risks", e.Risks},
		{"Boundary", e.Boundary},
		{"Environments", e.Environments},
		{"Final SHA", e.FinalSHA},
		{"CI", e.CIURL},
		{"Artifacts", e.Artifacts},
	}
	var b strings.Builder
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		fmt.Fprintf(&b, "- **%s:** %s\n", f.name, indent(plain(f.value)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// plain makes text someone else wrote safe to show: no HTML, so no fake Quipu
// marker or comment, and no @mention that would notify a person.
func plain(s string) string {
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, "@", "@⁠")
	return strings.TrimSpace(s)
}

// code shows a name in a code span, which cannot hold a backtick.
func code(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "`", "'")
	if s == "" {
		return "(none)"
	}
	return "`" + s + "`"
}

// indent keeps every line of a multi-line value inside its list item.
func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n  ") }

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
