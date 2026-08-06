//go:build linux

package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCodexAccountNativePathsExcludeSupervisorScratch(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	parent, err := os.MkdirTemp("/tmp", "acp-go-codex-account-ownership-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if chmodErr := os.Chmod(parent, 0o711); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	home := filepath.Join(parent, "home")
	if mkdirErr := os.Mkdir(home, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	control, err := HomeLockRoot(parent, home)
	if err != nil {
		t.Fatal(err)
	}
	proof := filepath.Join(control, "proof")
	if writeErr := os.WriteFile(proof, []byte("trusted"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	shim, err := newBrowserShim(parent)
	if err != nil {
		t.Fatal(err)
	}
	isolation := &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
	if err := handoffGeneratedNativeTree(home, isolation); err != nil {
		t.Fatal(err)
	}
	if err := shim.handoff(isolation); err != nil {
		t.Fatal(err)
	}
	if err := validateNativeOwnedDirectory(home, isolation); err != nil {
		t.Fatal(err)
	}

	launcher := filepath.Join(shim.dir, browserLauncherNames[0])
	cmd := exec.Command("/bin/sh", "-c", `: > "$1/native-ok" && "$2" https://example.invalid && ! cat "$3" >/dev/null 2>&1 && ! rm -f "$3"`, "sh", home, launcher, proof)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dropped identity proof: %v: %s", err, output)
	}
	if _, err := os.Stat(proof); err != nil {
		t.Fatalf("trusted supervisor proof changed: %v", err)
	}
}
