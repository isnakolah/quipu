// Package bootstrap installs source-controlled repository operating files.
package bootstrap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// Options shape the generated files. Templates use [[ ]] delimiters so the
// GitHub Actions ${{ }} expressions pass through untouched.
type Options struct {
	Owner         string
	ProjectNumber int
	DefaultBranch string
	// RunsOn maps writer, policy, and test to runner labels.
	RunsOn map[string][]string
	// QuipuVersion is the tag or commit the workflows install.
	QuipuVersion string
	// Schedule is the Writer's cron schedule; empty turns it off.
	Schedule            string
	WriterOnPullRequest bool
}

// installQuipu uses a quipu already on the runner's PATH (a self-hosted
// runner the owner keeps current) and otherwise installs the pinned version.
const installQuipu = `      - name: Find Quipu on the runner
        id: quipu
        run: |
          if command -v quipu >/dev/null 2>&1; then
            echo "Using $(command -v quipu): $(quipu version)"
            echo found=true >> "$GITHUB_OUTPUT"
          fi
      - uses: actions/setup-go@v6
        if: steps.quipu.outputs.found != 'true'
        with: {go-version: '1.26.4'}
      - name: Install Quipu [[.QuipuVersion]]
        if: steps.quipu.outputs.found != 'true'
        run: go install github.com/isnakolah/quipu/cmd/quipu@[[.QuipuVersion]]
`

var workflows = map[string]string{
	".github/workflows/quipu-writer.yml": `name: Quipu Writer
on:
  issue_comment:
    types: [created]
  issues:
    types: [opened, closed, reopened, edited, labeled, unlabeled]
[[- if .WriterOnPullRequest]]
  pull_request:
    types: [opened, synchronize, reopened, closed]
[[- end]]
[[- if .Schedule]]
  schedule:
    - cron: '[[.Schedule]]'
[[- end]]
  workflow_dispatch:
permissions:
  actions: write
  contents: read
  issues: write
  pull-requests: write
concurrency:
  group: quipu-writer-${{ github.repository_id }}
  cancel-in-progress: false
jobs:
  writer:
    runs-on: [[runner "writer"]]
    if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.fork == false
    steps:
      - uses: actions/checkout@v4
        with: {ref: [[.DefaultBranch]]}
` + installQuipu + `      - name: Writer and watchdog
        env:
          # The Writer token changes Project fields; the workflow token posts
          # receipts, so only github-actions[bot] can write a trusted receipt.
          GITHUB_TOKEN: ${{ secrets.QUIPU_WRITER_TOKEN }}
          QUIPU_RECEIPT_TOKEN: ${{ github.token }}
          EVENT: ${{ github.event_name }}
        run: |
          if [ -z "$GITHUB_TOKEN" ]; then
            echo '::error::QUIPU_WRITER_TOKEN is not configured, so no work can be claimed, reviewed, or finished. Owner: create a classic PAT with the project scope plus public_repo (public repository) or repo (private repository), then run gh secret set QUIPU_WRITER_TOKEN.'
            # Scheduled runs only annotate, so a missing token does not fail every five minutes.
            [ "$EVENT" = schedule ] && exit 0
            exit 1
          fi
          case "$EVENT" in
            issue_comment|issues)
              # Drain everything since the last successful run: Actions keeps
              # only one queued run per concurrency group, and runners sleep.
              quipu writer reconcile --since-last-run --apply --json ;;
            *)
              quipu writer reconcile --apply --json ;;
          esac
      - name: Validation summary
        if: always()
        env:
          GITHUB_TOKEN: ${{ secrets.QUIPU_WRITER_TOKEN }}
        run: |
          [ -n "$GITHUB_TOKEN" ] || exit 0
          { echo '## quipu validate'; echo '` + "```" + `'; quipu validate 2>&1 || true; echo '` + "```" + `'; } >> "$GITHUB_STEP_SUMMARY"
`,
	".github/workflows/quipu-policy.yml": `name: Quipu policy
on:
  pull_request_target:
    types: [opened, synchronize, reopened, edited, labeled, unlabeled, ready_for_review]
permissions:
  contents: read
  issues: read
  pull-requests: read
jobs:
  policy:
    runs-on: [[runner "policy"]]
    steps:
      # pull_request_target runs trusted default-branch code. Never check out
      # an untrusted head ref: the Writer credential is metadata-only here.
      - uses: actions/checkout@v4
        with:
          ref: [[.DefaultBranch]]
          persist-credentials: false
` + installQuipu + `      - name: Validate the PR against the Quipu flow
        env:
          GITHUB_TOKEN: ${{ secrets.QUIPU_WRITER_TOKEN }}
          PR_URL: ${{ github.event.pull_request.html_url }}
        run: |
          test -n "$GITHUB_TOKEN" || { echo '::error::QUIPU_WRITER_TOKEN not configured; policy cannot verify the Project lease.'; exit 1; }
          quipu policy pr --pr "$PR_URL"
`,
	".github/workflows/ci.yml": `name: CI
on:
  pull_request:
  push:
    branches: [ [[.DefaultBranch]] ]
permissions:
  contents: read
jobs:
  test:
    runs-on: [[runner "test"]]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v6
        with: {go-version: '1.26.4'}
      - name: Test Go source when present
        run: |
          if [ -f go.mod ]; then
            go test -race ./...
            go vet ./...
          else
            echo 'No Go module yet; bootstrap policy files only.'
          fi
`,
}

