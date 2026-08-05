package codex

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func testProcessIsolation() *ProcessIsolation {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 1
	}
	if gid == 0 {
		gid = 1
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			environment[key] = value
		}
	}
	if environment["PATH"] == "" {
		environment["PATH"] = "/usr/bin:/bin"
	}

	return &ProcessIsolation{
		UID: uint32(uid), GID: uint32(gid), BaseEnvironment: environment,
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}
}

func withTestVersionIsolation(options VersionProbeOptions) VersionProbeOptions {
	if options.ProcessIsolation == nil {
		options.ProcessIsolation = testProcessIsolation()
	}
	if options.ScratchParent == "" {
		options.ScratchParent = os.TempDir()
	}
	if options.WritableHome == "" {
		options.WritableHome = os.TempDir()
	}

	return options
}

func testTraversableTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "acp-go-codex-test-")
	if err != nil {
		t.Fatalf("create traversable test directory: %v", err)
	}
	if err = os.Chmod(directory, 0o711); err != nil {
		_ = os.RemoveAll(directory)
		t.Fatalf("make test directory traversable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	return directory
}

func testNativeOwnedTempDir(t *testing.T) string {
	t.Helper()
	directory := testTraversableTempDir(t)
	isolation := testProcessIsolation()
	if err := os.Chown(directory, int(isolation.UID), int(isolation.GID)); err != nil {
		t.Fatalf("assign native-owned test directory: %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("protect native-owned test directory: %v", err)
	}

	return directory
}

func skipUnprivilegedDarwinIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
}
