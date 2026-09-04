//go:build unix

package codexacp

import (
	"path/filepath"
	"syscall"
)

// handoffOpenFlags stop a FIFO or a device node from parking open(2) in the
// kernel for the lifetime of the process. Confining the open to the root already
// refuses anything outside it, but a confined root deliberately does not refuse
// a device or a pipe, so the descriptor still has to be checked once it exists.
const handoffOpenFlags = syscall.O_NONBLOCK

// handoffURIFilePath turns the path component of a file URI into a host path.
// On a Unix host the two spellings are the same path, so only the separator
// changes.
func handoffURIFilePath(path string) string { return filepath.FromSlash(path) }
