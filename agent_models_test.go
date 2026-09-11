package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestSessionConfigOptionsMutateTurnSettings(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest(absTestPath("tmp", "project"),
		WithSessionCodexOptions(NewCodexOptions(
			WithCodexModel("gpt-initial"),
			WithCodexEffort("low"),
			WithCodexServiceTier("flex"),
			WithCodexPersonality("pragmatic"),
		)),
	))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	requireNoTopLevelConfigState(t, resp)
	if len(resp.ConfigOptions) != 5 {
		t.Fatalf("config options = %d, want 5", len(resp.ConfigOptions))
	}

	_, err = agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configMode,
			Value:     acp.SessionConfigValueId(modePlan),
		},
	})
	if err != nil {
		t.Fatalf("SetSessionConfigOption mode returned error: %v", err)
	}
	_, err = agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configEffort,
			Value:     acp.SessionConfigValueId("high"),
		},
	})
	if err != nil {
		t.Fatalf("SetSessionConfigOption effort returned error: %v", err)
	}

	_, err = agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "test-turn", "hello"))
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}

	client.mu.Lock()
	turn := client.lastTurn
	client.mu.Unlock()

	if turn.Model != "gpt-initial" || turn.ReasoningEffort != "high" || turn.ServiceTier != "flex" || turn.Personality != "pragmatic" {
		t.Fatalf("turn settings = %#v", turn)
	}
	mode, _ := turn.CollaborationMode.(map[string]any)
	if mode["mode"] != string(modePlan) {
		t.Fatalf("collaboration mode = %#v, want plan", turn.CollaborationMode)
	}
	if len(conn.updates) == 0 {
		t.Fatal("expected config updates")
	}
	for _, update := range conn.updates {
		if update.Update.CurrentModeUpdate != nil {
			t.Fatalf("removed mode update emitted: %#v", update.Update.CurrentModeUpdate)
		}
	}
}

func TestLifecycleConfigValuesPassThroughToNative(t *testing.T) {
	const unknown = "registry-unknown"

	for _, test := range []struct {
		name     string
		id       acp.SessionConfigId
		options  CodexOptions
		asserted func(*testing.T, *spyCodexClient)
	}{
		{
			name:    "effort",
			id:      configEffort,
			options: CodexOptions{Effort: unknown},
			asserted: func(t *testing.T, client *spyCodexClient) {
				t.Helper()
				require.Equal(t, unknown, client.lastTurn.ReasoningEffort)
			},
		},
		{
			name:    "personality",
			id:      configPersonality,
			options: CodexOptions{Personality: unknown},
			asserted: func(t *testing.T, client *spyCodexClient) {
				t.Helper()
				require.Equal(t, unknown, client.start.Personality)
				require.Equal(t, unknown, client.lastTurn.Personality)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newSpyCodexClient()
			agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				return client, nil
			}))
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			agent.setAgentClient(newRecordingAgentClient())

			resp, err := agent.NewSession(context.Background(), NewSessionRequest(
				absTestPath("tmp", "project"),
				WithSessionCodexOptions(test.options),
			))
			require.NoError(t, err)
			requireConfigCurrentValue(t, resp.ConfigOptions, test.id, unknown)

			_, err = agent.Prompt(context.Background(), TextPromptRequest(resp.SessionId, "test-turn", "hello"))
			require.NoError(t, err)

			client.mu.Lock()
			defer client.mu.Unlock()
			test.asserted(t, client)
		})
	}
}

func TestSetSessionConfigValuesPassThroughToNative(t *testing.T) {
	const unknown = "registry-unknown"

	for _, id := range []acp.SessionConfigId{configMode, configEffort, configPersonality} {
		t.Run(string(id), func(t *testing.T) {
			client := newSpyCodexClient()
			agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				return client, nil
			}))
			agent.setAgentClient(newRecordingAgentClient())

			resp, err := agent.NewSession(context.Background(), NewSessionRequest(absTestPath("tmp", "project")))
			require.NoError(t, err)

			updated, err := agent.SetSessionConfigOption(
				context.Background(),
				SetConfigOptionRequest(resp.SessionId, id, unknown),
			)
			require.NoError(t, err)
			requireConfigCurrentValue(t, updated.ConfigOptions, id, unknown)

			_, err = agent.Prompt(context.Background(), TextPromptRequest(resp.SessionId, "test-turn", "hello"))
			require.NoError(t, err)

			client.mu.Lock()
			defer client.mu.Unlock()

			switch id {
			case configMode:
				mode := asType[map[string]any](t, client.lastTurn.CollaborationMode)
				require.Equal(t, unknown, mode[jsonFieldMode])
			case configEffort:
				require.Equal(t, unknown, client.lastTurn.ReasoningEffort)
			case configPersonality:
				require.Equal(t, unknown, client.lastTurn.Personality)
			}
		})
	}
}