var forms = map[string]string{
	".github/ISSUE_TEMPLATE/decision.yml": `name: Decision
description: Decision requiring named approver.
title: "DECISION — "
labels: ["kind:decision"]
body:
  - type: textarea
    id: question
    attributes: {label: Decision question}
    validations: {required: true}
  - type: textarea
    id: options
    attributes: {label: Options}
    validations: {required: true}
  - type: textarea
    id: consequences
    attributes: {label: Consequences}
    validations: {required: true}
  - type: input
    id: approver
    attributes: {label: Required approver}
    validations: {required: true}
`,
	".github/ISSUE_TEMPLATE/epic.yml": `name: Epic
description: Owner-approved long-lived outcome. Never implementation leaf.
title: "EPIC — "
labels: ["kind:epic"]
body:
  - type: markdown
    attributes:
      value: "Owner approval required before Ready. Epic never carries implementation branch."
  - type: textarea
    id: outcome
    attributes: {label: Outcome}
    validations: {required: true}
  - type: textarea
    id: non_goals
    attributes: {label: Non-goals}
    validations: {required: true}
  - type: textarea
    id: constraints
    attributes: {label: Standing constraints}
    validations: {required: true}
  - type: textarea
    id: exit
    attributes: {label: Exit criteria}
    validations: {required: true}
  - type: textarea
    id: children
    attributes: {label: Expected children}
    validations: {required: true}
  - type: input
    id: approval
    attributes: {label: Owner approval, placeholder: "@owner approval comment URL"}
    validations: {required: true}
`,
	".github/ISSUE_TEMPLATE/work.yml": `name: Work
description: Story, Contract, Task, Gate, or Defect with Quipu work contract.
title: "US- — "
body:
  - type: markdown
    attributes:
      value: |
        <!-- work:v1 -->
        ## Outcome
        ## Scope
        Includes:
        -
        Excludes:
        -
        ## Acceptance criteria
        - [ ]
        ## Required evidence
        -
        - Verification boundary: local | CI | target host | provider | staging | production | release
        ## Documentation impact
        None: explain specific reason
        ## Source and relationships
        - Native parent and blocker links are authoritative.
  - type: textarea
    id: contract
    attributes: {label: Work contract, description: "Copy exact contract above. Add native parent after creation."}
    validations: {required: true}
`,
}

// Render returns every generated file, keyed by repository path.
func Render(o Options) (map[string]string, error) {
	if o.DefaultBranch == "" {
		return nil, fmt.Errorf("default branch is required")
	}
	if o.QuipuVersion == "" {
		return nil, fmt.Errorf("a pinned Quipu version is required")
	}
	funcs := template.FuncMap{"runner": func(job string) string {
		labels := o.RunsOn[job]
		switch len(labels) {
		case 0:
			return "ubuntu-latest"
		case 1:
			return labels[0]
		}
		return "[" + strings.Join(labels, ", ") + "]"
	}}
	all := make(map[string]string, len(workflows)+len(forms)+1)
	for path, body := range workflows {
		tmpl, err := template.New(path).Delims("[[", "]]").Funcs(funcs).Parse(body)
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, o); err != nil {
			return nil, err
		}
		all[path] = out.String()
	}
	for path, body := range forms {
		all[path] = body
	}
	all[".github/ISSUE_TEMPLATE/config.yml"] = fmt.Sprintf(`blank_issues_enabled: false
contact_links:
  - name: Delivery Project
    url: https://github.com/users/%s/projects/%d
    about: Select existing Ready work. Do not use free-form task intake.
`, o.Owner, o.ProjectNumber)
	return all, nil
}

// Result says what Install did to each file.
type Result struct {
	Written   []string `json:"written"`
	Unchanged []string `json:"unchanged"`
	// Kept are existing files that differ from Quipu's and were left alone.
	Kept []string `json:"kept"`
}

// Install writes the generated files under root. Identical files are left
// as they are, and a different existing file is kept unless replace is set.
func Install(root string, o Options, replace bool) (Result, error) {
	var result Result
	all, err := Render(o)
	if err != nil {
		return result, err
	}
	paths := make([]string, 0, len(all))
	for path := range all {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		target := filepath.Join(root, path)
		current, err := os.ReadFile(target)
		switch {
		case err == nil && string(current) == all[path]:
			result.Unchanged = append(result.Unchanged, path)
			continue
		case err == nil && !replace:
			result.Kept = append(result.Kept, path)
			continue
		case err != nil && !os.IsNotExist(err):
			return result, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return result, err
		}
		if err := os.WriteFile(target, []byte(all[path]), 0644); err != nil {
			return result, err
		}
		result.Written = append(result.Written, path)
	}
	return result, nil
}
