package codexacp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
)

// errPartialNativeAppendLogRecord names a rollout row the native side has begun
// but not finished: bytes after the last newline are a record still being
// written, and a read that lands there fails closed rather than handing back a
// half row the mirror would then commit as terminal evidence.
var errPartialNativeAppendLogRecord = errors.New("native append-log ends inside a record")

func readOrdinaryNativeAppendLog(path string, after uint64) ([]SessionStoreEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, maxSessionImportLineBytes)
	scanner.Split(scanTerminatedLines)

	var row uint64

	records := make([]SessionStoreEntry, 0)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		if row >= after {
			records = append(records, append(SessionStoreEntry(nil), line...))
		}

		row++
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if after > row {
		return nil, fmt.Errorf("native append-log cursor %d exceeds row count %d", after, row)
	}

	return records, nil
}

// scanTerminatedLines splits on newlines with two differences from
// bufio.ScanLines: a record's bytes are returned exactly, with no carriage
// return stripped, because the store mirrors native rows without
// normalization and the managed broker reads the same file the same way; and a
// final line the file does not terminate is not a record. Trailing whitespace
// is a complete file; anything else after the last newline is a row still
// being appended.
func scanTerminatedLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}

	if !atEOF {
		return 0, nil, nil
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return len(data), nil, nil
	}

	return 0, nil, errPartialNativeAppendLogRecord
}
