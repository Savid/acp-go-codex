//go:build !windows

package codexacp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConsentedHomeRefusesADirectoryReplacedAfterConsent pins what consent is
// held over. A name is not a directory: anything at this agent's uid can point
// the consented path somewhere else, and the account legs drive native calls
// that resolve the path again when they run.
func TestConsentedHomeRefusesADirectoryReplacedAfterConsent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	other := filepath.Join(root, "other")

	for _, dir := range []string{home, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	granted := consentDirectHome(Options{Home: home, ProviderAuthDirectHome: home})

	t.Cleanup(granted.close)

	if !granted.unchanged() {
		t.Fatal("the gate refused the directory it opened")
	}

	if err := os.Rename(home, filepath.Join(root, "moved")); err != nil {
		t.Fatalf("move the consented home: %v", err)
	}

	if granted.unchanged() {
		t.Fatal("a path reaching nothing still read as the consented home")
	}

	if err := os.Symlink(other, home); err != nil {
		t.Fatalf("point the consented path elsewhere: %v", err)
	}

	if granted.unchanged() {
		t.Fatal("a repointed path still read as the consented home")
	}

	if (consentedHome{}).unchanged() {
		t.Fatal("an ungranted gate read as consented")
	}

	granted.close()

	if granted.unchanged() {
		t.Fatal("a released directory still read as the consented home")
	}
}

// TestDisconnectRefusesAHomeReplacedAfterConsent pins the account-level logout
// to the directory consent was granted over. The app-server resolves CODEX_HOME
// when the call runs, so a path repointed since then would log out an account
// nobody authorized this agent to touch.
func TestDisconnectRefusesAHomeReplacedAfterConsent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	other := filepath.Join(root, "other")

	for _, dir := range []string{home, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	fixture := newProviderAuthFixture(t, WithHome(home), WithProviderAuthDirectHome(home))
	fixture.authenticatedFlow(t)

	if err := os.Rename(home, filepath.Join(root, "moved")); err != nil {
		t.Fatalf("move the consented home: %v", err)
	}

	if err := os.Symlink(other, home); err != nil {
		t.Fatalf("point the consented path elsewhere: %v", err)
	}

	_, err := fixture.call(t, AuthDisconnectMethod, map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      "connection-1",
		"bindingGeneration": 1,
	})
	requireAuthCause(t, err, authCausePolicy)

	if fixture.client.logoutCalls != 0 {
		t.Fatal("a repointed home still drove an account-level logout")
	}
}
