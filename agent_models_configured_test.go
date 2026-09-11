package codexacp

import (
	"context"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestConfiguredModelsRefuseMalformedIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{name: "empty", ids: []string{""}, want: "is not a model id"},
		{name: "surrounding space", ids: []string{"vendor/qwen "}, want: "is not a model id"},
		{name: "duplicate", ids: []string{"vendor/qwen", "vendor/qwen"}, want: "listed twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, NewAgent(WithConfiguredModels(tc.ids)).optionsErr, tc.want)
		})
	}

	require.NoError(t, NewAgent(WithConfiguredModels([]string{"vendor/qwen", "vendor/deepseek"})).optionsErr)
}

// TestHostListedModelsFollowThePresets pins the host-listed entry against the
// native catalog: it follows the presets as the id alone, a preset of the same
// id stands with its facts, and the current model still closes the menu.
func TestHostListedModelsFollowThePresets(t *testing.T) {
	presets := []codex.Model{{ID: "gpt-5.5", Name: "GPT-5.5", Context: 400000}}
	hostListed := []string{"vendor/qwen", "gpt-5.5", "vendor/deepseek"}

	options := codexConfigOptions("vendor/other", modeDefault, "", "", "", presets, true, hostListed)
	values := *options[0].Select.Options.Ungrouped
	require.Equal(t,
		[]acp.SessionConfigValueId{"gpt-5.5", "vendor/qwen", "vendor/deepseek", "vendor/other"},
		configOptionValueIDs(t, options, configModel),
	)
	require.Equal(t, "GPT-5.5", values[0].Name)
	require.NotEmpty(t, values[0].Meta, "the native row stands and the host entry adds nothing")
	require.Equal(t, "vendor/qwen", values[1].Name)
	require.Empty(t, values[1].Meta, "a host-listed row carries no invented facts")
}

// TestHostListedModelsSurviveWithheldPresets pins the other route: with the
// presets withheld the host-listed ids are still published, except one naming
// a withheld preset, and the withheld facts stay withheld.
func TestHostListedModelsSurviveWithheldPresets(t *testing.T) {
	presets := []codex.Model{{
		ID: "gpt-5.5", Name: "GPT-5.5", DefaultReasoningEffort: "medium",
		ReasoningEfforts: []codex.ModelReasoningEffort{{ID: "low"}, {ID: "medium"}},
	}}
	hostListed := []string{"vendor/qwen", "gpt-5.5"}

	options := codexConfigOptions("vendor/qwen", modeDefault, "", "", "", presets, false, hostListed)
	require.Equal(t, []acp.SessionConfigValueId{"vendor/qwen"}, configOptionValueIDs(t, options, configModel))
	require.Empty(t, (*options[0].Select.Options.Ungrouped)[0].Meta)
	require.Equal(t, []acp.SessionConfigId{configModel, configMode}, configOptionIDs(options),
		"a host-listed id lends the withheld presets no effort menu")
}

// TestHostListedModelsReachTheSessionMenu pins the agent wiring: the option
// reaches every session's advertised menu on a route that withholds the
// presets, and stays out of the way of the current model.
func TestHostListedModelsReachTheSessionMenu(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	client.setRoute(codex.ProviderRoute{ProviderID: "omp"}, nil)
	agent := NewAgent(
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
		WithAmbientEnvironment(map[string]string{"HOME": t.TempDir()}),
		WithConfiguredModels([]string{"vendor/qwen", "vendor/deepseek", "gpt-initial"}),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest(
		absTestPath("tmp", "project"),
		WithSessionCodexOptions(CodexOptions{Model: "vendor/deepseek"}),
	))
	require.NoError(t, err)
	require.Equal(t,
		[]acp.SessionConfigValueId{"vendor/qwen", "vendor/deepseek"},
		configOptionValueIDs(t, resp.ConfigOptions, configModel),
		"gpt-initial names a withheld preset and is withheld with it",
	)
	requireConfigCurrentValue(t, resp.ConfigOptions, configModel, "vendor/deepseek")
}
