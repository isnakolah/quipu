package main

// `guard: deny-list` refuses only what is dangerous or irreversible: force
// pushes and pushes to the default branch, merging, approving or closing
// outside Quipu, another agent's live worktree or branch, Quipu's own files,
// the commands a repository lists under guard_deny, and changes to a governed
// worktree's files without a lease (commits, redirects and rm/mv into it).
// Every other command passes, so reads, scratch writes, interpreters, loops,
// `--help` and disk checks never stop an agent. With the setting unset the
// guard keeps its allow-list, unchanged.

import (
	"path/filepath"
	"strings"

	"github.com/isnakolah/quipu/internal/model"
)

// configFor returns the enforced config of dir's repository, read once per
// worktree for the whole tool call.
func (g *guard) configFor(dir string) (model.Config, bool) {
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return model.Config{}, false
	}
	if entry, ok := g.configs[w.Top]; ok {
		return entry.c, entry.ok
	}
	c, _, err := loadEnforcedConfig(w, false)
	ok := err == nil && hasConfig(w.Top)
	g.configs[w.Top] = configEntry{c, ok}
	return c, ok
}

type configEntry struct {
	c  model.Config
	ok bool
}

// denyList reports whether dir's repository sets guard: deny-list.
func (g *guard) denyList(dir string) bool {
	c, ok := g.configFor(dir)
	return ok && c.GuardDenyList()
}

// declaredDeny refuses a command the repository lists under guard_deny.
func (g *guard) declaredDeny(args []shellArg, dir string) error {
	c, ok := g.configFor(dir)
	if !ok || len(c.GuardDeny) == 0 {
		return nil
	}
	for _, entry := range c.GuardDeny {
		words := strings.Fields(entry)
		if len(words) == 0 || len(args) < len(words) {
			continue
		}
		match := true
		for i, word := range words {
			arg := args[i].s
			if i == 0 {
				arg = filepath.Base(arg)
			}
			if !args[i].ok || arg != word {
				match = false
				break
			}
		}
		if match {
			return deny("guard-deny", "the repository lists `"+entry+"` under guard_deny")
		}
	}
	return nil
}

// subcommandTools take `help` or `-h` after a subcommand.
var subcommandTools = setOf("git", "gh", "quipu", "docker", "cargo", "go", "npm", "pnpm", "yarn", "kubectl", "brew", "make")

// helpForm reports whether a command only prints its own help. Commands that
// write their path arguments are left out: BSD rm takes `rm f --help` as two
// files to delete, and `ln -h` changes how links are made.
func helpForm(args []shellArg) bool {
	for _, arg := range args {
		if !arg.ok {
			return false
		}
	}
	name := filepath.Base(args[0].s)
	if fileArgCommands[name] {
		return false
	}
	last := args[len(args)-1].s
	switch {
	case len(args) > 1 && (last == "--help" || last == "-help"):
		return true
	case len(args) == 2 && last == "-h":
		return true
	case subcommandTools[name] && len(args) == 3 && last == "-h":
		return true
	case subcommandTools[name] && len(args) > 1 && args[1].s == "help":
		return true
	}
	return false
}

// unknownCommand judges, in deny-list mode, a command the guard has no rule
// for: it passes unless it names Quipu's own state or enforcement files.
func (g *guard) unknownCommand(args []shellArg) error {
	for _, arg := range args {
		if arg.ok && mentionsQuipuState(arg.s) {
			return deny("git-internals", arg.s)
		}
		if arg.ok && mentionsProtectedPath(arg.s) {
			return deny("quipu-config", arg.s)
		}
	}
	return nil
}

// governed reports whether path lies inside a worktree Quipu governs.
func governed(path string) bool {
	w, err := openWorktree(existingDir(path))
	return err == nil && hasConfig(w.Top)
}

// outsideGoverned reports whether a computed path's literal start already
// puts it outside every governed worktree, wherever the rest points.
func (g *guard) outsideGoverned(prefix string) bool {
	if !filepath.IsAbs(prefix) {
		return false
	}
	dir := prefix
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		dir = filepath.Dir(prefix)
	}
	return dir != string(filepath.Separator) && !governed(dir)
}

// findExec returns the command `find -exec`, `-execdir`, `-ok` or `-okdir`
// runs, with `{}` as a literal, or nil when find runs none.
func findExec(args []shellArg) ([][]shellArg, bool) {
	var commands [][]shellArg
	for i := 0; i < len(args); i++ {
		switch args[i].s {
		case "-exec", "-execdir", "-ok", "-okdir":
			var command []shellArg
			for i++; i < len(args) && args[i].s != ";" && args[i].s != "+"; i++ {
				command = append(command, args[i])
			}
			if len(command) == 0 {
				return nil, false
			}
			commands = append(commands, command)
		case "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
			return nil, false
		}
	}
	return commands, true
}

// commandLine shows a simple command as the agent wrote it, computed parts as …
func commandLine(args []shellArg) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		if arg.ok {
			parts[i] = arg.s
		} else {
			parts[i] = arg.prefix + "…"
		}
	}
	return strings.Join(parts, " ")
}
