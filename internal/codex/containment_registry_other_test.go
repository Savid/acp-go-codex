//go:build !darwin

package codex

import (
	"io"
	"testing"
)

func TestDarwinContainmentOperationsRejectUnsupportedPlatform(t *testing.T) {
	if generation, err := NewDarwinGenerationRecord("parent", "root", "runtime"); err == nil || generation != nil {
		t.Fatalf("generation=%v err=%v", generation, err)
	}
	if err := DiagnoseDarwinContainment("parent", io.Discard); err == nil {
		t.Fatal("diagnose succeeded off Darwin")
	}
	if err := CleanupDarwinContainment("parent", "runtime", true, io.Discard); err == nil {
		t.Fatal("cleanup succeeded off Darwin")
	}
}
