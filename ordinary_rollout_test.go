package codexacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOrdinaryNativeAppendLogFailsClosedOnAPartialTrailingRecord pins that a
// rollout read landing between a row's bytes and its newline reports the
// unfinished record rather than returning it, and that trailing whitespace
// after the last newline is still a complete file.
func TestOrdinaryNativeAppendLogFailsClosedOnAPartialTrailingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"type\":\"one\",\"payload\":{}}\n{\"type\":\"tw"), 0o600))

	_, err := readOrdinaryNativeAppendLog(path, 0)
	require.ErrorIs(t, err, errPartialNativeAppendLogRecord)

	require.NoError(t, os.WriteFile(path, []byte("{\"type\":\"one\",\"payload\":{}}\r\n{\"type\":\"two\",\"payload\":{}}\n  \n\t"), 0o600))

	// Records mirror the file's bytes exactly: a carriage return before the
	// newline is part of the row, never normalized away.
	records, err := readOrdinaryNativeAppendLog(path, 0)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, SessionStoreEntry("{\"type\":\"one\",\"payload\":{}}\r"), records[0])
}
