package main

// workflow_changes: flagged opens .github/workflows/ to agents, except Quipu's
// own workflows. Such a change passes policy without the owner's label, and the
// digest lists it, so the owner sees it without being asked. Every other
// enforcement file stays owner-only. The setting counts only from the default
// branch's config: policy runs from it, and the guard reads it from there.

import (
	"path"
	"strings"

	"github.com/isnakolah/quipu/internal/model"
)

const workflowsDir = ".github/workflows/"

// workflowFile reports whether p names one file directly in .github/workflows/:
// not the directory, a subdirectory, or a glob.
func workflowFile(p string) bool {
	name, ok := strings.CutPrefix(p, workflowsDir)
	return ok && name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/*?[{")
}

// quipuWorkflow reports whether p is one of Quipu's own workflows, which
// enforce the flow and so stay owner-only whatever the setting.
func quipuWorkflow(p string) bool {
	return workflowFile(p) && strings.HasPrefix(strings.ToLower(path.Base(p)), "quipu")
}

// flaggedWorkflow reports whether c lets agents change p, flagged for the owner.
func flaggedWorkflow(c model.Config, p string) bool {
	return c.FlagsWorkflowChanges() && workflowFile(p) && !quipuWorkflow(p)
}

// ownerOnlyFile reports whether a change to p needs the owner's approval label.
func ownerOnlyFile(c model.Config, p string) bool {
	return protectedFile(p) && !flaggedWorkflow(c, p)
}

// flaggedWorkflowFiles lists the workflow files a PR changes that pass policy
// only because workflow_changes is flagged.
func flaggedWorkflowFiles(c model.Config, files []string) []string {
	var flagged []string
	for _, file := range files {
		if flaggedWorkflow(c, file) {
			flagged = append(flagged, file)
		}
	}
	return flagged
}

// changedWorkflows lists the files under .github/workflows/ in files.
func changedWorkflows(files []string) []string {
	var changed []string
	for _, file := range files {
		if strings.HasPrefix(file, workflowsDir) {
			changed = append(changed, file)
		}
	}
	return changed
}

// mentionsOwnerOnlyPath reports whether a command argument names an
// enforcement file c keeps owner-only. With workflow_changes: flagged, an
// argument naming one workflow other than Quipu's own passes; the directory
// itself, a glob, or Quipu's workflows do not.
func mentionsOwnerOnlyPath(c model.Config, arg string) bool {
	if !mentionsProtectedPath(arg) {
		return false
	}
	if !c.FlagsWorkflowChanges() {
		return true
	}
	at := strings.Index(arg, workflowsDir)
	if at < 0 {
		return true
	}
	rest := arg[at:]
	if strings.Count(arg, ".github") != 1 || !flaggedWorkflow(c, rest) {
		return true
	}
	// Nothing else protected may hide in the same argument.
	return mentionsProtectedPath(arg[:at])
}
