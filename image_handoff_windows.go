//go:build windows

package codexacp

// handoffOpenFlags is empty because the handoff form cannot be reached on
// Windows: every file:///C:/... spelling leaves a path that fails
// filepath.IsAbs once FromSlash has run, so the block is refused as
// invalid_handoff long before anything is opened. Windows has no equivalent of
// the non-blocking open the form depends on, so it stays unreachable rather
// than half-enabled.
const handoffOpenFlags = 0
