//go:build darwin

package codex

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHomeLockRootRefusesAnUnprotectableRoot(t *testing.T) {
	parent := t.TempDir()
	home := t.TempDir()

	root, err := HomeLockRoot(parent, home)
	require.NoError(t, err)

	// An immutable root cannot be reduced to owner-only access, so the lock
	// root must be refused rather than reused at whatever mode it carries.
	require.NoError(t, exec.Command("/usr/bin/chflags", "uchg", root).Run())
	t.Cleanup(func() { _ = exec.Command("/usr/bin/chflags", "nouchg", root).Run() })

	_, err = HomeLockRoot(parent, home)
	require.ErrorContains(t, err, "protect codex trusted home-lock root")
}
