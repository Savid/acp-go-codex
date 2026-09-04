package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountCommandArgs(t *testing.T) {
	require.Equal(t, []string{"login"}, requireAccountArgs(t, "login", false))
	require.Equal(t, []string{"login", "--device-auth"}, requireAccountArgs(t, "login", true))
	require.Equal(t, []string{"logout"}, requireAccountArgs(t, "logout", true))
	_, err := accountCommandArgs("invalid", false)
	require.Error(t, err)
}

func requireAccountArgs(t *testing.T, mode string, deviceAuth bool) []string {
	t.Helper()
	args, err := accountCommandArgs(mode, deviceAuth)
	require.NoError(t, err)

	return args
}

func TestRunAccountCommandFailureBranches(t *testing.T) {
	originalScratch := accountScratchParent
	originalProbe := accountProbeVersion
	t.Cleanup(func() {
		accountScratchParent = originalScratch
		accountProbeVersion = originalProbe
	})

	existing := anExistingExecutable(t)

	require.Error(t, RunAccountCommand(t.Context(), AccountCommandOptions{Mode: "invalid"}))
	require.ErrorContains(t, RunAccountCommand(t.Context(), AccountCommandOptions{Mode: accountCommandLogout}), "writable home")

	accountScratchParent = nil
	require.ErrorContains(t, RunAccountCommand(t.Context(), AccountCommandOptions{
		CLIPath: existing, CodexHome: t.TempDir(), Mode: accountCommandLogout,
	}), "scratch parent resolver")

	accountScratchParent = func(string) (string, error) { return "", errors.New("scratch unavailable") }
	require.ErrorContains(t, RunAccountCommand(t.Context(), AccountCommandOptions{
		CLIPath: existing, CodexHome: t.TempDir(), Mode: accountCommandLogout,
	}), "scratch unavailable")

	accountScratchParent = func(string) (string, error) { return t.TempDir(), nil }
	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) {
		return "", errors.New("probe failed")
	}
	require.ErrorContains(t, RunAccountCommand(t.Context(), AccountCommandOptions{
		CLIPath: existing, CodexHome: t.TempDir(), Mode: accountCommandLogout,
	}), "probe failed")
}

func TestRunAccountCommandOrdinaryBackend(t *testing.T) {
	originalScratch := accountScratchParent
	t.Cleanup(func() { accountScratchParent = originalScratch })
	parent := t.TempDir()
	accountScratchParent = func(string) (string, error) { return parent, nil }

	logPath := filepath.Join(t.TempDir(), "account.log")
	script := writeFakeCLI(t, t.TempDir(), "codex", fakeCLIAccountLog)
	t.Setenv("ACCOUNT_LOG", logPath)

	closedSignals := make(chan os.Signal)
	close(closedSignals)
	requireAccountLoginOutcome(t, RunAccountCommand(t.Context(), AccountCommandOptions{
		CLIPath: script, CodexHome: t.TempDir(), Mode: accountCommandLogin, DeviceAuth: true,
		Signals: closedSignals,
	}), logPath)
}

type blockingAccountInput struct {
	done chan struct{}
}

func (r *blockingAccountInput) Read([]byte) (int, error) {
	<-r.done

	return 0, io.EOF
}

func TestRunAccountCommandDoesNotWaitForBlockedInputAfterExit(t *testing.T) {
	originalScratch := accountScratchParent
	originalProbe := accountProbeVersion
	t.Cleanup(func() {
		accountScratchParent = originalScratch
		accountProbeVersion = originalProbe
	})
	accountScratchParent = func(string) (string, error) { return t.TempDir(), nil }
	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) { return minCodexVersion, nil }

	script := writeFakeCLI(t, t.TempDir(), "codex", fakeCLIExitZero)
	input := &blockingAccountInput{done: make(chan struct{})}
	t.Cleanup(func() { close(input.done) })

	done := make(chan error, 1)
	go func() {
		done <- RunAccountCommand(context.Background(), AccountCommandOptions{
			CLIPath: script, CodexHome: t.TempDir(), Mode: accountCommandLogout, Stdin: input,
		})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("account command waited for blocked stdin after native exit")
	}
}

func TestReaderAndWriterDefaults(t *testing.T) {
	var output bytes.Buffer
	_, err := io.Copy(&output, readerOrEmpty(nil))
	require.NoError(t, err)
	require.Empty(t, output.String())
	require.Equal(t, io.Discard, writerOrDiscard(nil))
}
