package codexacp

import (
	"os"
	"path/filepath"
	"strings"
)

var (
	privateMkdirTemp  = os.MkdirTemp
	privateChmod      = os.Chmod
	privateCreateTemp = os.CreateTemp
)

func createPrivateTempFile(dirPrefix string, filePattern string) (*os.File, error) {
	dir, err := privateMkdirTemp("", dirPrefix)
	if err != nil {
		return nil, err
	}

	if err = privateChmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)

		return nil, err
	}

	file, err := privateCreateTemp(dir, filePattern)
	if err != nil {
		_ = os.RemoveAll(dir)

		return nil, err
	}

	return file, nil
}

func removePrivateTempFile(path string, dirPrefix string, removeFile func(string) error) error {
	if path == "" {
		return nil
	}

	if err := removeFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}

	dir := filepath.Dir(path)
	if strings.HasPrefix(filepath.Base(dir), dirPrefix) {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}