// The two refusal texts below are measured, not invented: they are the exact
// bodies native returns when a value outside its own serde variant set reaches
// a door — `personality` at `thread/start`, and `mode` and `personality` at
// `turn/start`. Native reports them as JSON-RPC `-32600`; the adapter
// re-surfaces the text unchanged in `data.message` of its own `-32603` turn
// failure, which is what the pins here assert. Each text quotes the offending
// value, so it matches only the `registry-unknown` these pins send.
//
// The variant lists restate the same measurement as data. There is deliberately
// no effort list, because the door serde-checks `mode` and `personality` and
// checks `effort` against nothing at all.
//
// Source: codex-cli 0.148.0. Re-measure on a harness version bump — a drifted
// text, a widened variant set, or a door that has begun refusing an effort
// leaves every pin below describing a harness that no longer exists.
const (
	nativeModeRefusal        = "Invalid request: unknown variant `registry-unknown`, expected one of `plan`, `code`, `custom`, `default`, `execute`, `pair_programming`"
	nativePersonalityRefusal = "Invalid request: unknown variant `registry-unknown`, expected one of `none`, `friendly`, `pragmatic`"
)

var (
	nativeModeVariants        = []string{"plan", "code", "custom", "default", "execute", "pair_programming"}
	nativePersonalityVariants = []string{effortValueNone, personalityFriendly, personalityPragmatic}
)

func TestMeasuredNativeConfigRefusalsPropagate(t *testing.T) {
	const unknown = "registry-unknown"

	t.Run("personality during establishment", func(t *testing.T) {
		client := &nativeStartRefusalClient{
			spyCodexClient: newSpyCodexClient(),
			failure:        errors.New(nativePersonalityRefusal),
		}
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return client, nil
		}))

		_, err := agent.NewSession(context.Background(), NewSessionRequest(
			absTestPath("tmp", "project"),
			WithSessionCodexOptions(CodexOptions{Personality: unknown}),
		))
		require.EqualError(t, err, nativePersonalityRefusal)

		client.mu.Lock()
		defer client.mu.Unlock()
		require.Equal(t, unknown, client.start.Personality)
	})

	for _, test := range []struct {
		name    string
		id      acp.SessionConfigId
		message string
	}{
		{
			name:    "mode",
			id:      configMode,
			message: nativeModeRefusal,
		},
		{
			name:    "personality",
			id:      configPersonality,
			message: nativePersonalityRefusal,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &nativeConfigRefusalClient{
				spyCodexClient: newSpyCodexClient(),
				failure:        errors.New(test.message),
			}
			agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				return client, nil
			}))
			agent.setAgentClient(newRecordingAgentClient())

			resp, err := agent.NewSession(context.Background(), NewSessionRequest(absTestPath("tmp", "project")))
			require.NoError(t, err)
			_, err = agent.SetSessionConfigOption(
				context.Background(),
				SetConfigOptionRequest(resp.SessionId, test.id, unknown),
			)
			require.NoError(t, err)

			_, err = agent.Prompt(context.Background(), TextPromptRequest(resp.SessionId, "test-turn", "hello"))
			var requestErr *acp.RequestError
			require.ErrorAs(t, err, &requestErr)
			require.Equal(t, -32603, requestErr.Code)
			data := asType[map[string]any](t, requestErr.Data)
			require.Equal(t, valueTurnFailed, data[jsonFieldError])
			require.Equal(t, codex.CauseProvider, data[jsonFieldCause])
			require.Equal(t, "Codex provider turn failed", data[jsonFieldMessage])
			require.NotContains(t, err.Error(), test.message)
		})
	}
}

