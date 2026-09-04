//go:build !windows

package codexacp

import (
	"errors"
	"os"
	"testing"
)

// The commit is only as durable as the directory entry that names it, so a
// ledger root that cannot be opened for the post-rename flush fails the write
// rather than reporting a durability the platform did not deliver.
func TestAuthLedgerWriteFailsWhenTheLedgerRootCannotBeFlushed(t *testing.T) {
	ledger := newTestLedger(t)

	restoreLedgerHooks(t)

	ledgerOpen = func(string) (*os.File, error) { return nil, errors.New("open") }

	if err := ledger.write(sampleLedgerRecord()); err == nil {
		t.Fatal("a failed write reported success")
	}
}
