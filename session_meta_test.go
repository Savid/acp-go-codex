package codexacp

import (
	"testing"
)

func TestSessionMetaStructuredOutputValidation(t *testing.T) {
	schema := map[string]any{"type": "object"}
	meta := CodexOptions{
		Model:          "gpt",
		Mode:           modePlan,
		Effort:         "medium",
		ServiceTier:    "flex",
		Personality:    "friendly",
		Env:            map[string]string{"A": "B"},
		ApprovalPolicy: "never",
		SandboxPolicy:  map[string]any{"type": "workspaceWrite"},
		OutputSchema:   schema,
	}.Meta()
	parsed, err := sessionMetaFromLifecycle(meta)
	if err != nil {
		t.Fatalf("sessionMetaFromLifecycle returned error: %v", err)
	}
	if parsed.Model != "gpt" || parsed.Mode != modePlan || parsed.ReasoningEffort != "medium" || parsed.OutputSchema == nil {
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
		{codexMetaKey: map[string]any{"options": map[string]any{"outputSchema": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"mode": 1}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"mode": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"effort": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"personality": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"env": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"env": map[string]any{"A": 1}}}},
	}
	for _, tc := range cases {
		if _, err := sessionMetaFromLifecycle(tc); err == nil {
			t.Fatalf("expected error for %#v", tc)
		}
	}
}

func TestInitialSessionMode(t *testing.T) {
	if got := initialSessionMode(modePlan, modeDefault); got != modePlan {
		t.Fatalf("initial mode = %q, want plan", got)
	}
	if got := initialSessionMode("", modePlan); got != modePlan {
		t.Fatalf("default initial mode = %q, want plan", got)
	}
	if got := initialSessionMode("", "bad"); got != modeDefault {
		t.Fatalf("invalid default initial mode = %q, want default", got)
	}
}

func TestValidateSchemaObjectRejectsUnmarshalable(t *testing.T) {
	if err := validateSchemaObject(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unmarshalable output schema validated")
	}
}
