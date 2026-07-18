//go:build windows

package codex

import "syscall"

// The shared coverage tests replace these Unix seams. Define test-only
// equivalents so Windows compilation exercises the Windows production files
// without excluding the rest of the package's test corpus.
var (
	getProcessGroupID = func(pid int) (int, error) { return pid, nil }
	killProcessID     = func(int, syscall.Signal) error { return nil }
)
