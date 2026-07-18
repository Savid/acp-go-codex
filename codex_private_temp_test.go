package codexacp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreatePrivateTempFileErrors(t *testing.T) {
	resetHooks := func() {
		privateMkdirTemp = os.MkdirTemp
		privateChmod = os.Chmod
		privateCreateTemp = os.CreateTemp
	}
	t.Cleanup(resetHooks)

	blockedScratch := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blockedScratch, nil, 0o600); err != nil {
		t.Fatalf("write blocking scratch file: %v", err)
	}
	if _, err := createPrivateTempFile(blockedScratch, "prefix-", "file-*"); err == nil {
		t.Fatal("createPrivateTempFile ignored scratch parent error")
	}

	privateMkdirTemp = func(string, string) (string, error) {
		return "", errors.New("mkdir failed")
	}
	if _, err := createPrivateTempFile("", "prefix-", "file-*"); err == nil {
		t.Fatal("createPrivateTempFile ignored mkdir error")
	}

	resetHooks()
	dir := filepath.Join(t.TempDir(), "private")
	privateMkdirTemp = func(string, string) (string, error) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", err
		}

		return dir, nil
	}
	privateChmod = func(string, os.FileMode) error {
		return errors.New("chmod failed")
	}
	if _, err := createPrivateTempFile("", "prefix-", "file-*"); err == nil {
		t.Fatal("createPrivateTempFile ignored chmod error")
	}

	resetHooks()
	dir = filepath.Join(t.TempDir(), "private")
	privateMkdirTemp = func(string, string) (string, error) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", err
		}

		return dir, nil
	}
	privateCreateTemp = func(string, string) (*os.File, error) {
		return nil, errors.New("create failed")
	}
	if _, err := createPrivateTempFile("", "prefix-", "file-*"); err == nil {
		t.Fatal("createPrivateTempFile ignored create error")
	}
}

func TestPrivateTempFileModesAndCleanup(t *testing.T) {
	file, err := createPrivateTempFile("", "acp-go-codex-test-", "value-*")
	if err != nil {
		t.Fatalf("createPrivateTempFile returned error: %v", err)
	}
	name := file.Name()
	parent := filepath.Dir(name)
	if closeErr := file.Close(); closeErr != nil {
		t.Fatalf("close private temp file: %v", closeErr)
	}
	resolvedParent, resolveParentErr := filepath.EvalSymlinks(filepath.Dir(parent))
	resolvedScratch, resolveScratchErr := filepath.EvalSymlinks(scratchParent(""))
	if resolveParentErr != nil || resolveScratchErr != nil || resolvedParent != resolvedScratch {
		t.Fatalf("default private temp parent %q is not under the system temp directory", parent)
	}
	if info, statErr := os.Stat(parent); statErr != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private temp parent mode info=%v err=%v", info, statErr)
	}
	if info, statErr := os.Stat(name); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private temp file mode info=%v err=%v", info, statErr)
	}
	if removeErr := removePrivateTempFile(name, "acp-go-codex-test-", os.Remove); removeErr != nil {
		t.Fatalf("removePrivateTempFile returned error: %v", removeErr)
	}
	if _, statErr := os.Stat(parent); !os.IsNotExist(statErr) {
		t.Fatalf("private temp parent still exists: %v", statErr)
	}
	if _, createErr := createPrivateTempFile("", "acp-go-codex-test-", "bad/path"); createErr == nil {
		t.Fatal("createPrivateTempFile accepted invalid pattern")
	}

	file, err = createPrivateTempFile("", "acp-go-codex-test-", "value-*")
	if err != nil {
		t.Fatalf("create private temp for parent remove error: %v", err)
	}
	name = file.Name()
	parent = filepath.Dir(name)
	if err := file.Close(); err != nil {
		t.Fatalf("close private temp file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "sibling"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	if err := removePrivateTempFile(name, "acp-go-codex-test-", os.Remove); err == nil {
		t.Fatal("removePrivateTempFile ignored parent cleanup error")
	}
	_ = os.RemoveAll(parent)
}

func TestPrivateTempFileUnderScratchDir(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "nested", "scratch")

	file, err := createPrivateTempFile(scratch, "acp-go-codex-test-", "value-*")
	if err != nil {
		t.Fatalf("createPrivateTempFile returned error: %v", err)
	}
	name := file.Name()
	parent := filepath.Dir(name)
	if closeErr := file.Close(); closeErr != nil {
		t.Fatalf("close private temp file: %v", closeErr)
	}
	if filepath.Dir(parent) != scratch {
		t.Fatalf("private temp parent = %q, want under %q", parent, scratch)
	}
	if info, statErr := os.Stat(scratch); statErr != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch parent mode info=%v err=%v", info, statErr)
	}
	if removeErr := removePrivateTempFile(name, "acp-go-codex-test-", os.Remove); removeErr != nil {
		t.Fatalf("removePrivateTempFile returned error: %v", removeErr)
	}
	if _, statErr := os.Stat(parent); !os.IsNotExist(statErr) {
		t.Fatalf("private temp parent under scratch still exists: %v", statErr)
	}
	if _, statErr := os.Stat(scratch); statErr != nil {
		t.Fatalf("scratch parent was removed with the temp dir: %v", statErr)
	}
}
