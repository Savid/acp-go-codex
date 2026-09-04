//go:build windows

package codexacp

import (
	"errors"
	"os"
	"testing"
)

// Windows has no directory flush to fail. A commit is durable through the
// entry's own fsync and the journalled rename, so the ledger root is never
// opened on the commit path and a root that refuses to open cannot break a
// write.
func TestAuthLedgerWriteDoesNotFlushTheLedgerRoot(t *testing.T) {
	ledger := newTestLedger(t)

	restoreLedgerHooks(t)

	ledgerOpen = func(string) (*os.File, error) { return nil, errors.New("open") }

	if err := ledger.write(sampleLedgerRecord()); err != nil {
		t.Fatalf("write: %v", err)
	}
}
