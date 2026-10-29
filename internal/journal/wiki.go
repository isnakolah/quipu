package journal

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PublishWiki appends one deterministic receipt-derived entry to the GitHub
// wiki repository. The wiki is generated output only: callers retain GitHub
// Issues, Project fields, and receipts as canonical delivery state.
//
// Token is passed only through a per-process Git HTTP header. It is never
// encoded into the remote URL, written to disk, or returned in an error.
func PublishWiki(ctx context.Context, owner, repository, token string, entry Entry) (bool, error) {
	if owner == "" || repository == "" || token == "" {
		return false, fmt.Errorf("wiki publish requires owner, repository, and token")
	}
	dir, err := os.MkdirTemp("", "quipu-wiki-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	remote := fmt.Sprintf("https://github.com/%s/%s.wiki.git", owner, repository)
	if err := runGit(ctx, "", token, "clone", "--depth", "1", remote, dir); err != nil {
		return false, fmt.Errorf("clone generated wiki: %w", err)
	}
	path := filepath.Join(dir, "Implementation-Log.md")
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	next := Append(string(current), entry)
	if next == string(current) {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return false, err
	}
	if err := runGit(ctx, dir, token, "config", "user.name", "Quipu Writer"); err != nil {
		return false, err
	}
	if err := runGit(ctx, dir, token, "config", "user.email", "quipu-writer@users.noreply.github.com"); err != nil {
		return false, err
	}
	if err := runGit(ctx, dir, token, "add", "Implementation-Log.md"); err != nil {
		return false, err
	}
	if err := runGit(ctx, dir, token, "commit", "-m", "quipu journal "+entry.RequestID); err != nil {
		return false, err
	}
	if err := runGit(ctx, dir, token, "push", "origin", "HEAD:master"); err != nil {
		return false, err
	}
	return true, nil
}

func runGit(ctx context.Context, dir, token string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitAuthEnv(token)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return nil
}

func gitAuthEnv(token string) []string {
	if token == "" {
		return nil
	}
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}
