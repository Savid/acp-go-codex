//go:build unix

package codexacp

import "syscall"

// handoffOpenFlags stop a FIFO or a device node from parking open(2) in the
// kernel for the lifetime of the process. Confining the open to the root already
// refuses anything outside it, but a confined root deliberately does not refuse
// a device or a pipe, so the descriptor still has to be checked once it exists.
const handoffOpenFlags = syscall.O_NONBLOCK
