package codexacp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeRolloutFileHookErrorBranches(t *testing.T) {
	origCreateRollout := createMaterializedRolloutTemp
	origRemoveRollout := removeMaterializedRolloutFile
	t.Cleanup(func() {
		createMaterializedRolloutTemp = origCreateRollout
		removeMaterializedRolloutFile = origRemoveRollout
	})

	createMaterializedRolloutTemp = func() (materializedRolloutFile, error) {
		return &fakeRolloutFile{name: "rollout", failWriteAt: 1}, nil
	}
	if _, err := materializeRollout([]SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err == nil {
		t.Fatal("materializeRollout ignored entry write error")
	}
	createMaterializedRolloutTemp = func() (materializedRolloutFile, error) {
		return &fakeRolloutFile{name: "rollout", failWriteAt: 2}, nil
	}
	if _, err := materializeRollout([]SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err == nil {
		t.Fatal("materializeRollout ignored newline write error")
	}
	createMaterializedRolloutTemp = func() (materializedRolloutFile, error) {
		return &fakeRolloutFile{name: "rollout", closeErr: errors.New("close failed")}, nil
	}
	if _, err := materializeRollout([]SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err == nil {
		t.Fatal("materializeRollout ignored close error")
	}
	createMaterializedRolloutTemp = origCreateRollout
	if err := removeMaterializedRollout(""); err != nil {
		t.Fatalf("empty remove returned error: %v", err)
	}
	removeMaterializedRolloutFile = func(string) error { return os.ErrNotExist }
	if err := removeMaterializedRollout("missing"); err != nil {
		t.Fatalf("missing rollout remove returned error: %v", err)
	}
	removeMaterializedRolloutFile = func(string) error { return errors.New("remove failed") }
	if err := removeMaterializedRollout("bad"); err == nil {
		t.Fatal("removeMaterializedRollout ignored remove error")
	}
}

func TestMaterializeRolloutBranches(t *testing.T) {
	emptyPath, err := materializeRollout(nil)
	if err != nil || emptyPath != "" {
		t.Fatalf("empty materializeRollout path=%q err=%v", emptyPath, err)
	}
	path, err := materializeRollout([]SessionStoreEntry{json.RawMessage(`{"type":"a"}`)})
	if err != nil {
		t.Fatalf("materializeRollout returned error: %v", err)
	}
	if err := removeMaterializedRollout(path); err != nil {
		t.Fatalf("removeMaterializedRollout returned error: %v", err)
	}
	if err := removeMaterializedRollout(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("remove missing returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "dir"), nil, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
}

func TestMaterializeRolloutRejectsInvalidTempDir(t *testing.T) {
	t.Setenv("TMPDIR", "/path/that/does/not/exist")
	if _, err := materializeRollout([]SessionStoreEntry{SessionStoreEntry(`{"x":1}`)}); err == nil {
		t.Fatal("materializeRollout accepted invalid TMPDIR")
	}
}

type fakeRolloutFile struct {
	name        string
	writes      int
	failWriteAt int
	closeErr    error
}

func (f *fakeRolloutFile) Name() string { return f.name }
func (f *fakeRolloutFile) Write([]byte) (int, error) {
	f.writes++
	if f.failWriteAt == f.writes {
		return 0, errors.New("write failed")
	}

	return 1, nil
}
func (f *fakeRolloutFile) Close() error { return f.closeErr }
