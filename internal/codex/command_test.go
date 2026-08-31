package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
)

func TestAppServerArgs(t *testing.T) {
	args := appServerArgs(Options{
		Config:    map[string]any{"z": true, "a": "value"},
		ExtraArgs: []string{"--extra"},
	})
	require.Equal(t, []string{
		"app-server", "--listen", "stdio://", "--disable", "plugins",
		"-c", `a="value"`, "-c", "z=true", "--extra",
	}, args)
}

func TestCommandEnvironmentAndManagedSelector(t *testing.T) {
	environment, err := buildMergedEnv(Options{
		CodexHome:           "/managed/home",
		ImplicitEnvironment: map[string]string{"PATH": "/bin", "HOME": "/ambient", "SECRET": "remove"},
		Env:                 map[string]string{"KEEP": "yes", "HOME": "/ignored"},
	})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"CODEX_HOME": "/managed/home", "HOME": "/ambient", "KEEP": "yes", "PATH": "/bin", "SECRET": "remove",
	}, environmentMap(environment))

	_, err = buildProcessEnvironmentFrom(map[string]string{"BAD=KEY": "value"})
	require.Error(t, err)
	_, err = buildProcessEnvironmentFrom(map[string]string{"KEY": "bad\x00value"})
	require.Error(t, err)

	for _, selector := range []string{
		"node_modules/@openai/codex/bin/codex.js",
		"relative\\node_modules\\@openai\\codex",
		"/tmp/node_modules/codex",
	} {
		require.Error(t, validateManagedSelector(selector), selector)
	}
	require.NoError(t, validateManagedSelector("host-pinned-codex"))
}

func TestValidateCodexVersion(t *testing.T) {
	version, err := validateCodexVersionOutput("codex-cli 0.144.1")
	require.NoError(t, err)
	require.Equal(t, "0.144.1", version)
	version, err = validateCodexVersionOutput("codex 1.2.3-beta+build")
	require.NoError(t, err)
	require.Equal(t, "1.2.3", version)
	for _, output := range []string{"", "unknown", "codex 0.144.0"} {
		_, err := validateCodexVersionOutput(output)
		require.Error(t, err)
	}
	require.Equal(t, -1, compareSemver("1.2.2", "1.2.3"))
	require.Equal(t, 0, compareSemver("1.2.3", "1.2.3"))
	require.Equal(t, 1, compareSemver("1.3.0", "1.2.3"))
}

func TestProcessCloserNil(t *testing.T) {
	var nativeProcess *process
	require.NoError(t, nativeProcess.Close())
	require.NoError(t, (&process{}).Close())
}

func TestOrdinaryRuntimeStartFailureReleasesHomeLock(t *testing.T) {
	scratch := t.TempDir()
	home := t.TempDir()
	request := NativeRequest{Executable: filepath.Join(t.TempDir(), "missing")}
	_, err := startOrdinaryNative(t.Context(), request, Options{
		CodexHome: home, WritableHome: home, ScratchParent: scratch,
	})
	require.Error(t, err)

	lockRoot, lockErr := HomeLockRoot(scratch, home)
	require.NoError(t, lockErr)
	lock, lockErr := homelock.Acquire(lockRoot)
	require.NoError(t, lockErr)
	require.NoError(t, lock.Release())
}

func TestOrdinaryNativeWaitAndRevoke(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}

	native, err := startOrdinaryNative(t.Context(), NativeRequest{
		Executable: "/bin/sh", Arguments: []string{"-c", "exit 7"}, Environment: os.Environ(),
	}, Options{skipHomeLock: true})
	require.NoError(t, err)
	result, err := native.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)

	native, err = startOrdinaryNative(t.Context(), NativeRequest{
		Executable: "/bin/sh", Arguments: []string{"-c", "sleep 30"}, Environment: os.Environ(),
	}, Options{skipHomeLock: true})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, native.Revoke(ctx))
	result, err = native.Wait(ctx)
	require.NoError(t, err)
	require.True(t, result.Revoked)
}
