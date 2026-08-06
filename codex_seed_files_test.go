package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestWriteSeedFilesWritesUnderHome(t *testing.T) {
	home := t.TempDir()
	config := "[model_providers.litellm]\nbase_url = \"https://litellm.example/v1\"\n"

	err := writeSeedFiles(home, map[string]string{
		"config.toml":            config,
		"prompts/system.md":      "be terse\n",
		"nested/deep/values.txt": "ok\n",
	})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, config, string(got))

	nested, err := os.ReadFile(filepath.Join(home, "prompts", "system.md"))
	require.NoError(t, err)
	require.Equal(t, "be terse\n", string(nested))

	fileInfo, err := os.Stat(filepath.Join(home, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())

	dirInfo, err := os.Stat(filepath.Join(home, "prompts"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestWriteSeedFilesEmptyIsNoop(t *testing.T) {
	home := t.TempDir()

	require.NoError(t, writeSeedFiles(home, nil))

	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestWriteSeedFilesRequiresExplicitHome(t *testing.T) {
	err := writeSeedFiles("", map[string]string{"config.toml": "model = \"gpt-5.5\"\n"})
	requireUnsupported(t, err)

	err = writeSeedFiles("   ", map[string]string{"config.toml": "model = \"gpt-5.5\"\n"})
	requireUnsupported(t, err)
}

func TestWriteSeedFilesRejectsUnsafePaths(t *testing.T) {
	home := t.TempDir()

	cases := map[string]string{
		"absolute":        "/etc/passwd",
		"parent escape":   "../outside.toml",
		"nested parent":   "prompts/../../outside.toml",
		"empty key":       "",
		"whitespace key":  "   ",
		"leading slash":   "/config.toml",
		"trailing parent": "config/..",
	}

	for name, relPath := range cases {
		t.Run(name, func(t *testing.T) {
			err := writeSeedFiles(home, map[string]string{relPath: "data"})
			requireUnsupported(t, err)

			entries, readErr := os.ReadDir(home)
			require.NoError(t, readErr)
			require.Empty(t, entries, "seed files must not be written when a path is rejected")
		})
	}
}

func TestNewClientSeedsUnderHome(t *testing.T) {
	home := t.TempDir()
	contents := "[model_providers.litellm]\nbase_url = \"https://litellm.example/v1\"\n"

	agent := NewAgent(
		WithHome(home),
		WithSeedFiles(map[string]string{"config.toml": contents}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return newSpyCodexClient(), nil
		}),
	)

	client, err := agent.launchRuntimeClient(context.Background(), 1, "", minSupportedCodexVersion)
	require.NoError(t, err)
	require.NotNil(t, client)

	got, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, contents, string(got))
}

func TestNewClientSeedFilesRequireHome(t *testing.T) {
	agent := NewAgent(
		WithSeedFiles(map[string]string{"config.toml": "model = \"gpt-5.5\"\n"}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			t.Fatal("client factory called despite unresolved CODEX_HOME")

			return newSpyCodexClient(), nil
		}),
	)

	_, err := agent.launchRuntimeClient(context.Background(), 1, "", minSupportedCodexVersion)
	requireUnsupported(t, err)
}

func TestWriteSeedFilesRecordsManifest(t *testing.T) {
	home := t.TempDir()

	require.NoError(t, writeSeedFiles(home, map[string]string{
		"config.toml":       "model = \"gpt-5.5\"\n",
		"prompts/system.md": "be terse\n",
	}))

	require.ElementsMatch(t, []string{"config.toml", "prompts/system.md"}, readSeedManifest(t, home))
}

func TestWriteSeedFilesIdempotentSkipsBackup(t *testing.T) {
	home := t.TempDir()
	contents := "model = \"gpt-5.5\"\n"

	require.NoError(t, writeSeedFiles(home, map[string]string{"config.toml": contents}))
	require.NoError(t, writeSeedFiles(home, map[string]string{"config.toml": contents}))

	_, err := os.Stat(filepath.Join(home, "config.toml"+seedBackupSuffix))
	require.True(t, errors.Is(err, os.ErrNotExist), "identical re-seed must not create a backup")
}

func TestWriteSeedFilesBacksUpChangedManagedFile(t *testing.T) {
	home := t.TempDir()

	require.NoError(t, writeSeedFiles(home, map[string]string{"config.toml": "old\n"}))
	require.NoError(t, writeSeedFiles(home, map[string]string{"config.toml": "new\n"}))

	current, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, "new\n", string(current))

	backup, err := os.ReadFile(filepath.Join(home, "config.toml"+seedBackupSuffix))
	require.NoError(t, err)
	require.Equal(t, "old\n", string(backup))
}

func TestWriteSeedFilesFailsClosedOnUnmanagedFile(t *testing.T) {
	home := t.TempDir()
	operator := "operator-owned\n"
	target := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(target, []byte(operator), 0o600))

	err := writeSeedFiles(home, map[string]string{"config.toml": "seeded\n"})
	requireUnsupported(t, err)

	current, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, operator, string(current), "unmanaged operator file must be left untouched")

	_, statErr := os.Stat(filepath.Join(home, seedManifestName))
	require.True(t, errors.Is(statErr, os.ErrNotExist), "fail-closed seed must not write a manifest")
}

