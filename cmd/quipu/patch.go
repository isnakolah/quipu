package main

// Enforcement patches: an agent that needs a change to one of Quipu's own files
// cannot make it, so `quipu propose --patch FILE` attaches the exact patch to its
// proposal. The digest lists it as one command for the owner, `quipu patch apply
// ID`, which applies it to the owner's checkout and commits nothing.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/isnakolah/quipu/internal/github"
)

const (
	patchMarker = "<!-- quipu-patch:v1 -->"
	patchEnd    = "<!-- /quipu-patch -->"
	// patchMaxBytes keeps the comment under GitHub's 65536-character limit.
	patchMaxBytes = 60000
)

var backtickRun = regexp.MustCompile("`{3,}")

// patchFiles lists the files a unified diff changes, and rejects anything that
// is not one or that reaches outside the repository.
func patchFiles(patch string) ([]string, error) {
	if len(patch) > patchMaxBytes {
		return nil, fmt.Errorf("the patch is %d bytes; the most a proposal carries is %d", len(patch), patchMaxBytes)
	}
	if strings.Contains(patch, "quipu-request") || strings.Contains(patch, "quipu-receipt") || strings.Contains(patch, patchMarker) || strings.Contains(patch, patchEnd) {
		return nil, errors.New("a patch cannot contain Quipu markers")
	}
	var files []string
	seen := map[string]bool{}
	var from string
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			from = patchPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			file := patchPath(strings.TrimPrefix(line, "+++ "))
			if file == "/dev/null" {
				file = from
			}
			if file == "" || file == "/dev/null" || strings.HasPrefix(file, "/") || strings.Contains("/"+file+"/", "/../") || insideGitDir(file) {
				return nil, fmt.Errorf("the patch changes %q; a patch may change only files inside the repository", file)
			}
			if !seen[file] {
				seen[file] = true
				files = append(files, file)
			}
		}
	}
	if len(files) == 0 {
		return nil, errors.New("the file is not a unified diff: it has no ---/+++ file lines. Make it with git diff, or diff -u a/PATH b/PATH")
	}
	return files, nil
}

// patchPath is the repository path a ---/+++ line names, without its a/ or b/.
func patchPath(field string) string {
	field = strings.TrimSpace(field)
	if tab := strings.IndexByte(field, '\t'); tab >= 0 {
		field = field[:tab]
	}
	if field == "/dev/null" {
		return field
	}
	if rest, ok := strings.CutPrefix(field, "a/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(field, "b/"); ok {
		return rest
	}
	return field
}

// patchSection is the part of a proposal that carries the patch, in a fence
// longer than any backtick run inside it.
func patchSection(patch string, files []string) string {
	fence := "```"
	for _, run := range backtickRun.FindAllString(patch, -1) {
		if len(run) >= len(fence) {
			fence = strings.Repeat("`", len(run)+1)
		}
	}
	patch = strings.TrimRight(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	return fmt.Sprintf("%s\n**Patch for the owner** (%s). Apply it with `quipu patch apply <comment id>`; the digest names the id.\n\n%sdiff\n%s\n%s\n%s\n",
		patchMarker, strings.Join(files, ", "), fence, patch, fence, patchEnd)
}

// extractPatch takes the patch back out of a proposal comment.
func extractPatch(body string) (string, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	_, rest, ok := strings.Cut(body, patchMarker+"\n")
	if !ok {
		return "", errors.New("the comment carries no Quipu patch")
	}
	rest, _, ok = strings.Cut(rest, "\n"+patchEnd)
	if !ok {
		return "", errors.New("the patch in the comment is cut short")
	}
	lines := strings.Split(rest, "\n")
	open := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "```") && strings.HasSuffix(line, "diff") {
			open = i
			break
		}
	}
	if open < 0 || len(lines) < open+2 {
		return "", errors.New("the patch in the comment has no diff block")
	}
	fence := strings.TrimSuffix(lines[open], "diff")
	if lines[len(lines)-1] != fence {
		return "", errors.New("the patch in the comment is cut short")
	}
	return strings.Join(lines[open+1:len(lines)-1], "\n") + "\n", nil
}

func patchCommand(args []string) error {
	if len(args) == 0 || args[0] != "apply" {
		return errors.New("usage: quipu patch apply COMMENT_ID [--check] [--json]  (owner only)")
	}
	fs := flag.NewFlagSet("patch apply", flag.ContinueOnError)
	check := fs.Bool("check", false, "only say whether the patch applies")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: quipu patch apply COMMENT_ID [--check] [--json]  (owner only)")
	}
	id := strings.TrimPrefix(fs.Arg(0), "#")
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(id) {
		return fmt.Errorf("%q is not a comment id", fs.Arg(0))
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	var comment github.Comment
	if err := github.NewClient().Do(context.Background(), http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/comments/%s", c.Owner, c.Repository, id), nil, &comment); err != nil {
		return err
	}
	patch, err := extractPatch(comment.Body)
	if err != nil {
		return err
	}
	files, err := patchFiles(patch)
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "quipu-patch")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	file := filepath.Join(scratch, "patch.diff")
	if err := os.WriteFile(file, []byte(patch), 0o600); err != nil {
		return err
	}
	gitArgs := []string{"-C", here.Top, "apply", "--check", file}
	if output, err := exec.Command("git", gitArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("the patch does not apply here, perhaps because it is already applied: %s", strings.TrimSpace(string(output)))
	}
	status, next := "Applies", fmt.Sprintf("Run quipu patch apply %s to apply it.", id)
	if !*check {
		if output, err := exec.Command("git", "-C", here.Top, "apply", file).CombinedOutput(); err != nil {
			return fmt.Errorf("git apply: %s", strings.TrimSpace(string(output)))
		}
		status, next = "Applied", "Review the change with git diff, then commit and merge it yourself; nothing was committed."
	}
	if *asJSON {
		return printValue(map[string]any{"status": status, "comment": id, "files": files, "proposed_by": comment.User.Login, "next": next}, true)
	}
	fmt.Printf("%s: %s (proposed by %s)\n%s\n", status, strings.Join(files, ", "), comment.User.Login, next)
	return nil
}
