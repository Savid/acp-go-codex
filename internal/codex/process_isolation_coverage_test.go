//go:build unix

package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestProcessIsolationBaseEnvironmentValidation(t *testing.T) {
	_, err := buildProcessEnvironment(&ProcessIsolation{UID: 1, GID: 2})
	require.ErrorContains(t, err, "base environment is required")

	_, err = buildProcessEnvironment(&ProcessIsolation{
		UID: 1, GID: 2,
		BaseEnvironment:     map[string]string{"BAD=KEY": "value"},
		StandaloneOwnerID:   "test-owner",
		StandaloneStateRoot: "/var/lib/acp-go-test",
	})
	require.ErrorContains(t, err, "validate process isolation base environment")

	_, err = buildProcessEnvironment(&ProcessIsolation{
		UID: 1, GID: 2,
		BaseEnvironment:     map[string]string{"PATH": "/usr/bin"},
		StandaloneOwnerID:   "test-owner",
		StandaloneStateRoot: "/var/lib/acp-go-test",
	}, map[string]string{"": "anonymous"})
	require.ErrorContains(t, err, "invalid environment entry")
}

func TestProcessIdentityAdoptedDispositionValidation(t *testing.T) {
	for name, test := range map[string]struct {
		isolation ProcessIsolation
		wantError string
	}{
		"adopted alone": {
			isolation: ProcessIsolation{identityAuthorityAdopted: true},
		},
		"adopted with lock": {
			isolation: ProcessIsolation{identityAuthorityAdopted: true, IdentityLock: testProcessIdentityCapability{}},
			wantError: "duplicable capabilities",
		},
		"adopted with domain": {
			isolation: ProcessIsolation{identityAuthorityAdopted: true, AuthorityDomain: testProcessIdentityCapability{}},
			wantError: "duplicable capabilities",
		},
		"adopted with standalone owner": {
			isolation: ProcessIsolation{identityAuthorityAdopted: true, StandaloneOwnerID: "deployment-1"},
			wantError: "forbids standalone owner fields",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateStandaloneIdentityDisposition(&test.isolation)
			if test.wantError == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestProcessSearchPathAcceptsUnsetValue(t *testing.T) {
	require.NoError(t, validateProcessSearchPath(""))
}

func TestResolveProcessExecutableRejectsUnusablePaths(t *testing.T) {
	root := t.TempDir()

	_, err := resolveProcessExecutable("  ", nil)
	require.ErrorContains(t, err, "executable path is empty")

	_, err = resolveProcessExecutable("relative/codex", nil)
	require.ErrorContains(t, err, "is not absolute")

	_, err = resolveProcessExecutable(filepath.Join(root, "missing"), nil)
	require.ErrorContains(t, err, "stat executable")

	_, err = resolveProcessExecutable(root, nil)
	require.ErrorContains(t, err, "is not executable")

	plain := filepath.Join(root, "plain")
	require.NoError(t, os.WriteFile(plain, []byte("x"), 0o600))
	_, err = resolveProcessExecutable(plain, nil)
	require.ErrorContains(t, err, "is not executable")

	_, err = resolveProcessExecutable("codex", []string{"HOME=" + root})
	require.ErrorContains(t, err, "process isolation PATH is empty")

	_, err = resolveProcessExecutable("codex", []string{"PATH=relative"})
	require.ErrorContains(t, err, "non-absolute entry")

	_, err = resolveProcessExecutable("codex", []string{"PATH=" + plain})
	require.ErrorContains(t, err, "find codex in process isolation PATH")
	require.NotErrorIs(t, err, os.ErrNotExist)
}

func TestResolveProcessExecutableFindsSearchPathEntry(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700))

	resolved, err := resolveProcessExecutable("codex", []string{"PATH=" + filepath.Join(root, "missing") + string(os.PathListSeparator) + root})
	require.NoError(t, err)
	require.Equal(t, executable, resolved)
}

func TestApplyProcessCredentialRequiresIsolation(t *testing.T) {
	require.ErrorContains(t, applyProcessCredential(nil, nil), "process isolation is required")
}

func TestCloseInheritedOnExecFailureModes(t *testing.T) {
	require.ErrorContains(t, closeInheritedOnExec(nil), "inherited config descriptor is unavailable")

	file, err := os.CreateTemp(t.TempDir(), "inherited")
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	require.NoError(t, closeInheritedOnExec(file))

	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	require.NoError(t, err)
	require.NotZero(t, flags&unix.FD_CLOEXEC)

	original := inheritedDescriptorFcntl
	t.Cleanup(func() { inheritedDescriptorFcntl = original })

	inheritedDescriptorFcntl = func(uintptr, int, int) (int, error) { return 0, errors.New("fcntl failed") }
	require.ErrorContains(t, closeInheritedOnExec(file), "read inherited descriptor flags")

	calls := 0
	inheritedDescriptorFcntl = func(fd uintptr, command int, argument int) (int, error) {
		calls++
		if calls == 1 {
			return original(fd, command, argument)
		}

		return 0, errors.New("fcntl failed")
	}
	require.ErrorContains(t, closeInheritedOnExec(file), "protect inherited descriptor from native exec")
}
