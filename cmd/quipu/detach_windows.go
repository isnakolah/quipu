package main

import (
	"os"
	"os/exec"
)

// detach does nothing here: Windows does not signal a process group on exit.
func detach(*exec.Cmd) {}

// lockFile does nothing here, so two agents starting at once may both run
// runner_start.
func lockFile(*os.File) error { return nil }
