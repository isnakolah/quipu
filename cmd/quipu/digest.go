package main

// `quipu digest` puts the owner's day in one place, so nothing has to interrupt
// them: what merged (Issue, PR, and the boundary its evidence reached), what
// waits on them (Decisions, Gates, parked work, a held queue, enforcement
// patches), and which PRs changed .github/workflows/. It reads merged PRs and
// the Writer's receipts and changes nothing. `quipu status` prints its headline,
// and with `digest: issue` the Writer keeps it in one pinned Issue.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
)

// digestWindow is what a digest covers unless told otherwise.
const digestWindow = 24 * time.Hour

// digestPull is a PR the window touched.
type digestPull struct {
	Number   int
	URL      string
	Title    string
	Body     string
	State    string // OPEN, MERGED or CLOSED
	MergedAt time.Time
	Files    []string
}

// digestComment is an Issue comment, as the repository-wide comment list gives it.
type digestComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	IssueURL  string `json:"issue_url"`
	CreatedAt string `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// digestInputs is everything a digest is built from.
type digestInputs struct {
	Pulls []digestPull
	// Boundaries maps an Issue to the boundary its latest evidence receipt
	// reached, or "" for none yet. An Issue missing from it was not looked up.
	Boundaries map[int]string
	OwnerQueue []ownerItem
	Comments   []digestComment
}

type digestMerge struct {
	Issue    int    `json:"issue"`
	Title    string `json:"title"`
	PR       string `json:"pr"`
	Boundary string `json:"boundary,omitempty"`
	MergedAt string `json:"merged_at"`
}

type digestPatch struct {
	Issue      int      `json:"issue"`
	Comment    int64    `json:"comment"`
	URL        string   `json:"url"`
	Files      []string `json:"files"`
	ProposedBy string   `json:"proposed_by"`
	Apply      string   `json:"apply"`
}

type digestWorkflow struct {
	Number int      `json:"number"`
	PR     string   `json:"pr"`
	Title  string   `json:"title"`
	Issues []int    `json:"issues,omitempty"`
	State  string   `json:"state"`
	Files  []string `json:"files"`
}

type digestReport struct {
	Repository      string           `json:"repository"`
	Since           string           `json:"since"`
	Until           string           `json:"until"`
	Headline        []string         `json:"headline"`
	Merged          []digestMerge    `json:"merged"`
	OwnerQueue      []ownerItem      `json:"owner_queue"`
	Patches         []digestPatch    `json:"patches"`
	WorkflowChanges []digestWorkflow `json:"workflow_changes"`
	Warnings        []string         `json:"warnings,omitempty"`
}

// buildDigest sorts the inputs into what merged, what waits on the owner, and
// which workflow files changed, between since and now.
func buildDigest(repository string, in digestInputs, since, now time.Time) digestReport {
	d := digestReport{Repository: repository, Since: since.UTC().Format(time.RFC3339), Until: now.UTC().Format(time.RFC3339),
		Merged: []digestMerge{}, OwnerQueue: append([]ownerItem{}, in.OwnerQueue...), Patches: []digestPatch{}, WorkflowChanges: []digestWorkflow{}}
	for _, pull := range in.Pulls {
		merged := pull.State == "MERGED" && !pull.MergedAt.Before(since)
		issues, _ := policycheck.ReferencedIssueSet(pull.Body)
		if merged {
			for _, issue := range issuesOrNone(issues) {
				entry := digestMerge{Issue: issue, Title: pull.Title, PR: pull.URL, MergedAt: pull.MergedAt.UTC().Format(time.RFC3339)}
				if boundary, looked := in.Boundaries[issue]; looked {
					entry.Boundary = boundary
					if boundary == "" {
						entry.Boundary = "evidence pending"
					}
				}
				d.Merged = append(d.Merged, entry)
			}
		}
		if files := changedWorkflows(pull.Files); len(files) > 0 && (merged || pull.State == "OPEN") {
			d.WorkflowChanges = append(d.WorkflowChanges, digestWorkflow{Number: pull.Number, PR: pull.URL, Title: pull.Title, Issues: issues, State: strings.ToLower(pull.State), Files: files})
		}
	}
	for _, comment := range in.Comments {
		if patch, ok := digestPatchOf(comment, since); ok {
			d.Patches = append(d.Patches, patch)
		}
	}
	sort.SliceStable(d.Merged, func(i, j int) bool { return d.Merged[i].MergedAt < d.Merged[j].MergedAt })
	sort.SliceStable(d.WorkflowChanges, func(i, j int) bool { return d.WorkflowChanges[i].Number < d.WorkflowChanges[j].Number })
	sort.SliceStable(d.Patches, func(i, j int) bool { return d.Patches[i].Comment < d.Patches[j].Comment })
	d.Headline = digestHeadline(d, now.Sub(since))
	return d
}

func issuesOrNone(issues []int) []int {
	if len(issues) == 0 {
		return []int{0}
	}
	return issues
}

// digestPatchOf reads an enforcement patch out of a proposal comment.
func digestPatchOf(comment digestComment, since time.Time) (digestPatch, bool) {
	if !strings.Contains(comment.Body, "<!-- quipu-proposal:v1 -->") || !strings.Contains(comment.Body, patchMarker) {
		return digestPatch{}, false
	}
	if created, err := time.Parse(time.RFC3339, comment.CreatedAt); err == nil && created.Before(since) {
		return digestPatch{}, false
	}
	patch, err := extractPatch(comment.Body)
	if err != nil {
		return digestPatch{}, false
	}
	files, err := patchFiles(patch)
	if err != nil {
		return digestPatch{}, false
	}
	issue, _ := strconv.Atoi(comment.IssueURL[strings.LastIndex(comment.IssueURL, "/")+1:])
	return digestPatch{Issue: issue, Comment: comment.ID, URL: comment.HTMLURL, Files: files, ProposedBy: comment.User.Login,
		Apply: fmt.Sprintf("quipu patch apply %d", comment.ID)}, true
}

// digestHeadline is the three lines `quipu status` prints and the pinned Issue
// opens with.
func digestHeadline(d digestReport, window time.Duration) []string {
	span := "the last " + shortDuration(window)
	merged := "none"
	if len(d.Merged) > 0 {
		merged = fmt.Sprintf("%d: %s", len(d.Merged), mergedList(d.Merged))
	}
	waits := "nothing"
	if total := len(d.OwnerQueue) + len(d.Patches); total > 0 {
		waits = fmt.Sprintf("%d (%d on the board, %d enforcement patch(es))", total, len(d.OwnerQueue), len(d.Patches))
	}
	workflows := "none"
	if len(d.WorkflowChanges) > 0 {
		var names []string
		for _, change := range d.WorkflowChanges {
			names = append(names, fmt.Sprintf("PR #%d %s (%s)", change.Number, change.State, strings.Join(change.Files, ", ")))
		}
		workflows = fmt.Sprintf("%d: %s", len(d.WorkflowChanges), strings.Join(names, "; "))
	}
	return []string{"Merged in " + span + ": " + merged, "Waits on you: " + waits, "Workflow changes in " + span + ": " + workflows}
}

func mergedList(merged []digestMerge) string {
	var names []string
	for _, entry := range merged {
		if entry.Issue == 0 {
			names = append(names, entry.PR)
			continue
		}
		names = append(names, fmt.Sprintf("#%d", entry.Issue))
	}
	return strings.Join(names, ", ")
}

func shortDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

// renderDigest is the digest as Markdown: the CLI prints it, and the pinned
// Issue holds it.
func renderDigest(d digestReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Quipu digest\n\n%s, %s to %s.\n\n", d.Repository, d.Since, d.Until)
	for _, line := range d.Headline {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	if len(d.Merged) > 0 {
		b.WriteString("\n## Merged\n\n| Issue | PR | Boundary |\n|---|---|---|\n")
		for _, entry := range d.Merged {
			issue := "none"
			if entry.Issue > 0 {
				issue = fmt.Sprintf("#%d", entry.Issue)
			}
			fmt.Fprintf(&b, "| %s %s | %s | %s |\n", issue, cell(entry.Title), entry.PR, orDash(entry.Boundary))
		}
	}
	if len(d.OwnerQueue)+len(d.Patches) > 0 {
		b.WriteString("\n## Waits on you\n\n")
		for _, item := range d.OwnerQueue {
			if item.Number == 0 {
				fmt.Fprintf(&b, "- %s\n", item.Reason)
				continue
			}
			fmt.Fprintf(&b, "- #%d %s: %s\n", item.Number, item.Title, item.Reason)
		}
		for _, patch := range d.Patches {
			fmt.Fprintf(&b, "- Enforcement patch on #%d by %s (%s): `%s` ([proposal](%s))\n", patch.Issue, patch.ProposedBy, strings.Join(patch.Files, ", "), patch.Apply, patch.URL)
		}
	}
	if len(d.WorkflowChanges) > 0 {
		b.WriteString("\n## Workflow changes\n\n")
		for _, change := range d.WorkflowChanges {
			fmt.Fprintf(&b, "- PR #%d (%s) %s: %s\n", change.Number, change.State, change.Title, strings.Join(change.Files, ", "))
		}
	}
	for _, warning := range d.Warnings {
		fmt.Fprintf(&b, "\n> %s\n", warning)
	}
	return b.String()
}

func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

// evidenceBoundary is the boundary of the latest accepted receipt that carries
// evidence, or "" when there is none.
func evidenceBoundary(receipts []writer.Receipt) string {
	for i := len(receipts) - 1; i >= 0; i-- {
		if receipts[i].Result == "accepted" && receipts[i].Evidence != nil {
			return receipts[i].Evidence.Boundary
		}
	}
	return ""
}

// digestSources load what a digest needs; tests replace them.
type digestSources struct {
	pulls    func(since time.Time) ([]digestPull, error)
	comments func(since time.Time) ([]digestComment, error)
	// receipts is nil for a light digest, which skips the boundary of each merge.
	receipts func(issue int) ([]writer.Receipt, error)
}

// collectDigest loads the inputs and builds the digest. A source that fails
// leaves a warning, not an error, so one slow API never hides the rest.
func collectDigest(repository string, src digestSources, owner []ownerItem, since, now time.Time) digestReport {
	in := digestInputs{OwnerQueue: owner, Boundaries: map[int]string{}}
	var warnings []string
	pulls, err := src.pulls(since)
	if err != nil {
		warnings = append(warnings, "merged PRs: "+err.Error())
	}
	in.Pulls = pulls
	if in.Comments, err = src.comments(since); err != nil {
		warnings = append(warnings, "enforcement patches: "+err.Error())
	}
	if src.receipts != nil {
		for _, pull := range pulls {
			if pull.State != "MERGED" || pull.MergedAt.Before(since) {
				continue
			}
			issues, _ := policycheck.ReferencedIssueSet(pull.Body)
			for _, issue := range issues {
				if _, done := in.Boundaries[issue]; done {
					continue
				}
				receipts, err := src.receipts(issue)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("receipts of #%d: %v", issue, err))
					continue
				}
				in.Boundaries[issue] = evidenceBoundary(receipts)
			}
		}
	}
	d := buildDigest(repository, in, since, now)
	d.Warnings = warnings
	return d
}

func liveDigestSources(c model.Config, full bool) digestSources {
	repo := c.Owner + "/" + c.Repository
	src := digestSources{
		pulls: func(since time.Time) ([]digestPull, error) {
			output, err := ghOutput("pr", "list", "--repo", repo, "--state", "all", "--limit", "100",
				"--search", "updated:>="+since.UTC().Format(time.RFC3339), "--json", "number,url,title,body,state,mergedAt,files")
			if err != nil {
				return nil, err
			}
			return parseDigestPulls(output)
		},
		comments: func(since time.Time) ([]digestComment, error) {
			query := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}}
			output, err := ghOutput("api", "--paginate", "--slurp", "repos/"+repo+"/issues/comments?"+query.Encode())
			if err != nil {
				return nil, err
			}
			var pages [][]digestComment
			if err := json.Unmarshal(output, &pages); err != nil {
				return nil, err
			}
			var all []digestComment
			for _, page := range pages {
				all = append(all, page...)
			}
			return all, nil
		},
	}
	if full {
		trust := receiptTrust(c)
		src.receipts = func(issue int) ([]writer.Receipt, error) {
			comments, err := github.NewClient().ListComments(context.Background(), c.Owner, c.Repository, issue)
			if err != nil {
				return nil, err
			}
			return trust.Receipts(comments), nil
		}
	}
	return src
}

func parseDigestPulls(output []byte) ([]digestPull, error) {
	var raw []struct {
		Number   int    `json:"number"`
		URL      string `json:"url"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		State    string `json:"state"`
		MergedAt string `json:"mergedAt"`
		Files    []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, err
	}
	pulls := make([]digestPull, 0, len(raw))
	for _, r := range raw {
		pull := digestPull{Number: r.Number, URL: r.URL, Title: r.Title, Body: r.Body, State: r.State}
		pull.MergedAt, _ = time.Parse(time.RFC3339, r.MergedAt)
		for _, file := range r.Files {
			pull.Files = append(pull.Files, file.Path)
		}
		pulls = append(pulls, pull)
	}
	return pulls, nil
}