// TestMeasuredNativeEffortIsAcceptedAtTheDoor pins the half of the measurement
// the refusal table above cannot show. The three menu-valued options do not
// fail alike: an unknown `mode` or `personality` is refused at the door and the
// adapter reports that verdict, while any `effort` string is accepted and the
// turn goes live. Effort therefore gets no truth signal at the door, and the
// effort a host reads back is the effort it sent — an echo, not a native
// confirmation.
//
// What a turn then does with an effort native never named is deliberately
// unpinned: reading it costs model tokens, so it was not measured and is not
// asserted. This pins the door as it stands, so a harness that begins refusing
// an unknown effort breaks the pin and forces the re-look.
func TestMeasuredNativeEffortIsAcceptedAtTheDoor(t *testing.T) {
	const unknown = "registry-unknown"

	newDoorAgent := func(t *testing.T) (*Agent, *measuredNativeTurnDoorClient) {
		t.Helper()

		client := &measuredNativeTurnDoorClient{spyCodexClient: newSpyCodexClient()}
		agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return client, nil
		}))
		agent.setAgentClient(newRecordingAgentClient())

		return agent, client
	}

	t.Run("effort", func(t *testing.T) {
		ctx := context.Background()
		agent, client := newDoorAgent(t)

		resp, err := agent.NewSession(ctx, NewSessionRequest(
			absTestPath("tmp", "project"),
			WithSessionCodexOptions(CodexOptions{Effort: unknown}),
		))
		require.NoError(t, err)
		requireConfigCurrentValue(t, resp.ConfigOptions, configEffort, unknown)

		prompted, err := agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "test-turn", "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, prompted.StopReason)

		client.mu.Lock()
		require.Equal(t, unknown, client.lastTurn.ReasoningEffort)
		client.mu.Unlock()

		// The door returned no verdict on the value, so nothing corrects it.
		// After a completed turn the advertised effort is still the sent one.
		session := agent.activeSession(resp.SessionId)
		require.NotNil(t, session)
		requireConfigCurrentValue(t, agent.sessionConfigOptions(ctx, session, nil), configEffort, unknown)
	})

	// The same door, driven with the option native does serde-check. Without
	// this the acceptance above would only prove the double says yes to
	// everything; with it, the double refuses exactly what native refuses.
	t.Run("personality at the same door", func(t *testing.T) {
		ctx := context.Background()
		agent, _ := newDoorAgent(t)

		resp, err := agent.NewSession(ctx, NewSessionRequest(absTestPath("tmp", "project")))
		require.NoError(t, err)

		_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(resp.SessionId, configPersonality, unknown))
		require.NoError(t, err)

		_, err = agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "test-turn", "hello"))
		var requestErr *acp.RequestError
		require.ErrorAs(t, err, &requestErr)
		data := asType[map[string]any](t, requestErr.Data)
		require.Equal(t, "Codex provider turn failed", data[jsonFieldMessage])
		require.NotContains(t, err.Error(), nativePersonalityRefusal)
	})
}

type nativeStartRefusalClient struct {
	*spyCodexClient
	failure error
}

func (c *nativeStartRefusalClient) StartThread(
	_ context.Context,
	request codex.ThreadStartRequest,
) (codex.Thread, error) {
	c.mu.Lock()
	c.start = request
	c.mu.Unlock()

	return codex.Thread{}, c.failure
}

type nativeConfigRefusalClient struct {
	*spyCodexClient
	failure error
}

func (c *nativeConfigRefusalClient) RunTurn(_ context.Context, request codex.TurnStartRequest) (codex.Turn, error) {
	c.mu.Lock()
	c.lastTurn = request
	c.mu.Unlock()

	return codex.Turn{}, c.failure
}

// measuredNativeTurnDoorClient answers `turn/start` the way native was measured
// to answer it. A `mode` or `personality` outside its serde variant set is
// refused with the pinned text; a `reasoningEffort` is checked against nothing,
// because native checks it against nothing — every string is accepted and the
// turn goes live. Absent values are omitted on the wire and reach no check.
type measuredNativeTurnDoorClient struct {
	*spyCodexClient
}

func (c *measuredNativeTurnDoorClient) RunTurn(
	ctx context.Context,
	request codex.TurnStartRequest,
) (codex.Turn, error) {
	mode, _ := request.CollaborationMode.(map[string]any)
	if name, ok := mode[jsonFieldMode].(string); ok && !measuredNativeVariant(name, nativeModeVariants) {
		return c.refuse(request, nativeModeRefusal)
	}

	if name, ok := request.Personality.(string); ok && !measuredNativeVariant(name, nativePersonalityVariants) {
		return c.refuse(request, nativePersonalityRefusal)
	}

	return c.spyCodexClient.RunTurn(ctx, request)
}

