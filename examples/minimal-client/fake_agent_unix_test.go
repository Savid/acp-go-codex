//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeAgentCommand builds a stand-in for the agent process: something that
// starts, holds its pipes open, and exits cleanly when its stdin closes.
func fakeAgentCommand(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()

	script := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	return exec.CommandContext(ctx, script) // #nosec G204 -- fixed test fixture.
}