// boardOwnerQueue is what waits on the owner on the board, without the per-PR
// calls `quipu status` makes.
func boardOwnerQueue(c model.Config, now time.Time) ([]ownerItem, error) {
	items, err := loadStatusItems(c)
	if err != nil {
		return nil, err
	}
	return ownerQueue(items, c.AgentsMerge() || c.MergesInQueue(), now), nil
}

func digestCommand(args []string) error {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	since := fs.Duration("since", digestWindow, "how far back the digest looks, such as 24h or 90m")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *since <= 0 {
		return fmt.Errorf("--since must be positive, not %s", *since)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	now := time.Now()
	owner, err := boardOwnerQueue(c, now)
	if err != nil {
		return err
	}
	if here, err := currentWorktree(); err == nil {
		if enforced, source, err := loadEnforcedConfig(here, false); err == nil && source != configSourceWorktree {
			if queue := loadQueueReport(enforced, here, func() ([]queueMember, []queueReturned, error) { return nil, nil, nil }); queue != nil && queue.Held != "" {
				owner = append(owner, ownerItem{Reason: "the merge queue is held: " + firstLine(queue.Held) + " Run quipu queue resume once it is safe"})
			}
		}
	}
	d := collectDigest(c.Owner+"/"+c.Repository, liveDigestSources(c, true), owner, now.Add(-*since), now)
	if *asJSON {
		return printValue(d, true)
	}
	fmt.Print(renderDigest(d))
	return nil
}

// statusDigest is the headline `quipu status` prints. It skips the receipts of
// each merge, which the headline does not need, and reuses the owner queue the
// status already built.
func statusDigest(c model.Config, owner []ownerItem, now time.Time) []string {
	d := collectDigest(c.Owner+"/"+c.Repository, liveDigestSources(c, false), owner, now.Add(-digestWindow), now)
	for _, warning := range d.Warnings {
		d.Headline = append(d.Headline, "digest incomplete: "+warning)
	}
	return d.Headline
}
