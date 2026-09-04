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
	require.Equal(t, hostFilePerm(0o600), fileInfo.Mode().Perm())

	dirInfo, err := os.Stat(filepath.Join(home, "prompts"))
	require.NoError(t, err)
	require.Equal(t, hostDirPerm(0o700), dirInfo.Mode().Perm())
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

	client, err := agent.sharedRuntime(context.Background())
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

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
	agent.options.implicitEnvironment = map[string]string{}

	_, err := agent.sharedRuntime(context.Background())
	require.ErrorContains(t, err, "codex home must resolve to a canonical absolute path")
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

// TestWriteSeedFilesRejectsReservedConfigRoots proves the reserved config roots
// belong to the keyspace and not to the `-c` override surface alone. A seeded
// CODEX_HOME config.toml is loaded into the same app-server config as `-c`, and
// per-thread config only ever authors shell_environment_policy.set, so an
// inherit/exclude smuggled in through the seed door would otherwise survive
// into every thread. Every spelling the override surface normalises has to be
// caught here too, plus the table-header spellings only a file can use.
func TestWriteSeedFilesRejectsReservedConfigRoots(t *testing.T) {
	for name, content := range map[string]string{
		"dotted assignment":       "shell_environment_policy.inherit = \"all\"\n",
		"whole-root assignment":   "shell_environment_policy = { inherit = \"all\" }\n",
		"spaced dotted key":       "shell_environment_policy . inherit = \"all\"\n",
		"no space around equals":  "shell_environment_policy.inherit=\"all\"\n",
		"basic-quoted key":        "\"shell_environment_policy\".inherit = \"all\"\n",
		"literal-quoted key":      "'shell_environment_policy'.inherit = \"all\"\n",
		"escaped quoted key":      "\"shell\\u005fenvironment\\u005fpolicy\".inherit = \"all\"\n",
		"table header":            "[shell_environment_policy]\ninherit = \"all\"\n",
		"child table header":      "[shell_environment_policy.set]\nFOO = \"bar\"\n",
		"quoted table header":     "[\"shell_environment_policy\"]\ninherit = \"all\"\n",
		"literal table header":    "['shell_environment_policy']\ninherit = \"all\"\n",
		"spaced table header":     "[ shell_environment_policy ]\ninherit = \"all\"\n",
		"indented table header":   "  [shell_environment_policy]\ninherit = \"all\"\n",
		"array of tables header":  "[[shell_environment_policy]]\ninherit = \"all\"\n",
		"header trailing comment": "[shell_environment_policy] # thread owned\ninherit = \"all\"\n",
		"header bracket decoy":    "[shell_environment_policy] # ]\ninherit = \"all\"\n",
		"unterminated header":     "[shell_environment_policy\ninherit = \"all\"\n",
		"crlf line endings":       "model = \"gpt-5.5\"\r\n[shell_environment_policy]\r\ninherit = \"all\"\r\n",
		"byte order mark":         "\ufeff[shell_environment_policy]\ninherit = \"all\"\n",
		"nested under a parent":   "[profiles.staging]\nshell_environment_policy.inherit = \"all\"\n",
		"after a decoy in a string": "banner = \"\"\"\n[decoy]\n\"\"\"\n" +
			"shell_environment_policy.inherit = \"all\"\n",
		"mcp servers table":  "[mcp_servers.exfil]\ncommand = \"sh\"\n",
		"mcp servers dotted": "mcp_servers.exfil.command = \"sh\"\n",
		"mcp servers quoted": "[\"mcp_servers\".exfil]\ncommand = \"sh\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()

			requireUnsupported(t, writeSeedFiles(home, map[string]string{"config.toml": content}))
			require.Empty(t, seedFilesInHome(t, home), "a rejected seed must leave the home untouched")
		})
	}
}

// TestWriteSeedFilesReservedRootsSurviveKeySpelling proves the guard keys off
// the resolved destination rather than the caller's key, so a path that spells
// the same CODEX_HOME config file differently cannot walk past it. `Config.toml`
// is the same file wherever the filesystem folds case, so it is refused
// everywhere rather than only on the hosts where it would have been loaded.
func TestWriteSeedFilesReservedRootsSurviveKeySpelling(t *testing.T) {
	const reserved = "[shell_environment_policy]\ninherit = \"all\"\n"

	for _, key := range []string{"config.toml", "./config.toml", "Config.toml", "CONFIG.TOML"} {
		home := t.TempDir()

		requireUnsupported(t, writeSeedFiles(home, map[string]string{key: reserved}))
		require.Empty(t, seedFilesInHome(t, home), "a rejected seed key %q must leave the home untouched", key)
	}
}

// TestWriteSeedFilesAcceptsLegitimateConfig proves the guard costs nothing to
// the configuration seeding exists for: a gateway definition, a neighbouring
// root that merely shares a prefix, and prose naming a reserved root in a
// comment all still write.
func TestWriteSeedFilesAcceptsLegitimateConfig(t *testing.T) {
	home := t.TempDir()
	config := "# shell_environment_policy.inherit = \"all\" is never seeded; the thread owns it\n" +
		"model = \"gpt-5.5\"\n" +
		"model_provider = \"litellm\"\n" +
		"shell_environment_policy_extra = \"ok\"\n" +
		"[model_providers.litellm]\n" +
		"base_url = \"https://litellm.example/v1\"\n" +
		"env_key = \"LITELLM_API_KEY\"\n"

	require.NoError(t, writeSeedFiles(home, map[string]string{
		"config.toml": config,
		// Codex loads config only from CODEX_HOME/config.toml, so a reserved
		// root in any other seeded file reaches no thread and stays allowed.
		"profiles/config.toml": "[shell_environment_policy]\ninherit = \"all\"\n",
	}))

	written, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, config, string(written))
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
// directory permissions: a process with filesystem override capability and
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
