//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunDetachedLeavesQuipusProcessGroup(t *testing.T) {
	top := t.TempDir()
	if err := runDetached(top, `ps -o pgid= -p $$ >group`, filepath.Join(top, "log"), time.Minute); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(top, "group"))
	if err != nil {
		t.Fatal(err)
	}
	group, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if group == syscall.Getpgrp() {
		t.Fatalf("still in quipu's process group %d", group)
	}
}
