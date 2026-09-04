//go:build windows

package codex

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fake native CLIs a test launches, in this host's own script form. Windows
// resolves an executable by PATHEXT and honours no interpreter line, so each of
// these is a batch twin of the posix script beside it, behaving the same way.
const (
	fakeCLIVersionOnly = "@echo off\r\necho codex-cli 0.144.1\r\n"

	fakeCLIExitZero = "@echo off\r\nexit /b 0\r\n"

	fakeCLIAccountLog = "@echo off\r\n" +
		"if \"%~1\"==\"--version\" (\r\n" +
		"  echo codex-cli 0.144.1\r\n" +
		"  exit /b 0\r\n" +
		")\r\n" +
		"echo %* > \"%ACCOUNT_LOG%\"\r\n"

	fakeCLIAppServer = "@echo off\r\n" +
		"if \"%~1\"==\"--version\" (\r\n" +
		"  echo codex-cli 0.144.1\r\n" +
		"  exit /b 0\r\n" +
		")\r\n" +
		"set /p line=\r\n" +
		"echo {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\r\n" +
		"findstr \"^\" >nul\r\n"
)

// fakeCLIName is the filename a fake native CLI has to carry to be launchable
// on this host: an extension PATHEXT names, because Windows resolves an
// executable by its extension and never by its first line.
func fakeCLIName(base string) string { return base + ".cmd" }

// anExistingExecutable is a path that already resolves to a launchable file, for
// the gates a test drives before anything is launched.
func anExistingExecutable(t *testing.T) string {
	t.Helper()

	return writeFakeCLI(t, t.TempDir(), "true", fakeCLIExitZero)
}

// requireAccountLoginOutcome states what a login leg does on this host. Windows
// can neutralise no browser launch — CreateProcess resolves the launchers out of
// the system directory ahead of PATH — so the leg fails closed before anything
// is run, and the native CLI is never reached.
func requireAccountLoginOutcome(t *testing.T, err error, logPath string) {
	t.Helper()

	require.ErrorIs(t, err, errBrowserShimUnsupported)

	_, statErr := os.Stat(logPath)
	require.ErrorIs(t, statErr, os.ErrNotExist, "a refused login still reached the native CLI")
}
