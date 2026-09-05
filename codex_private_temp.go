package codexacp

import (
	"os"
)

var (
	privateMkdirTemp = os.MkdirTemp
	privateChmod     = os.Chmod
)

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
