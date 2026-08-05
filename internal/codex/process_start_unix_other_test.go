//go:build unix && !linux

package codex

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStartProcessReportsSpawnFailure(t *testing.T) {
	waiter, err := startProcess(exec.Command(filepath.Join(t.TempDir(), "missing")))
	require.Nil(t, waiter)
	require.Error(t, err)
}

func TestConfigureProcessPreservesExistingCredential(t *testing.T) {
	credential := &syscall.Credential{Uid: 1234, Gid: 5678}
	cmd := exec.Command("/usr/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}

	configureProcess(cmd)

	require.True(t, cmd.SysProcAttr.Setpgid)
	require.Same(t, credential, cmd.SysProcAttr.Credential)
}
