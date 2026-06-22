package codexacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestSessionConfigOptionsMutateTurnSettings(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	ctx := context.Background()
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project",
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

	_, err = agent.Prompt(ctx, TextPromptRequest(resp.SessionId, "hello"))
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
			t.Fatalf("legacy mode update emitted: %#v", update.Update.CurrentModeUpdate)
		}
	}
}

func TestNewSessionInitialMode(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(
		WithDefaultMode(modePlan),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession with default mode returned error: %v", err)
	}
	requireNoTopLevelConfigState(t, resp)
	modeConfig := findSelectConfig(resp.ConfigOptions, configMode)
	if modeConfig == nil || modeConfig.CurrentValue != acp.SessionConfigValueId(modePlan) {
		t.Fatalf("default mode config = %#v", modeConfig)
	}

	resp, err = agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionCodexOptions(NewCodexOptions(WithCodexMode(modeDefault)))))
	if err != nil {
		t.Fatalf("NewSession with meta mode returned error: %v", err)
	}
	requireNoTopLevelConfigState(t, resp)
	modeConfig = findSelectConfig(resp.ConfigOptions, configMode)
	if modeConfig == nil || modeConfig.CurrentValue != acp.SessionConfigValueId(modeDefault) {
		t.Fatalf("meta mode config = %#v", modeConfig)
	}
}

func TestSetSessionConfigOptionRejectsBadModeValue(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configMode, Value: "bad"}}); err == nil {
		t.Fatal("mode config accepted bad value")
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

	options := codexConfigOptions("gpt-5.5", modeDefault, "", "", "", models)
	if len(options) != 3 {
		t.Fatalf("config options = %#v", options)
	}
	modelOption := options[0].Select
	if modelOption == nil || modelOption.Id != configModel || modelOption.Category == nil || *modelOption.Category != acp.SessionConfigOptionCategoryModel {
		t.Fatalf("model option = %#v", modelOption)
	}
	modelValues := *modelOption.Options.Ungrouped
	if modelValues[0].Name != "GPT-5.5" || modelValues[0].Description == nil || *modelValues[0].Description != "Frontier model" || modelValues[0].Meta["displayName"] != "GPT-5.5" {
		t.Fatalf("model values = %#v", modelValues)
	}
	effortOption := options[2].Select
	if effortOption == nil || effortOption.Id != configEffort || effortOption.CurrentValue != "medium" {
		t.Fatalf("effort option = %#v", effortOption)
	}
	effortValues := *effortOption.Options.Ungrouped
	if len(effortValues) != 2 || effortValues[0].Description == nil || *effortValues[0].Description != "Fast" {
		t.Fatalf("effort values = %#v", effortValues)
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
	resp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
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

func findSelectConfig(options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionSelect {
	for _, option := range options {
		if option.Select != nil && option.Select.Id == id {
			return option.Select
		}
	}

	return nil
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
		t.Fatalf("response contains legacy models: %s", string(encoded))
	}
	if _, ok := object["modes"]; ok {
		t.Fatalf("response contains legacy modes: %s", string(encoded))
	}
}
