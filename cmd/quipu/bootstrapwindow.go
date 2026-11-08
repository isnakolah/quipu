package main

// The bootstrap window. `quipu adopt --apply` writes Quipu's config into a
// repository that agents already work in, and from that moment the guard
// governs it, before the Writer, the token or the workflows exist. The window
// lets the agent that adopted finish setup: for a while, in that worktree
// only, the guard allows the setup commands and reads (see guard_adoption.go).
// The record lives in the clone's common git directory, where no agent may
// write, and names the one worktree it is for.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const bootstrapFileName = "quipu-bootstrap"

// defaultBootstrapMinutes and maxBootstrapMinutes bound `adopt --bootstrap-minutes`.
const (
	defaultBootstrapMinutes = 120
	maxBootstrapMinutes     = 24 * 60
)

// bootstrapWindow is the record adopt leaves for the adopting worktree.
type bootstrapWindow struct {
	Worktree string `json:"worktree"`
	GitDir   string `json:"git_dir"`
	Started  string `json:"started"`
	Expires  string `json:"expires"`
}

// open reports whether the window is open at now.
func (b bootstrapWindow) open(now time.Time) bool {
	expires, err := time.Parse(time.RFC3339, b.Expires)
	return err == nil && now.Before(expires)
}

// commonDir is the clone's shared git directory, read the way git records it
// so the guard needs no extra git process on every call.
func (w worktree) commonDir() string {
	raw, err := os.ReadFile(filepath.Join(w.GitDir, "commondir"))
	if err != nil {
		return w.GitDir
	}
	dir := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(w.GitDir, dir)
	}
	return filepath.Clean(dir)
}

// recordBootstrap opens a window of minutes for w, replacing any other.
func (w worktree) recordBootstrap(now time.Time, minutes int) (bootstrapWindow, error) {
	b := bootstrapWindow{Worktree: w.Top, GitDir: w.GitDir, Started: now.UTC().Format(time.RFC3339),
		Expires: now.UTC().Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return b, err
	}
	return b, os.WriteFile(filepath.Join(w.commonDir(), bootstrapFileName), append(raw, '\n'), 0o600)
}

// bootstrapFor returns the clone's window when it is open and was opened for w.
func (w worktree) bootstrapFor(now time.Time) (bootstrapWindow, bool) {
	raw, err := os.ReadFile(filepath.Join(w.commonDir(), bootstrapFileName))
	if err != nil {
		return bootstrapWindow{}, false
	}
	var b bootstrapWindow
	if json.Unmarshal(raw, &b) != nil || filepath.Clean(b.GitDir) != filepath.Clean(w.GitDir) {
		return bootstrapWindow{}, false
	}
	return b, b.open(now)
}

// configLanded reports whether origin's default branch, as this clone last
// fetched it, already holds Quipu's config: the adoption commit has landed.
func (w worktree) configLanded() bool {
	branch := w.defaultBranch()
	if branch == "" {
		return false
	}
	for _, name := range []string{rootConfigPath, configPath} {
		if _, err := w.git("cat-file", "-e", "origin/"+branch+":"+name); err == nil {
			return true
		}
	}
	return false
}

// validBootstrapMinutes checks the adopt flag.
func validBootstrapMinutes(minutes int) error {
	if minutes < 0 || minutes > maxBootstrapMinutes {
		return fmt.Errorf("--bootstrap-minutes must be 0 to %d, not %d", maxBootstrapMinutes, minutes)
	}
	return nil
}
