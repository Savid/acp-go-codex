//go:build darwin

package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeVersionUsesIndependentDarwinDiscoveryGeneration(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(parent, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'codex-cli 0.144.1\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var roots []string
	for attempt := 0; attempt < 2; attempt++ {
		scratch, mkdirErr := os.MkdirTemp(parent, "acp-go-codex-runtime-version-")
		if mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		roots = append(roots, scratch)
		version, probeErr := ProbeVersion(ctx, VersionProbeOptions{
			CLIPath: script, WritableHome: home, Scratch: scratch, ScratchParent: parent, DarwinBestEffort: true,
		})
		if probeErr != nil {
			t.Fatal(probeErr)
		}
		if version != minCodexVersion {
			t.Fatalf("version = %q, want %q", version, minCodexVersion)
		}
	}

	_, records, err := readDarwinRecords(parent, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].LifecycleKind != lifecycleDiscovery || records[0].GenerationRoot == records[1].GenerationRoot || records[0].State != darwinStateGroupAbsent || records[1].State != darwinStateGroupAbsent {
		t.Fatalf("discovery records = %#v", records)
	}
	for _, root := range roots {
		if records[0].GenerationRoot != root && records[1].GenerationRoot != root {
			t.Fatalf("fresh discovery root %q missing from %#v", root, records)
		}
	}
}
