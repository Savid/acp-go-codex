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

func createPrivateTempFile(scratchDir string, dirPrefix string, filePattern string) (*os.File, error) {
	dir, err := createPrivateTempDir(scratchDir, dirPrefix)
	if err != nil {
		return nil, err
	}

	file, err := privateCreateTemp(dir, filePattern)
	if err != nil {
		_ = os.RemoveAll(dir)

		return nil, err
	}

	return file, nil
}

func createPrivateTempDir(scratchDir string, dirPrefix string) (string, error) {
	parent, err := ensureScratchParent(scratchDir)
	if err != nil {
		return "", err
	}

	dir, err := privateMkdirTemp(parent, dirPrefix)
	if err != nil {
		return "", err
	}

	if err = privateChmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)

		return "", err
	}

	return dir, nil
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
