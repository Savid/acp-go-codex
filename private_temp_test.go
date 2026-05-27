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
	privateMkdirTemp = func(string, string) (string, error) {
		return "", errors.New("mkdir failed")
	}
	if _, err := createPrivateTempFile("prefix-", "file-*"); err == nil {
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
	if _, err := createPrivateTempFile("prefix-", "file-*"); err == nil {
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
	if _, err := createPrivateTempFile("prefix-", "file-*"); err == nil {
		t.Fatal("createPrivateTempFile ignored create error")
	}
}

func TestPrivateTempFileModesAndCleanup(t *testing.T) {
	file, err := createPrivateTempFile("acp-go-codex-test-", "value-*")
	if err != nil {
		t.Fatalf("createPrivateTempFile returned error: %v", err)
	}
	name := file.Name()
	parent := filepath.Dir(name)
	if err := file.Close(); err != nil {
		t.Fatalf("close private temp file: %v", err)
	}
	if info, err := os.Stat(parent); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private temp parent mode info=%v err=%v", info, err)
	}
	if info, err := os.Stat(name); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private temp file mode info=%v err=%v", info, err)
	}
	if err := removePrivateTempFile(name, "acp-go-codex-test-", os.Remove); err != nil {
		t.Fatalf("removePrivateTempFile returned error: %v", err)
	}
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("private temp parent still exists: %v", err)
	}
	if _, err := createPrivateTempFile("acp-go-codex-test-", "bad/path"); err == nil {
		t.Fatal("createPrivateTempFile accepted invalid pattern")
	}

	file, err = createPrivateTempFile("acp-go-codex-test-", "value-*")
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