func (c *measuredNativeTurnDoorClient) refuse(request codex.TurnStartRequest, message string) (codex.Turn, error) {
	c.mu.Lock()
	c.lastTurn = request
	c.mu.Unlock()

	return codex.Turn{}, errors.New(message)
}

func measuredNativeVariant(value string, variants []string) bool {
	return value == "" || slices.Contains(variants, value)
}

func requireConfigCurrentValue(
	t *testing.T,
	options []acp.SessionConfigOption,
	id acp.SessionConfigId,
	want acp.SessionConfigValueId,
) {
	t.Helper()

	for _, option := range options {
		if option.Select != nil && option.Select.Id == id {
			require.Equal(t, want, option.Select.CurrentValue)

			return
		}
	}

	require.Failf(t, "missing config option", "config option %q is absent from %#v", id, options)
}

// TestSetSessionConfigOptionRejectionsCarryTheUniformUnsupportedShape pins
// session/set_config_option to the uniform unsupported-field error and to the
// request member each rejection actually faults. Every advertised option is a
// select, so a boolean payload faults the `type` discriminator that selected
// the boolean variant, an empty or absent value faults `value`, and only an
// unrecognised configId names configId.
func TestSetSessionConfigOptionRejectionsCarryTheUniformUnsupportedShape(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest(absTestPath("tmp", "project")))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	for name, test := range map[string]struct {
		params acp.SetSessionConfigOptionRequest
		field  string
	}{
		"empty mode value": {
			params: SetConfigOptionRequest(resp.SessionId, configMode, ""),
			field:  jsonFieldValue,
		},
		"empty effort value": {
			params: SetConfigOptionRequest(resp.SessionId, configEffort, ""),
			field:  jsonFieldValue,
		},
		"empty personality value": {
			params: SetConfigOptionRequest(resp.SessionId, configPersonality, ""),
			field:  jsonFieldValue,
		},
		"config id is not advertised": {
			params: SetConfigOptionRequest(resp.SessionId, "unknown", "x"),
			field:  jsonFieldConfigID,
		},
		"boolean discriminator": {
			params: acp.SetSessionConfigOptionRequest{Boolean: &acp.SetSessionConfigOptionBoolean{
				SessionId: resp.SessionId, ConfigId: configMode, Value: true,
			}},
			field: jsonFieldType,
		},
		"no value at all": {
			params: acp.SetSessionConfigOptionRequest{},
			field:  jsonFieldValue,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := agent.SetSessionConfigOption(ctx, test.params)
			requireInvalidParamsField(t, err, test.field)
		})
	}
}

func TestCodexConfigOptionsExposeModelCatalogAndEffort(t *testing.T) {
	models := []codex.Model{{
		ID:                     "gpt-5.5",
		Name:                   "GPT-5.5",
		Description:            "Frontier model",
		DefaultReasoningEffort: "medium",
		ReasoningEfforts: []codex.ModelReasoningEffort{
			{ID: "low", Description: "Fast"},
			{ID: "medium", Description: "Balanced"},
		},
		Raw: map[string]any{"displayName": "GPT-5.5"},
	}}

	options := codexConfigOptions("gpt-5.5", modeDefault, "", "", "", models, true)
	if len(options) != 3 {
		t.Fatalf("config options = %#v", options)
	}
	modelOption := options[0].Select
	if modelOption == nil || modelOption.Id != configModel || modelOption.Category == nil || *modelOption.Category != acp.SessionConfigOptionCategoryModel {
		t.Fatalf("model option = %#v", modelOption)
	}
	modelValues := *modelOption.Options.Ungrouped
	modelMeta, _ := modelValues[0].Meta[codexMetaKey].(map[string]any)
	if modelValues[0].Name != "GPT-5.5" || modelValues[0].Description == nil || *modelValues[0].Description != "Frontier model" || modelMeta["modelId"] != "gpt-5.5" {
		t.Fatalf("model values = %#v", modelValues)
	}
	effortOption := options[2].Select
	if effortOption == nil || effortOption.Id != configEffort || effortOption.CurrentValue != "medium" || effortOption.Category == nil || *effortOption.Category != acp.SessionConfigOptionCategoryThoughtLevel {
		t.Fatalf("effort option = %#v", effortOption)
	}
	effortValues := *effortOption.Options.Ungrouped
	if len(effortValues) != 2 || effortValues[0].Description == nil || *effortValues[0].Description != "Fast" {
		t.Fatalf("effort values = %#v", effortValues)
	}
}

