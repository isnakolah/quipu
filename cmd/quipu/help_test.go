package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestHelpRequestShapes(t *testing.T) {
	cases := []struct {
		args []string
		path []string
		ok   bool
	}{
		{[]string{"issue", "create", "--help"}, []string{"issue", "create"}, true},
		{[]string{"worktree", "--help"}, []string{"worktree"}, true},
		{[]string{"start", "-h"}, []string{"start"}, true},
		{[]string{"help", "issue", "create"}, []string{"issue", "create"}, true},
		{[]string{"help"}, []string{}, true},
		{[]string{"--help"}, []string{}, true},
		{[]string{"help", "writer", "run", "--apply"}, []string{"writer", "run"}, true},
		// A flag before --help could take it as its value, so the command
		// would run: that is not a help request.
		{[]string{"issue", "create", "--title", "-h"}, nil, false},
		{[]string{"issue", "create", "--apply", "--help"}, nil, false},
		{[]string{"status", "--json"}, nil, false},
		{[]string{"issue", "create"}, nil, false},
		{nil, nil, false},
	}
	for _, c := range cases {
		path, ok := helpRequest(c.args)
		if ok != c.ok || (ok && !slices.Equal(path, c.path)) {
			t.Errorf("helpRequest(%q) = %q, %v; want %q, %v", c.args, path, ok, c.path, c.ok)
		}
	}
}

// dispatchedCommands reads the top-level commands from dispatch's switch, so a
// new command cannot miss the help and --json tests below.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if clause, ok := n.(*ast.CaseClause); ok {
				for _, expr := range clause.List {
					if lit, ok := expr.(*ast.BasicLit); ok {
						name, _ := strconv.Unquote(lit.Value)
						names = append(names, name)
					}
				}
			}
			return true
		})
		return false
	})
	if len(names) < 30 {
		t.Fatalf("found only %v in dispatch", names)
	}
	return names
}

func TestEveryCommandIsInTheHelpTree(t *testing.T) {
	for _, name := range dispatchedCommands(t) {
		if _, ok := subcommands[name]; !ok && !contains(leafCommands, name) {
			t.Errorf("command %q is in neither leafCommands nor subcommands", name)
		}
	}
}

func TestEveryCommandTakesJSONAndHelp(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, path := range commandPaths() {
		// --json comes first: a command without it fails on it before --help.
		args := append(append([]string{}, path...), "--json", "--help")
		if err := dispatch(args); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("quipu %s: want help, got %v", strings.Join(args, " "), err)
		}
	}
}

func TestEveryCommandPrintsJSONErrorOnFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, path := range commandPaths() {
		args := append(append([]string{}, path...), "--json", "--no-such-flag")
		err := dispatch(args)
		if err == nil {
			t.Fatalf("quipu %s succeeded", strings.Join(args, " "))
		}
		var stdout, stderr bytes.Buffer
		printError(err, args, &stdout, &stderr)
		var decoded struct {
			Error *flowError `json:"error"`
		}
		if json.Unmarshal(stdout.Bytes(), &decoded) != nil || decoded.Error == nil || decoded.Error.Code == "" || decoded.Error.Message == "" || decoded.Error.Next == "" {
			t.Errorf("quipu %s: stdout %q is not a JSON error", strings.Join(args, " "), stdout.String())
		}
	}
}

func TestHelpRunsNothingAndSucceeds(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"issue", "create", "--help"},
		{"worktree", "--help"},
		{"help", "worktree"},
		{"help", "issue", "create"},
		{"help"},
		{"--help"},
		{"start", "--next", "--help"},
		{"move", "--help"},
		{"verify-commit", "--help"},
		{"guard", "--help"},
	} {
		if err := run(args); err != nil {
			t.Errorf("quipu %s: %v", strings.Join(args, " "), err)
		}
	}
	if err := run([]string{"help", "no-such-command"}); err == nil {
		t.Error("help for an unknown command must fail")
	}
}

func TestGuardDenialPrintsJSONWithJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	printError(deny("quipu-admin", "quipu writer run"), []string{"guard", "--json"}, &stdout, &stderr)
	var decoded struct {
		Error flowError `json:"error"`
	}
	if json.Unmarshal(stdout.Bytes(), &decoded) != nil || decoded.Error.Code != "quipu-admin" || decoded.Error.Next == "" {
		t.Fatalf("stdout %q", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "QUIPU DENIED [quipu-admin]") {
		t.Fatalf("the hook reason must stay on stderr: %q", stderr.String())
	}
}

func TestGuardAllowsQuipuHelp(t *testing.T) {
	f := newGuardFixture(t)
	for _, cwd := range []string{f.a, f.top} {
		for _, command := range []string{
			"quipu issue create --help",
			"quipu worktree --help",
			"quipu help issue create",
			"quipu writer run --help",
			"quipu daemon -h",
			"quipu --help",
		} {
			if code := guardCode(t, cwd, "Bash", map[string]any{"command": command}); code != "" {
				t.Errorf("%s in %s: denied %s", command, cwd, code)
			}
		}
	}
	// A flag before --help may take it as its value and run the command.
	for _, command := range []string{
		"quipu writer run --issue-number 1 --apply --help",
		"quipu import --manifest -h --apply",
		"quipu worktree add x",
	} {
		if code := guardCode(t, f.a, "Bash", map[string]any{"command": command}); code != "quipu-admin" {
			t.Errorf("%s: want quipu-admin, got %q", command, code)
		}
	}
}
