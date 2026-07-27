//go:build darwin

package main

import (
	"github.com/savid/acp-go-codex/internal/codex"
)

var diagnoseContainment = codex.DiagnoseDarwinContainment

var cleanupContainment = codex.CleanupDarwinContainment
