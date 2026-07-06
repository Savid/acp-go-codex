package codexacp

import (
	"context"
	"encoding/json"
	"errors"
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
			t.Fatalf("removed mode update emitted: %#v", update.Update.CurrentModeUpdate)
		}
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

	options := codexConfigOptions("custom-model", "", "", "priority", "friendly", models)
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
	capabilities, _ := modelMeta["capabilities"].([]string)
	if len(capabilities) != 1 || capabilities[0] != "vision" {
		t.Fatalf("model capabilities = %#v", modelMeta["capabilities"])
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

func TestSetSessionConfigOptionErrorBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	if _, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest("missing", configModel, "gpt")); err == nil {
		t.Fatal("SetSessionConfigOption accepted missing session")
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{}); err == nil {
		t.Fatal("SetSessionConfigOption accepted empty request")
	}
	if _, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{
			SessionId: "missing",
			ConfigId:  configModel,
			Value:     true,
		},
	}); err == nil {
		t.Fatal("SetSessionConfigOption accepted boolean config")
	}
	if _, err := agent.SetSessionMode(ctx, acp.SetSessionModeRequest{}); err == nil {
		t.Fatal("SetSessionMode succeeded")
	}

	updateErr := errors.New("update failed")
	agent = NewAgent()
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: updateErr})
	session := newSession(agent, "session-1", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, newSpyCodexClient(), sessionMeta{})
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
