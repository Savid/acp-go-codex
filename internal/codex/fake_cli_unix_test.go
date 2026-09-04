//go:build !windows

package codex

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fake native CLIs a test launches, in this host's own script form. Each
// body has a Windows twin that behaves the same way, so a test states what the
// native side does rather than which shell says it.
const (
	fakeCLIVersionOnly = "#!/bin/sh\necho codex-cli 0.144.1\n"

	fakeCLIExitZero = "#!/bin/sh\nexit 0\n"

	fakeCLIAccountLog = `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
printf '%s' "$*" > "$ACCOUNT_LOG"
`

	fakeCLIAppServer = `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
read line || exit 0
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read line || true
while read line; do :; done
`
)

// fakeCLIName is the filename a fake native CLI has to carry to be launchable
// on this host.
func fakeCLIName(base string) string { return base }

// anExistingExecutable is a path that already resolves to a launchable file, for
// the gates a test drives before anything is launched.
func anExistingExecutable(t *testing.T) string {
	t.Helper()

	return writeFakeCLI(t, t.TempDir(), "true", fakeCLIExitZero)
}

// requireAccountLoginOutcome states what a login leg does on this host. A posix
// host neutralises the browser launch with a shim on PATH and then runs the
// native CLI, so the login reaches the CLI with the arguments it was given.
func requireAccountLoginOutcome(t *testing.T, err error, logPath string) {
	t.Helper()

	require.NoError(t, err)

	raw, readErr := os.ReadFile(logPath)
	require.NoError(t, readErr)
	require.Equal(t, "login --device-auth", strings.TrimSpace(string(raw)))
}
