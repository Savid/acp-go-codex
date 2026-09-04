package codexacp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScratchParent(t *testing.T) {
	if got := scratchParent(""); got != os.TempDir() {
		t.Fatalf("scratchParent(\"\") = %q, want %q", got, os.TempDir())
	}

	dir := t.TempDir()
	if got := scratchParent(dir); got != dir {
		t.Fatalf("scratchParent(%q) = %q, want %q", dir, got, dir)
	}
}

func TestEnsureScratchParent(t *testing.T) {
	parent, err := ensureScratchParent("")
	if err != nil || parent != os.TempDir() {
		t.Fatalf("ensureScratchParent(\"\") = %q, %v; want %q, nil", parent, err, os.TempDir())
	}

	missing := filepath.Join(t.TempDir(), "nested", "scratch")
	parent, err = ensureScratchParent(missing)
	if err != nil || parent != missing {
		t.Fatalf("ensureScratchParent(%q) = %q, %v; want %q, nil", missing, parent, err, missing)
	}
	if info, statErr := os.Stat(missing); statErr != nil || !info.IsDir() || info.Mode().Perm() != hostDirPerm(0o700) {
		t.Fatalf("scratch parent info=%v err=%v, want 0700 directory", info, statErr)
	}

	blocked := filepath.Join(t.TempDir(), "blocked")
	if writeErr := os.WriteFile(blocked, nil, 0o600); writeErr != nil {
		t.Fatalf("write blocking scratch file: %v", writeErr)
	}
	if _, err := ensureScratchParent(blocked); err == nil {
		t.Fatal("ensureScratchParent accepted a regular file as scratch parent")
	}
}
