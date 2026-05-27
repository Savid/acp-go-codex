package codexacp

import (
	"testing"
)

func TestSessionMetaStructuredOutputValidation(t *testing.T) {
	schema := map[string]any{"type": "object"}
	meta := CodexOptions{Model: "gpt", Effort: "medium", ServiceTier: "flex", Personality: "friendly", OutputSchema: schema}.Meta()
	parsed, err := sessionMetaFromLifecycle(meta)
	if err != nil {
		t.Fatalf("sessionMetaFromLifecycle returned error: %v", err)
	}
	if parsed.Model != "gpt" || parsed.ReasoningEffort != "medium" || parsed.OutputSchema == nil {
		t.Fatalf("parsed meta = %#v", parsed)
	}

	cases := []map[string]any{
		{codexMetaKey: map[string]any{"options": map[string]any{"outputSchema": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"effort": "bad"}}},
		{codexMetaKey: map[string]any{"options": map[string]any{"personality": "bad"}}},
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