func TestCodexConfigOptionsEdgeBranches(t *testing.T) {
	models := []codex.Model{
		{},
		{
			ID:          "gpt-a",
			Name:        "GPT A",
			Context:     123,
			Description: "primary",
			ReasoningEfforts: []codex.ModelReasoningEffort{
				{ID: "low"},
			},
			Raw: map[string]any{"capabilities": []any{"vision", "", 42}},
		},
		{ID: "gpt-a", Name: "duplicate"},
	}

	options := codexConfigOptions("custom-model", "", "", "priority", "friendly", models, true)
	if len(options) != 4 {
		t.Fatalf("config options = %#v", options)
	}
	if options[1].Select.CurrentValue != acp.SessionConfigValueId(modeDefault) {
		t.Fatalf("default mode option = %#v", options[1].Select)
	}
	modelValues := *options[0].Select.Options.Ungrouped
	if len(modelValues) != 2 || modelValues[1].Value != "custom-model" {
		t.Fatalf("model values = %#v", modelValues)
	}
	modelMeta, _ := modelValues[0].Meta[codexMetaKey].(map[string]any)
	if modelMeta["contextWindow"] == nil {
		t.Fatalf("model meta missing context: %#v", modelMeta)
	}
	if _, present := modelMeta["capabilities"]; present {
		t.Fatalf("model capabilities must be absent: %#v", modelMeta)
	}
	if modelByID("missing", models) != nil {
		t.Fatal("modelByID found missing model")
	}
}

func TestEffortConfigValuesCoverCatalogEdges(t *testing.T) {
	models := []codex.Model{{
		ID:                     "gpt-5.5",
		DefaultReasoningEffort: "medium",
		ReasoningEfforts: []codex.ModelReasoningEffort{
			{},
			{ID: "low", Description: "Fast"},
			{ID: "low", Description: "Duplicate"},
			{ID: "high"},
		},
	}}

	current, values := effortConfigValues("gpt-5.5", "custom", models)
	if current != "custom" {
		t.Fatalf("current effort = %q", current)
	}
	if len(values) != 3 || values[0].Value != "low" || values[1].Value != "high" || values[2].Value != "custom" {
		t.Fatalf("catalog effort values = %#v", values)
	}
	if values[0].Description == nil || *values[0].Description != "Fast" {
		t.Fatalf("catalog effort description = %#v", values[0].Description)
	}

	current, values = effortConfigValues("missing", "high", models)
	if current != "high" || len(values) != len(codexEffortValues) {
		t.Fatalf("fallback effort values = current %q values %#v", current, values)
	}

	current, values = effortConfigValues("missing", "", models)
	if current != "" || values != nil {
		t.Fatalf("empty fallback effort values = current %q values %#v", current, values)
	}
}

func TestSessionConfigSettersRespectTurnLock(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest(absTestPath("tmp", "project")))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	session := agent.sessionMust(resp.SessionId)
	held := session.turnQueue()
	held <- struct{}{}
	defer func() { <-held }()

	if _, err := agent.SetSessionConfigOption(canceledContext(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configMode,
			Value:     acp.SessionConfigValueId(modePlan),
		},
	}); err == nil {
		t.Fatal("SetSessionConfigOption mode ignored canceled turn lock")
	}
	if _, err := agent.SetSessionConfigOption(canceledContext(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configModel,
			Value:     "gpt-other",
		},
	}); err == nil {
		t.Fatal("SetSessionConfigOption model ignored canceled turn lock")
	}
}

func TestSetSessionConfigOptionErrorBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	if _, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest("missing", configModel, "gpt")); err == nil {
		t.Fatal("SetSessionConfigOption accepted missing session")
	}
	if _, err := agent.SetSessionMode(ctx, acp.SetSessionModeRequest{}); err == nil {
		t.Fatal("SetSessionMode succeeded")
	}

	updateErr := errors.New("update failed")
	agent = NewAgent()
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: updateErr})
	session := newSession(agent, "session-1", absTestPath("tmp", "project"), nil, codex.Thread{ID: "thread-1"}, newSpyCodexClient(), sessionMeta{}, nil)
	if err := agent.storeStartedSession(session); err != nil {
		t.Fatalf("store session: %v", err)
	}
	if _, err := agent.SetSessionConfigOption(ctx, SetModelRequest("session-1", "gpt-other")); !errors.Is(err, updateErr) {
		t.Fatalf("SetSessionConfigOption update error = %v", err)
	}
}

