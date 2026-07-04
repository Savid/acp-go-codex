package codexacp

import (
	"testing"
)

func TestSessionMetaStructuredOutputValidation(t *testing.T) {
	if err := validateLifecycleMeta(map[string]any{"other": true}); err != nil {
		t.Fatalf("unrelated lifecycle meta returned error: %v", err)
	}

	schema := map[string]any{"type": "object"}
	meta := CodexOptions{
		Model:               "gpt",
		Effort:              "medium",
		ServiceTier:         "flex",
		Personality:         "friendly",
		Env:                 map[string]string{"A": "B"},
		ApprovalPolicy:      "never",
		SandboxPolicy:       map[string]any{"type": "workspaceWrite"},
		OutputSchema:        schema,
		MCPToolApprovalMode: "approve",
	}.Meta()
	parsed, err := sessionMetaFromLifecycle(meta)
	if err != nil {
		t.Fatalf("sessionMetaFromLifecycle returned error: %v", err)
	}
	if parsed.Model != "gpt" || parsed.ReasoningEffort != "medium" || parsed.OutputSchema == nil || parsed.MCPToolApprovalMode != "approve" {
		t.Fatalf("parsed meta = %#v", parsed)
	}
	if parsed.Env["A"] != "B" || parsed.ApprovalPolicy != "never" || parsed.SandboxPolicy.(map[string]any)["type"] != "workspaceWrite" {
		t.Fatalf("parsed Codex runtime options = %#v", parsed)
	}
	meta = map[string]any{codexMetaKey: map[string]any{"options": map[string]any{
		metaEnvKey:            map[string]any{"C": "D"},
		metaApprovalPolicyKey: map[string]any{"mode": "ask"},
		metaSandboxPolicyKey:  []any{"workspace-write"},
	}}}
	parsed, err = sessionMetaFromLifecycle(meta)
	if err != nil {
		t.Fatalf("sessionMetaFromLifecycle map env returned error: %v", err)
	}
	if parsed.Env["C"] != "D" {
		t.Fatalf("parsed map env = %#v", parsed.Env)
	}

	cases := []map[string]any{
		{"github.com/savid/acp-go-codex": map[string]any{}},
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
	}
	for _, tc := range cases {
		if _, err := sessionMetaFromLifecycle(tc); err == nil {
			t.Fatalf("expected error for %#v", tc)
		}
	}
}

func TestValidateSchemaObjectRejectsUnmarshalable(t *testing.T) {
	if err := validateSchemaObject(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unmarshalable output schema validated")
	}
}
