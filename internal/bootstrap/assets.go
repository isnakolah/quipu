// Package bootstrap installs source-controlled repository operating files.
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var files = map[string]string{
	".github/workflows/quipu-writer.yml": `name: Quipu Writer
on:
  issue_comment:
    types: [created]
  pull_request:
    types: [opened, synchronize, reopened, closed]
  schedule:
    - cron: '*/5 * * * *'
  workflow_dispatch:
permissions:
  contents: read
  issues: write
  pull-requests: write
concurrency:
  group: quipu-writer-${{ github.repository_id }}
  cancel-in-progress: false
jobs:
  writer:
    runs-on: ubuntu-latest
    if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.fork == false
    steps:
      - uses: actions/checkout@v4
        with: {ref: __QUIPU_DEFAULT_BRANCH__}
      - uses: actions/setup-go@v6
        with: {go-version: '1.26.4'}
      - name: Install canonical Quipu
        run: go install github.com/isnakolah/quipu/cmd/quipu@main
      - name: Writer activation boundary
        env:
          GITHUB_TOKEN: ${{ secrets.QUIPU_WRITER_TOKEN }}
        run: |
          test -n "$GITHUB_TOKEN" || { echo 'QUIPU_WRITER_TOKEN not configured; writer remains inactive.'; exit 0; }
          if [ "${{ github.event_name }}" = "issue_comment" ]; then
            quipu writer run --issue-number "${{ github.event.issue.number }}" --apply
          else
            quipu writer reconcile --apply
          fi
`,
	".github/workflows/quipu-policy.yml": `name: Quipu policy
on:
  pull_request_target:
    types: [opened, synchronize, reopened, edited]
permissions:
  contents: read
  issues: read
  pull-requests: read
jobs:
  policy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: __QUIPU_DEFAULT_BRANCH__
          persist-credentials: false
      - uses: actions/setup-go@v6
        with: {go-version: '1.26.4'}
      - name: Install canonical Quipu
        run: go install github.com/isnakolah/quipu/cmd/quipu@main
      - name: Validate PR metadata and live work state
        env:
          GITHUB_TOKEN: ${{ secrets.QUIPU_WRITER_TOKEN }}
          PR_URL: ${{ github.event.pull_request.html_url }}
        run: |
          test -n "$GITHUB_TOKEN" || { echo 'QUIPU_WRITER_TOKEN not configured; policy cannot verify Project lease.'; exit 1; }
          quipu policy pr --pr "$PR_URL" --json
`,
	".github/workflows/ci.yml": `name: CI
on:
  pull_request:
  push:
    branches: [__QUIPU_DEFAULT_BRANCH__]
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
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
	".agent/rules/quipu.md": `# Quipu

Agents run this project. Loop: ` + "`quipu status --json`" + `, then ` + "`quipu start --next --json`" + ` (stop if ` + "`Idle`" + `), implement on the created branch, open a PR whose body says ` + "`Refs #N`" + `, run ` + "`quipu done N --json`" + `; after merge run ` + "`quipu done N --json`" + ` with ` + "`--criteria --commands --documentation --risks --boundary`" + `. Always follow the result's ` + "`next`" + `. One worktree per agent; rerun ` + "`quipu start N`" + ` to renew long leases. Native parent and blocker links are authority. Never create local task ledger, autonomous Epic, or approve your own work.
`,
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

func Install(root, owner string, projectNumber int, defaultBranch string) error {
	if defaultBranch == "" {
		return fmt.Errorf("default branch is required")
	}
	all := make(map[string]string, len(files)+1)
	for path, body := range files {
		all[path] = strings.ReplaceAll(body, "__QUIPU_DEFAULT_BRANCH__", defaultBranch)
	}
	all[".github/ISSUE_TEMPLATE/config.yml"] = fmt.Sprintf(`blank_issues_enabled: false
contact_links:
  - name: Delivery Project
    url: https://github.com/users/%s/projects/%d
    about: Select existing Ready work. Do not use free-form task intake.
`, owner, projectNumber)
	for path, body := range all {
		target := filepath.Join(root, path)
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("refuse to overwrite bootstrap file %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(body), 0644); err != nil {
			return err
		}
	}
	return nil
}
