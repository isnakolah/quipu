package model

import (
	"strings"
	"testing"
)

func TestScopedCommand(t *testing.T) {
	c := Config{ScopedChecks: []ScopedCheck{
		{Command: "cargo test", Paths: []string{"src/**", "Cargo.*"}},
		{Command: "npm test", Paths: []string{"web/**"}},
		{Paths: []string{"docs/**", "**/*.md"}},
	}}
	for _, test := range []struct {
		name, want, reason string
		paths              []string
	}{
		{"docs only skip everything", "", "nothing ran", []string{"docs/a.txt", "README.md"}},
		{"a path a check also names still runs it", "cargo test", "ran cargo test", []string{"src/notes.md"}},
		{"a code path runs its check", "cargo test", "skipped npm test", []string{"src/a/b.rs", "docs/a.txt"}},
		{"a top level glob stays in its directory", "cargo test", "ran cargo test", []string{"Cargo.toml"}},
		{"both", "cargo test && npm test", "ran cargo test and npm test", []string{"src/a.rs", "web/a.js"}},
		{"a path nobody names runs everything", "make all", "scripts/x.sh is named by no entry", []string{"docs/a.txt", "scripts/x.sh"}},
		{"* does not cross a directory", "make all", "named by no entry", []string{"Cargo/x.toml"}},
		{"nothing to go on runs everything", "make all", "no changed paths", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := c.ScopedCommand(test.paths, "make all")
			if got.Command != test.want || !strings.Contains(got.Reason, test.reason) {
				t.Fatalf("got %+v, want %q with %q", got, test.want, test.reason)
			}
		})
	}
	if got := (Config{}).ScopedCommand([]string{"docs/a.txt"}, "make all"); got.Command != "make all" || got.Reason != "" {
		t.Fatalf("unset must change nothing: %+v", got)
	}
}

func TestAutonomyConfigValidates(t *testing.T) {
	queue := Config{Merge: "queue", MergeQueue: MergeQueue{Check: "make"}}
	withWorkflows := func(c Config, defects ...string) Config {
		c.Workflows.DefectsFrom = &defects
		return c
	}
	for _, test := range []struct {
		name string
		edit func(Config) Config
		want string
	}{
		{"scoped checks need something to scope", func(c Config) Config {
			c.Merge, c.MergeQueue, c.ScopedChecks = "", MergeQueue{}, []ScopedCheck{{Paths: []string{"a"}}}
			return c
		}, "neither is on"},
		{"a scoped check needs paths", func(c Config) Config { c.ScopedChecks = []ScopedCheck{{Command: "x"}}; return c }, "needs paths"},
		{"paths stay in the repository", func(c Config) Config {
			c.ScopedChecks = []ScopedCheck{{Command: "x", Paths: []string{"../a"}}}
			return c
		}, "relative"},
		{"a slow check outside defects_from", func(c Config) Config {
			c.SlowChecks = []SlowCheck{{Workflow: "Linux", When: "nightly"}}
			return withWorkflows(c, "CI")
		}, "must be in workflows.defects_from"},
		{"when is batch or nightly", func(c Config) Config {
			c.SlowChecks = []SlowCheck{{Workflow: "Linux", When: "weekly"}}
			return withWorkflows(c, "Linux")
		}, "batch or nightly"},
		{"batch needs the queue", func(c Config) Config {
			c.Merge, c.MergeQueue = "", MergeQueue{}
			c.SlowChecks = []SlowCheck{{Workflow: "Linux", When: "batch"}}
			return withWorkflows(c, "Linux")
		}, "needs merge: queue"},
		{"dispatch needs a command", func(c Config) Config { c.Dispatch.MaxAgents = 3; return c }, "agent_command"},
		{"good", func(c Config) Config {
			c.ScopedChecks = []ScopedCheck{{Paths: []string{"docs/**"}}}
			c.SlowChecks = []SlowCheck{{Workflow: "Linux", When: "batch"}}
			c.Dispatch.AgentCommand = "claude"
			return withWorkflows(c, "Linux")
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.edit(queue).ValidateAutonomy()
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
	loaded := queue
	loaded.AutoReady, loaded.Dispatch.AgentCommand = true, "claude"
	loaded.ScopedChecks = []ScopedCheck{{Paths: []string{"docs/**"}}}
	loaded.SlowChecks = []SlowCheck{{Workflow: "CI", When: "nightly"}}
	off := loaded.WithoutRelaxations()
	if off.AutoReady || off.Dispatches() || len(off.ScopedChecks) != 0 || len(off.SlowChecks) != 0 {
		t.Fatalf("not cleared: %+v", off)
	}
}
