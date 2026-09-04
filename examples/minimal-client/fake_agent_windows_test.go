//go:build windows

package main

import (
	"context"
	"os/exec"
	"testing"
)

// fakeAgentCommand builds a stand-in for the agent process: something that
// starts, holds its pipes open, and exits cleanly when its stdin closes. A
// shell script cannot be that on Windows, which resolves an executable by
// PATHEXT and has no interpreter line, so the stand-in is an executable the
// system already ships with exactly that behaviour.
func fakeAgentCommand(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()

	return exec.CommandContext(ctx, "sort") // #nosec G204 -- fixed test fixture.
}
