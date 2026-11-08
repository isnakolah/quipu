package main

// Help. `quipu <cmd> --help` and `quipu help <cmd>` print usage and never run
// the command, so the guard lets every agent ask. The guard and the CLI share
// helpRequest: whatever it calls help, run answers with usage alone.

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
)

// subcommands lists each command that takes one, so help can name them and
// reach the flags of each.
var subcommands = map[string][]string{
	"configure": {"writer-token"},
	"evidence":  {"submit"},
	"issue":     {"create", "link-parent", "add-blocker"},
	"journal":   {"render"},
	"patch":     {"apply"},
	"lease":     {"release"},
	"policy":    {"pr"},
	"pr":        {"link"},
	"queue":     {"status", "run", "resume"},
	"request":   {"status"},
	"runner":    {"status", "setup"},
	"work":      {"list", "claim", "start", "renew", "release", "block", "resume"},
	"worktree":  {"remove"},
	"writer":    {"run", "reconcile", "defect", "serve"},
}

// leafCommands is every top-level command without subcommands.
var leafCommands = []string{"adopt", "check", "config", "context", "daemon", "digest", "doctor", "done", "guard", "guide", "hooks",
	"import", "init", "move", "park", "propose", "protect", "ready", "start", "status", "triage", "validate",
	"verify-commit", "version"}

// commandPaths is every command a caller can run, subcommands included.
func commandPaths() [][]string {
	paths := make([][]string, 0, 48)
	for _, name := range leafCommands {
		paths = append(paths, []string{name})
	}
	for name, subs := range subcommands {
		for _, sub := range subs {
			paths = append(paths, []string{name, sub})
		}
	}
	sort.Slice(paths, func(i, j int) bool { return strings.Join(paths[i], " ") < strings.Join(paths[j], " ") })
	return paths
}

func isHelpFlag(arg string) bool { return arg == "-h" || arg == "-help" || arg == "--help" }

// helpRequest reports whether args only ask for help, and for which command
// words. It is `help WORDS...`, `--help WORDS...`, or `WORDS... --help` where
// no word is a flag: a flag before --help could take it as its value and the
// command would run.
func helpRequest(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if args[0] == "help" || isHelpFlag(args[0]) {
		return bareWords(args[1:]), true
	}
	last := len(args) - 1
	if !isHelpFlag(args[last]) {
		return nil, false
	}
	for _, arg := range args[:last] {
		if strings.HasPrefix(arg, "-") {
			return nil, false
		}
	}
	return args[:last], true
}

// bareWords is args up to the first flag.
func bareWords(args []string) []string {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return args[:i]
		}
	}
	return args
}

// printHelp prints usage for the command words in path. A leaf command prints
// its flags; the flag package stops it before it does anything.
func printHelp(path []string) error {
	if len(path) == 0 {
		fmt.Println(usageText)
		return nil
	}
	name := path[0]
	leaf := []string{name}
	if subs, ok := subcommands[name]; ok {
		if len(path) < 2 || !contains(subs, path[1]) {
			fmt.Printf("usage: quipu %s {%s} [flags]\n\nRun `quipu %s <subcommand> --help` for its flags.\n", name, strings.Join(subs, "|"), name)
			return nil
		}
		leaf = append(leaf, path[1])
	} else if !contains(leafCommands, name) {
		return fmt.Errorf("unknown command %q; run `quipu help` for the list", name)
	}
	if err := dispatch(append(leaf, "--help")); err != nil && !errors.Is(err, flag.ErrHelp) {
		return err
	}
	return nil
}

// keepHelp returns err when it is a request for help, so --help exits cleanly,
// and the usage error otherwise.
func keepHelp(err error, usage string) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errors.New(usage)
}