func requireNoTopLevelConfigState(t *testing.T, response any) {
	t.Helper()

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if _, ok := object["configOptions"]; !ok {
		t.Fatalf("response missing configOptions: %s", string(encoded))
	}
	if _, ok := object["models"]; ok {
		t.Fatalf("response contains removed models: %s", string(encoded))
	}
	if _, ok := object["modes"]; ok {
		t.Fatalf("response contains removed modes: %s", string(encoded))
	}
}

func TestUnstableConfigOptionMapping(t *testing.T) {
	boolOpt := acp.SessionConfigOption{Boolean: &acp.SessionConfigOptionBoolean{Id: "b", Name: "Bool", Type: "checkbox", CurrentValue: true}}
	if unstableConfigOption(boolOpt).Boolean == nil {
		t.Fatal("unstableConfigOption did not map boolean")
	}
	if unstableConfigOption(acp.SessionConfigOption{}).Select != nil {
		t.Fatal("empty unstableConfigOption produced select")
	}
}

// TestNativeModelMenuFollowsRoutingAndLogin pins the catalog rule: `model/list`
// reports the presets the Codex build ships, so the adapter publishes them only
// for a session whose requests actually reach OpenAI's own endpoint under a
// credential. Otherwise the menu carries the session's current model alone —
// the one id such a session can use.
func TestNativeModelMenuFollowsRoutingAndLogin(t *testing.T) {
	const gatewayModel = "opencode-go/qwen3.8-flash"

	presets := []acp.SessionConfigValueId{"gpt-initial", "gpt-other", gatewayModel}
	configuredOnly := []acp.SessionConfigValueId{gatewayModel}

	for _, tc := range []struct {
		name     string
		provider string
		route    codex.ProviderRoute
		routeErr error
		account  codex.Account
		ambient  map[string]string
		options  []Option
		want     []acp.SessionConfigValueId
	}{
		{
			name:    "routed to openai under a subscription account",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			account: codex.Account{AuthMode: codex.AuthModeChatGPT},
			want:    presets,
		},
		{
			name:    "routed to openai under a stored API key",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			account: codex.Account{AuthMode: codex.AuthModeAPIKey},
			want:    presets,
		},
		{
			// Codex signs requests with this variable and reports no account
			// for it, so the environment is the only place it is visible.
			name:    "routed to openai under an environment key",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			ambient: map[string]string{nativeAuthEnvAPIKey: "sk-test"},
			want:    presets,
		},
		{
			name:    "routed to openai under a launch-block key",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			options: []Option{WithEnv(map[string]string{nativeAuthEnvAPIKey: "sk-test"})},
			want:    presets,
		},
		{
			// The launch block is applied over the ambient one, so an entry it
			// names withdraws the inherited value it blanks.
			name:    "environment key withdrawn by the launch block",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			ambient: map[string]string{nativeAuthEnvAPIKey: "sk-inherited"},
			options: []Option{WithEnv(map[string]string{nativeAuthEnvAPIKey: ""})},
			want:    configuredOnly,
		},
		{
			// Codex sends no authorization header for this variable, so it
			// signs nothing.
			name:    "routed to openai with only an agent identity token",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			ambient: map[string]string{"CODEX_ACCESS_TOKEN": "tok"},
			want:    configuredOnly,
		},
		{
			name:  "routed to openai and signed out",
			route: codex.ProviderRoute{ProviderID: authProviderOpenAI},
			want:  configuredOnly,
		},
		{
			// OPENAI_API_KEY does not authenticate native Codex requests.
			name:    "routed to openai with only OPENAI_API_KEY",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			ambient: map[string]string{"OPENAI_API_KEY": "sk-unrelated"},
			want:    configuredOnly,
		},
		{
			name:    "routed to a gateway provider",
			route:   codex.ProviderRoute{ProviderID: "omp"},
			account: codex.Account{AuthMode: codex.AuthModeChatGPT},
			want:    configuredOnly,
		},
		{
			// An endpoint override keeps the provider id and changes where its
			// requests land, which is what Custom reports.
			name:    "routed to openai at another endpoint",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI, Custom: true},
			account: codex.Account{AuthMode: codex.AuthModeChatGPT},
			want:    configuredOnly,
		},
		{
			name:    "account that does not sign OpenAI requests",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI},
			account: codex.Account{AuthMode: "amazonBedrock"},
			want:    configuredOnly,
		},
		{
			name:     "route the app-server will not report",
			route:    codex.ProviderRoute{ProviderID: authProviderOpenAI},
			routeErr: errors.New("config"),
			account:  codex.Account{AuthMode: codex.AuthModeChatGPT},
			want:     configuredOnly,
		},
		{
			// An OPENAI_BASE_URL in the app-server's environment moves no
			// request, so it must not withdraw the presets.
			name:    "environment base URL that routes nothing",
			route:   codex.ProviderRoute{ProviderID: authProviderOpenAI, EnvironmentBaseURL: true},
			account: codex.Account{AuthMode: codex.AuthModeChatGPT},
			want:    presets,
		},
		{
			// The thread carries the provider it started on; the workspace
			// carries the one a turn started now would use. Either withdraws.
			name:     "thread started on a gateway provider",
			provider: "omp",
			route:    codex.ProviderRoute{ProviderID: authProviderOpenAI},
			account:  codex.Account{AuthMode: codex.AuthModeChatGPT},
			want:     configuredOnly,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			client := newSpyCodexClient()
			client.thread.Provider = firstNonEmpty(tc.provider, authProviderOpenAI)
			client.setRoute(tc.route, tc.routeErr)
			client.setAccount(tc.account)

			// The adapter's own process environment can carry a Codex
			// credential, so the case declares the whole ambient block rather
			// than adding to the operator's.
			ambient := map[string]string{"HOME": t.TempDir()}
			maps.Copy(ambient, tc.ambient)
			agent := NewAgent(append(
				[]Option{
					withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
						return client, nil
					}),
					WithAmbientEnvironment(ambient),
				},
				tc.options...,
			)...)

			cwd := absTestPath("tmp", "project")
			resp, err := agent.NewSession(ctx, NewSessionRequest(
				cwd,
				WithSessionCodexOptions(CodexOptions{Model: gatewayModel}),
			))
			require.NoError(t, err)

			require.Equal(t, tc.want, configOptionValueIDs(t, resp.ConfigOptions, configModel))
			requireConfigCurrentValue(t, resp.ConfigOptions, configModel, gatewayModel)
			if tc.provider == "" {
				require.Equal(t, cwd, client.routeCwdSnapshot(),
					"the route is workspace-scoped and must be read for the session's own")
			}
		})
	}
}

