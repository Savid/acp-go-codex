package codexacp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreatePrivateTempDirErrors(t *testing.T) {
	t.Cleanup(func() {
		privateMkdirTemp = os.MkdirTemp
		privateChmod = os.Chmod
	})

	blockedScratch := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blockedScratch, nil, 0o600))

	_, err := createPrivateTempDir(blockedScratch, "prefix-")
	require.Error(t, err, "createPrivateTempDir ignored scratch parent error")

	privateMkdirTemp = func(string, string) (string, error) { return "", errors.New("mkdir failed") }
	_, err = createPrivateTempDir("", "prefix-")
	require.ErrorContains(t, err, "mkdir failed")

	privateMkdirTemp = os.MkdirTemp
	privateChmod = func(string, os.FileMode) error { return errors.New("chmod failed") }
	_, err = createPrivateTempDir("", "prefix-")
	require.ErrorContains(t, err, "chmod failed")
}

func TestPrivateTempDirModeAndScratchPlacement(t *testing.T) {
	dir, err := createPrivateTempDir("", "acp-go-codex-test-")
	require.NoError(t, err)

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	scratch := filepath.Join(t.TempDir(), "scratch")
	scoped, err := createPrivateTempDir(scratch, "acp-go-codex-test-")
	require.NoError(t, err)
	require.Equal(t, scratch, filepath.Dir(scoped))
}
