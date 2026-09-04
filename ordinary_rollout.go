package codexacp

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
)

func readOrdinaryNativeAppendLog(path string, after uint64) ([]SessionStoreEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, maxSessionImportLineBytes)

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