// TestSuppressedNativeCatalogAlsoWithdrawsItsEffortMenu pins that the presets
// are trusted as a whole: a gateway model id that collides with a preset must
// not inherit the preset's reasoning-effort levels or its metadata.
func TestSuppressedNativeCatalogAlsoWithdrawsItsEffortMenu(t *testing.T) {
	models := []codex.Model{{
		ID:                     "gpt-5.5",
		Name:                   "GPT-5.5",
		DefaultReasoningEffort: "medium",
		ReasoningEfforts:       []codex.ModelReasoningEffort{{ID: "low"}, {ID: "medium"}},
	}}

	native := codexConfigOptions("gpt-5.5", modeDefault, "", "", "", models, true)
	requireConfigCurrentValue(t, native, configEffort, "medium")
	require.Len(t, *native[0].Select.Options.Ungrouped, 1)
	require.NotEmpty(t, (*native[0].Select.Options.Ungrouped)[0].Meta)

	withoutSelection := codexConfigOptions("gpt-5.5", modeDefault, "", "", "", models, false)
	require.Equal(t,
		[]acp.SessionConfigId{configModel, configMode},
		configOptionIDs(withoutSelection),
		"with no effort selected and no catalog describing the model, there is no effort menu to publish",
	)

	// A value chosen while the presets applied stays selectable after they are
	// withdrawn: a select never reports a current value outside its options.
	outsideVocabulary := codexConfigOptions("gpt-5.5", modeDefault, "ultra", "", "", models, false)
	requireConfigCurrentValue(t, outsideVocabulary, configEffort, "ultra")
	require.Contains(t,
		[]acp.SessionConfigSelectOption(*outsideVocabulary[2].Select.Options.Ungrouped),
		acp.SessionConfigSelectOption{Name: "ultra", Value: "ultra"},
	)

	suppressed := codexConfigOptions("gpt-5.5", modeDefault, "high", "", "", models, false)
	values := *suppressed[0].Select.Options.Ungrouped
	require.Len(t, values, 1)
	require.Equal(t, acp.SessionConfigValueId("gpt-5.5"), values[0].Value)
	require.Empty(t, values[0].Meta, "a suppressed preset publishes none of its facts")
	requireConfigCurrentValue(t, suppressed, configEffort, "high")
	require.Equal(t,
		stringConfigValues(codexEffortValues),
		[]acp.SessionConfigSelectOption(*suppressed[2].Select.Options.Ungrouped),
		"the effort menu falls back to the harness vocabulary, not the preset's",
	)
}

