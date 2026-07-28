//go:build integration

package codexacp

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const (
	envRunIntegration = "ACP_GO_CODEX_RUN_INTEGRATION"
	envRunKeystore    = "ACP_GO_CODEX_RUN_KEYSTORE"

	// envSessionBus is the address of the session bus a Secret Service answers
	// on. Its presence is the configuration itself rather than a label for one:
	// the keystore-present and keystore-absent Linux halves are the same image
	// run with and without it.
	envSessionBus = "DBUS_SESSION_BUS_ADDRESS"
)

// requireKeystoreTier answers to both gates. Below them the tier is not
// selected and the matrix does not run; above them it fails rather than skips,
// because a silently green residence suite is worse than a red one.
func requireKeystoreTier(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 and %s=1 to run the credential-residence tier", envRunIntegration, envRunKeystore)
	}
}

// keystoreServicePresent reports whether a keystore answers here at all. macOS
// has no absent half — the Keychain is always there, and that is the third of
// the matrix no container can carry — while on Linux the reachable session bus
// is what makes a Secret Service present or absent.
func keystoreServicePresent() bool {
	return runtime.GOOS == "darwin" || os.Getenv(envSessionBus) != ""
}

// TestKeystoreResidenceMatrix proves which credential store the read path wins
// from under each configured mode. Both stores hold a distinguishable canary
// wherever both exist, so a mode that read the wrong one is visible rather than
// merely unproven. It runs in all three configurations — keystore-absent Linux,
// keystore-present Linux, and macOS — because which store is authoritative is a
// behavioral fork in the harness rather than an environment caveat.
func TestKeystoreResidenceMatrix(t *testing.T) {
	requireKeystoreTier(t)

	keystorePresent := keystoreServicePresent()

	cases := map[string]struct {
		mode string
		// wins is the access token the read path must return. An empty value
		// means the read must fail: either no store has determinate authority,
		// or the configured store is not there to answer.
		wins string
	}{
		"file":      {mode: authStoreModeFile, wins: "file-canary-access"},
		"keyring":   {mode: authStoreModeKeyring, wins: keystoreCanaryWinner(keystorePresent)},
		"auto":      {mode: authStoreModeAuto},
		"ephemeral": {mode: authStoreModeEphemeral},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t, WithCodexConfigOverrides(map[string]any{
				authStoreConfigKey: testCase.mode,
			}))

			seedStoredLogin(t, fixture.home, storedLoginWithAccess("file-canary-access"))

			if keystorePresent {
				seedKeystoreCanary(t, fixture.home, storedLoginWithAccess("keystore-canary-access"))
			}

			stored, err := fixture.broker.readStoredCredential()
			if testCase.wins == "" {
				if err == nil {
					t.Fatalf("%s harvested a store that answers for nothing", testCase.mode)
				}

				return
			}

			if err != nil {
				t.Fatalf("%s read: %v", testCase.mode, err)
			}

			if stored.AccessToken != testCase.wins {
				t.Fatalf("%s read %q, want %q", testCase.mode, stored.AccessToken, testCase.wins)
			}
		})
	}
}

// keystoreCanaryWinner names what the keyring mode must return. Keyring mode
// has no file fallback, so with no service behind it the read fails rather than
// quietly answering from the readable file sitting beside it.
func keystoreCanaryWinner(keystorePresent bool) string {
	if keystorePresent {
		return "keystore-canary-access"
	}

	return ""
}

func storedLoginWithAccess(access string) string {
	return `{"auth_mode":"chatgpt","tokens":{"access_token":"` + access +
		`","refresh_token":"canary-refresh","account_id":"canary-account"}}`
}

// seedKeystoreCanary plants canary material through the platform tool rather
// than through the read path, so the assertion is not a round trip of one
// library against itself. The account is derived from a fresh temporary home,
// so the item this writes is addressable by nothing else and is removed again
// when the test ends.
func seedKeystoreCanary(t *testing.T, home string, contents string) {
	t.Helper()

	account := authKeystoreAccount(home)

	if runtime.GOOS == "darwin" {
		seedDarwinKeychainCanary(t, account, contents)

		return
	}

	command := exec.Command("secret-tool", "store", "--label=codex-canary",
		"service", authKeystoreService,
		"username", account)
	command.Stdin = strings.NewReader(contents)

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seed keystore canary: %v: %s", err, output)
	}
}

// seedDarwinKeychainCanary writes into the operator's real login Keychain,
// which is the only Keychain the read path can reach: a scratch HOME removes it
// from the search list, and a Keychain write under one blocks on an interactive
// modal instead. The item is unlocked for every reader so the read path is
// never gated on a prompt nobody can answer.
func seedDarwinKeychainCanary(t *testing.T, account string, contents string) {
	t.Helper()

	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password",
			"-s", authKeystoreService, "-a", account).Run()
	})

	command := exec.Command("security", "add-generic-password", "-U", "-A",
		"-s", authKeystoreService, "-a", account, "-w", contents)

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seed keychain canary: %v: %s", err, output)
	}
}