// TestWriteSeedBytesRefusesAnUncreatableParent proves the seed writer reports
// a parent directory it cannot create instead of writing anywhere else. The
// entry point cannot reach this guard: writeSeedFiles stats every target
// first, and a parent that is secretly a file already fails that stat, so the
// guard is driven directly with the same shape of path.
func TestWriteSeedBytesRefusesAnUncreatableParent(t *testing.T) {
	home := t.TempDir()
	occupied := filepath.Join(home, "occupied")
	require.NoError(t, os.WriteFile(occupied, []byte("x"), 0o600))

	err := writeSeedBytes(filepath.Join(occupied, "nested", "seed.toml"), []byte("seeded\n"))
	require.ErrorContains(t, err, "create seed file directory")

	current, readErr := os.ReadFile(occupied)
	require.NoError(t, readErr)
	require.Equal(t, "x", string(current), "the occupying file must be left untouched")
}

func TestWriteSeedFilesManifestSurvivesPasses(t *testing.T) {
	home := t.TempDir()

	require.NoError(t, writeSeedFiles(home, map[string]string{"config.toml": "one\n"}))
	require.Equal(t, []string{"config.toml"}, readSeedManifest(t, home))

	// The second pass must see config.toml as managed (not fail closed) and add
	// a new managed entry.
	require.NoError(t, writeSeedFiles(home, map[string]string{
		"config.toml":       "two\n",
		"prompts/system.md": "terse\n",
	}))
	require.ElementsMatch(t, []string{"config.toml", "prompts/system.md"}, readSeedManifest(t, home))
}

func TestWriteSeedFilesRecreatesStaleManifestEntry(t *testing.T) {
	home := t.TempDir()
	writeSeedManifest(t, home, []string{"config.toml"})

	require.NoError(t, writeSeedFiles(home, map[string]string{"config.toml": "recreated\n"}))

	current, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, "recreated\n", string(current))
	require.Equal(t, []string{"config.toml"}, readSeedManifest(t, home))
}

func TestWriteSeedFilesRejectsCorruptManifest(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, seedManifestName), []byte("{not json"), 0o600))

	err := writeSeedFiles(home, map[string]string{"config.toml": "x\n"})
	require.Error(t, err)
	require.NotContains(t, seedFilesInHome(t, home), "config.toml")
}

func TestWriteSeedFilesManifestReadError(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, seedManifestName), 0o700))

	err := writeSeedFiles(home, map[string]string{"config.toml": "x\n"})
	require.Error(t, err)
}

func TestWriteSeedFilesStatError(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "blocker"), []byte("file"), 0o600))

	// Statting <home>/blocker/x.toml fails with ENOTDIR, which is neither a
	// missing file nor a manageable target.
	err := writeSeedFiles(home, map[string]string{"blocker/x.toml": "x\n"})
	require.Error(t, err)
}

func TestWriteSeedFilesReadManagedError(t *testing.T) {
	home := t.TempDir()
	writeSeedManifest(t, home, []string{"config.toml"})
	require.NoError(t, os.MkdirAll(filepath.Join(home, "config.toml"), 0o700))

	err := writeSeedFiles(home, map[string]string{"config.toml": "x\n"})
	require.Error(t, err)
}

// The three cases below each deny one write. None of them denies it with
// directory permissions: a privileged identity carries CAP_DAC_OVERRIDE and
// walks through a 0500 home, which left every one of them proving nothing when
// the suite ran as root. Each obstruction is structural instead, so the write
// fails for every identity.

func TestWriteSeedFilesBackupWriteError(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, writeSeedFiles(home, map[string]string{"config.toml": "old\n"}))

	// A directory where the .seed.bak sidecar belongs lets the managed compare
	// read succeed and fails the sidecar write.
	require.NoError(t, os.Mkdir(filepath.Join(home, "config.toml"+seedBackupSuffix), 0o700))

	err := writeSeedFiles(home, map[string]string{"config.toml": "new\n"})
	require.Error(t, err)
}

func TestWriteSeedFilesMkdirError(t *testing.T) {
	home := t.TempDir()

	// A file where the seed subdirectory belongs fails the parent creation.
	require.NoError(t, os.WriteFile(filepath.Join(home, "sub"), []byte("x"), 0o600))

	err := writeSeedFiles(home, map[string]string{"sub/config.toml": "x\n"})
	require.Error(t, err)
}

func TestWriteSeedFilesWriteError(t *testing.T) {
	home := t.TempDir()

	// A dangling symlink still reads as absent, so the target is written
	// directly — into a directory that does not exist.
	require.NoError(t, os.Symlink(filepath.Join(home, "missing", "config.toml"), filepath.Join(home, "config.toml")))

	err := writeSeedFiles(home, map[string]string{"config.toml": "x\n"})
	require.Error(t, err)
}

func readSeedManifest(t *testing.T, home string) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(home, seedManifestName))
	require.NoError(t, err)

	var entries []string
	require.NoError(t, json.Unmarshal(data, &entries))

	return entries
}

func writeSeedManifest(t *testing.T, home string, entries []string) {
	t.Helper()

	data, err := json.Marshal(entries)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, seedManifestName), data, 0o600))
}

func seedFilesInHome(t *testing.T, home string) []string {
	t.Helper()

	entries, err := os.ReadDir(home)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

func requireUnsupported(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)

	var reqErr *acp.RequestError
	require.True(t, errors.As(err, &reqErr), "error %v is not an acp.RequestError", err)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok, "error data is not a map: %#v", reqErr.Data)
	require.Equal(t, "unsupported", data[jsonFieldError])
}
