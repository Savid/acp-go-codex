package codexacp

import (
	"context"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
)

func TestValidateAbsolutePathsRejectsRelative(t *testing.T) {
	if err := validateAbsolutePaths("paths", []string{"/ok", "relative"}); err == nil {
		t.Fatal("validateAbsolutePaths accepted relative path")
	}
}

func TestValidatePathBranches(t *testing.T) {
	if err := validateRequiredAbsolutePath("cwd", ""); err == nil {
		t.Fatal("validateRequiredAbsolutePath accepted empty path")
	}
	if err := validateRequiredAbsolutePath("cwd", "relative"); err == nil {
		t.Fatal("validateRequiredAbsolutePath accepted relative path")
	}
	if err := validateSessionStartPaths("", nil); err == nil {
		t.Fatal("validateSessionStartPaths accepted empty cwd")
	}
	if err := validateSessionStartPaths(absTestPath("repo"), []string{"relative"}); err == nil {
		t.Fatal("validateSessionStartPaths accepted relative additional directory")
	}
	if err := validateAbsolutePaths("paths", []string{""}); err == nil {
		t.Fatal("validateAbsolutePaths accepted empty path")
	}
	if err := validateOptionalAbsolutePath("cwd", nil); err != nil {
		t.Fatalf("nil optional path returned error: %v", err)
	}
	path := absTestPath("repo")
	if err := validateOptionalAbsolutePath("cwd", &path); err != nil {
		t.Fatalf("absolute optional path returned error: %v", err)
	}
}

// A relative or absent cwd is one verdict on one field, in the uniform shape
// every other rejection uses.
func TestSessionEntryPointsRefuseARelativeCwd(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))

	t.Cleanup(func() { _ = agent.Close() })

	want := map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldCwd}

	for _, cwd := range []string{"", "relative/project"} {
		_, err := agent.NewSession(ctx, NewSessionRequest(cwd))
		requireInvalidParamsData(t, err, want)

		_, err = agent.LoadSession(ctx, LoadSessionRequest("session", cwd))
		requireInvalidParamsData(t, err, want)

		_, err = agent.ResumeSession(ctx, ResumeSessionRequest("session", cwd))
		requireInvalidParamsData(t, err, want)

		_, err = agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
		requireInvalidParamsData(t, err, want)
	}
}