// TestNativeModelMenuFollowsALoginWithinTheSession pins that the rule reads the
// account rather than remembering it: a session that started signed out
// publishes the presets as soon as the app-server holds a credential, and drops
// them again when it stops.
func TestNativeModelMenuFollowsALoginWithinTheSession(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	client.setRoute(codex.ProviderRoute{ProviderID: authProviderOpenAI}, nil)
	client.setAccount(codex.Account{})
	agent := NewAgent(
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
		WithAmbientEnvironment(map[string]string{"HOME": t.TempDir()}),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest(absTestPath("tmp", "project")))
	require.NoError(t, err)
	require.Equal(t,
		[]acp.SessionConfigValueId{"gpt-initial"},
		configOptionValueIDs(t, resp.ConfigOptions, configModel),
	)

	session := agent.activeSession(resp.SessionId)
	require.NotNil(t, session)

	client.setAccount(codex.Account{AuthMode: codex.AuthModeAPIKey})
	require.Equal(t,
		[]acp.SessionConfigValueId{"gpt-initial", "gpt-other"},
		configOptionValueIDs(t, agent.sessionConfigOptions(ctx, session, modelList(ctx, client)), configModel),
	)

	client.setAccount(codex.Account{})
	require.Equal(t,
		[]acp.SessionConfigValueId{"gpt-initial"},
		configOptionValueIDs(t, agent.sessionConfigOptions(ctx, session, modelList(ctx, client)), configModel),
	)
}

// TestNativeModelMenuNeedsAnAnsweredAccountRead pins that an account read that
// fails is not an account: the double answers with a signed-in account beside
// its error, and the presets still stay out of the menu.
func TestNativeModelMenuNeedsAnAnsweredAccountRead(t *testing.T) {
	ctx := context.Background()
	client := &errorCodexClient{spyCodexClient: newSpyCodexClient(), accountErr: errors.New("account")}
	client.setRoute(codex.ProviderRoute{ProviderID: authProviderOpenAI}, nil)
	agent := NewAgent(
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
		WithAmbientEnvironment(map[string]string{"HOME": t.TempDir()}),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest(absTestPath("tmp", "project")))
	require.NoError(t, err)
	require.Equal(t,
		[]acp.SessionConfigValueId{"gpt-initial"},
		configOptionValueIDs(t, resp.ConfigOptions, configModel),
	)
}

// TestNativeModelMenuNeedsARouteReader pins the client that cannot answer where
// its requests go: without that answer the presets are not published.
func TestNativeModelMenuNeedsARouteReader(t *testing.T) {
	ctx := context.Background()
	client := &routelessCodexClient{Client: newSpyCodexClient()}
	agent := NewAgent(
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
		WithAmbientEnvironment(map[string]string{"HOME": t.TempDir()}),
	)

	resp, err := agent.NewSession(ctx, NewSessionRequest(absTestPath("tmp", "project")))
	require.NoError(t, err)
	require.Equal(t,
		[]acp.SessionConfigValueId{"gpt-initial"},
		configOptionValueIDs(t, resp.ConfigOptions, configModel),
	)
}

func configOptionIDs(options []acp.SessionConfigOption) []acp.SessionConfigId {
	ids := make([]acp.SessionConfigId, 0, len(options))
	for _, option := range options {
		if option.Select != nil {
			ids = append(ids, option.Select.Id)
		}
	}

	return ids
}

func configOptionValueIDs(
	t *testing.T,
	options []acp.SessionConfigOption,
	id acp.SessionConfigId,
) []acp.SessionConfigValueId {
	t.Helper()

	for _, option := range options {
		if option.Select == nil || option.Select.Id != id {
			continue
		}

		require.NotNil(t, option.Select.Options.Ungrouped)

		values := make([]acp.SessionConfigValueId, 0, len(*option.Select.Options.Ungrouped))
		for _, value := range *option.Select.Options.Ungrouped {
			values = append(values, value.Value)
		}

		return values
	}

	require.Failf(t, "missing config option", "config option %q is absent from %#v", id, options)

	return nil
}
