//go:build windows

package codexacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConsentedHomeCannotBeReplacedWhileConsentIsHeld pins the same property the
// posix proof pins by repointing the consented path: consent is held over a
// directory rather than over a name. Windows reaches it from the other side.
// The gate opens the directory and keeps it open, and Windows refuses to move a
// directory anything holds open, so the swap the posix proof stages cannot be
// staged here at all. Releasing the gate is what lets the name move again, which
// is what makes the handle — not the spelling — the thing consent is granted
// over.
func TestConsentedHomeCannotBeReplacedWhileConsentIsHeld(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	moved := filepath.Join(root, "moved")

	require.NoError(t, os.Mkdir(home, 0o700))

	granted := consentDirectHome(Options{Home: home, ProviderAuthDirectHome: home})

	t.Cleanup(granted.close)

	require.True(t, granted.unchanged(), "the gate refused the directory it opened")
	require.Error(t, os.Rename(home, moved), "the consented directory moved while consent was held")
	require.True(t, granted.unchanged(), "a refused move disturbed the consented home")
	require.False(t, (consentedHome{}).unchanged(), "an ungranted gate read as consented")

	granted.close()

	require.NoError(t, os.Rename(home, moved), "releasing consent left the directory pinned")
	require.False(t, granted.unchanged(), "a released directory still read as the consented home")
}

// TestDisconnectsConsentedHomeCannotBeRepointed pins the account-level logout to
// the directory consent was granted over, which is what the posix proof pins by
// repointing that path and watching the disconnect refuse. Here the repoint is
// what is refused: the broker holds the consented directory open for the life of
// the agent, so no logout can be aimed at an account nobody authorized by moving
// the home out from under it.
func TestDisconnectsConsentedHomeCannotBeRepointed(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")

	require.NoError(t, os.Mkdir(home, 0o700))

	fixture := newProviderAuthFixture(t, WithHome(home), WithProviderAuthDirectHome(home))
	fixture.authenticatedFlow(t)

	require.Error(t, os.Rename(home, filepath.Join(root, "moved")),
		"the consented home moved while the broker held it")
	require.Zero(t, fixture.client.logoutCalls, "a refused move still drove an account-level logout")
}
