//go:build darwin

package main

import (
	"io"

	"github.com/savid/acp-go-codex/internal/codex"
)

var diagnoseContainment = func(scratchDir string, output io.Writer) error {
	return codex.DiagnoseDarwinContainment(scratchDir, output)
}

var cleanupContainment = func(scratchDir, runtimeID string, force bool, output io.Writer) error {
	return codex.CleanupDarwinContainment(scratchDir, runtimeID, force, output)
}
