package codexacp

import (
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestSessionMetaStructuredOutputValidation(t *testing.T) {
	if err := validateLifecycleMeta(map[string]any{"other": true}); err != nil {
		t.Fatalf("unrelated lifecycle meta returned error: %v", err)
	}

	if err := validateLifecycleMeta(map[string]any{"github.com/savid/acp-go-codex": map[string]any{}}); err != nil {
		t.Fatalf("foreign module-path _meta namespace returned error instead of being ignored: %v", err)
	}

	schema := map[string]any{"type": "object"}
	meta := CodexOptions{
		Model:               "gpt",
		Env:                 map[string]string{"A": "B"},
		Effort:              "medium",
		ServiceTier:         "flex",
		Personality:         "friendly",
		ApprovalPolicy:      "never",
		SandboxPolicy:       map[string]any{"type": "workspaceWrite"},
		OutputSchema:        schema,
		MCPToolApprovalMode: "approve",
	}.Meta()
	parsed, err := sessionMetaFromLifecycle(meta)
	if err != nil {
		t.Fatalf("sessionMetaFromLifecycle returned error: %v", err)
	}
	if parsed.Model != "gpt" || parsed.Env["A"] != "B" || parsed.ReasoningEffort != "medium" || parsed.OutputSchema == nil || parsed.MCPToolApprovalMode != "approve" {
		t.Fatalf("parsed meta = %#v", parsed)
	}
	if parsed.ApprovalPolicy != "never" || asType[map[string]any](t, parsed.SandboxPolicy)["type"] != "workspaceWrite" {
		t.Fatalf("parsed Codex runtime options = %#v", parsed)
	}
	parsedAnyEnv, err := sessionMetaFromLifecycle(map[string]any{
		codexMetaKey: map[string]any{"options": map[string]any{"env": map[string]any{"C": "D"}}},
	})
	if err != nil || parsedAnyEnv.Env["C"] != "D" {
		t.Fatalf("parsed map env = %#v err=%v", parsedAnyEnv.Env, err)
	}

	cases := []map[string]any{
		{codexMetaKey: "bad"},
		{codexMetaKey: map[string]any{"options": "bad"}},
		{codexMetaKey: map[string]any{"options": map[string]any{"old": true}}},
		{codexMetaKey: map[string]any{"rawEvent": "bad"}},
		{codexMetaKey: map[string]any{"rawEvent": map[string]any{"enabled": "yes"}}},
		{codexMetaKey: map[string]any{"rawEvent": map[string]any{"extra": true}}},
		{codexMetaKey: map[string]any{"unexpected": true}},
		{codexMetaKey: map[string]any{"options": map[string]any{"outputSchema": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"outputSchema": map[string]any{}}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"effort": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"personality": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"mcpToolApprovalMode": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"env": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"env": map[string]any{"A": 1}}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"model": 42}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"effort": 42}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"serviceTier": 42}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"personality": 42}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"mcpToolApprovalMode": 42}}},
	}
	for _, tc := range cases {
		if _, err := sessionMetaFromLifecycle(tc); err == nil {
			t.Fatalf("expected error for %#v", tc)
		}
	}
}

func TestSessionMetaRejectsWrongTypedOptionValues(t *testing.T) {
	_, err := sessionMetaFromLifecycle(map[string]any{
		codexMetaKey: map[string]any{"options": map[string]any{"model": 42}},
	})

	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("wrong-typed model error = %#v, want -32602 invalid params", err)
	}
	if data, ok := reqErr.Data.(map[string]any); !ok || data["error"] != "unsupported" || data["field"] != "_meta.codex.options.model" {
		t.Fatalf("wrong-typed model data = %#v, want unsupported/_meta.codex.options.model", reqErr.Data)
	}
}

func TestValidateSchemaObjectRejectsUnmarshalable(t *testing.T) {
	if err := validateSchemaObject(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unmarshalable output schema validated")
	}
}
