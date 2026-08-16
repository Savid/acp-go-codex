package codexacp

import (
	"os"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
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
}

// TestLifecycleMetaRejectionsCarryTheUniformUnsupportedShape pins every
// _meta.codex rejection to invalid params whose data is exactly
// {"error":"unsupported","field":<json path>} and nothing else. An unknown
// field and a wrong-typed one answer alike: both name a value this adapter does
// not accept at the path the caller wrote it, and neither has a shape of its
// own to report.
func TestLifecycleMetaRejectionsCarryTheUniformUnsupportedShape(t *testing.T) {
	options := func(values map[string]any) map[string]any {
		return map[string]any{codexMetaKey: map[string]any{metaOptionsKey: values}}
	}

	for name, test := range map[string]struct {
		meta  map[string]any
		field string
	}{
		"vendor block is not an object":   {meta: map[string]any{codexMetaKey: "bad"}, field: "_meta.codex"},
		"unknown vendor key":              {meta: map[string]any{codexMetaKey: map[string]any{"unexpected": true}}, field: "_meta.codex.unexpected"},
		"options is not an object":        {meta: map[string]any{codexMetaKey: map[string]any{metaOptionsKey: "bad"}}, field: "_meta.codex.options"},
		"unknown option key":              {meta: options(map[string]any{"old": true}), field: "_meta.codex.options.old"},
		"rawEvent is not an object":       {meta: map[string]any{codexMetaKey: map[string]any{rawEventKey: "bad"}}, field: "_meta.codex.rawEvent"},
		"rawEvent enabled is not boolean": {meta: map[string]any{codexMetaKey: map[string]any{rawEventKey: map[string]any{rawEventEnabledKey: "yes"}}}, field: "_meta.codex.rawEvent.enabled"},
		"unknown rawEvent key":            {meta: map[string]any{codexMetaKey: map[string]any{rawEventKey: map[string]any{"extra": true}}}, field: "_meta.codex.rawEvent.extra"},
		"model is not a string":           {meta: options(map[string]any{metaModelKey: 42}), field: "_meta.codex.options.model"},
		"serviceTier is not a string":     {meta: options(map[string]any{metaServiceTierKey: 42}), field: "_meta.codex.options.serviceTier"},
		"effort is not a string":          {meta: options(map[string]any{metaEffortKey: 42}), field: "_meta.codex.options.effort"},
		"effort is not an accepted value": {meta: options(map[string]any{metaEffortKey: "bad"}), field: "_meta.codex.options.effort"},
		"personality is not a string":     {meta: options(map[string]any{metaPersonalityKey: 42}), field: "_meta.codex.options.personality"},
		"personality is not accepted":     {meta: options(map[string]any{metaPersonalityKey: "bad"}), field: "_meta.codex.options.personality"},
		"approval mode is not a string":   {meta: options(map[string]any{metaMCPToolApprovalModeKey: 42}), field: "_meta.codex.options.mcpToolApprovalMode"},
		"approval mode is not accepted":   {meta: options(map[string]any{metaMCPToolApprovalModeKey: "bad"}), field: "_meta.codex.options.mcpToolApprovalMode"},
		"env is not an object":            {meta: options(map[string]any{metaEnvKey: "bad"}), field: "_meta.codex.options.env"},
		"env value is not a string":       {meta: options(map[string]any{metaEnvKey: map[string]any{"A": 1}}), field: "_meta.codex.options.env.A"},
		"env claims a reserved key":       {meta: options(map[string]any{metaEnvKey: map[string]string{managedCodexHomeEnv: "/x"}}), field: "_meta.codex.options.env." + managedCodexHomeEnv},
		"output schema is not an object":  {meta: options(map[string]any{metaOutputSchemaKey: "bad"}), field: outputSchemaConfigPath},
		"output schema is empty":          {meta: options(map[string]any{metaOutputSchemaKey: map[string]any{}}), field: outputSchemaConfigPath},
		"output schema is not encodable":  {meta: options(map[string]any{metaOutputSchemaKey: map[string]any{"bad": func() {}}}), field: outputSchemaConfigPath},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sessionMetaFromLifecycle(test.meta)
			requireInvalidParamsField(t, err, test.field)
		})
	}
}

func TestLifecycleMetaValidatesExtraPathDirs(t *testing.T) {
	absolute := t.TempDir()
	second := t.TempDir()

	for _, value := range []any{
		[]string{absolute, second, absolute},
		[]any{absolute, second, absolute},
	} {
		parsed, err := sessionMetaFromLifecycle(map[string]any{
			codexMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: value}},
		})
		require.NoError(t, err)
		require.Equal(t, []string{absolute, second, absolute}, parsed.ExtraPathDirs)
	}

	tests := []struct {
		name  string
		value any
		field string
	}{
		{name: "wrong list type", value: "bad", field: "_meta.codex.options.extraPathDirs"},
		{name: "wrong element type", value: []any{absolute, 1}, field: "_meta.codex.options.extraPathDirs[1]"},
		{name: "empty", value: []string{absolute, ""}, field: "_meta.codex.options.extraPathDirs[1]"},
		{name: "relative", value: []string{"relative"}, field: "_meta.codex.options.extraPathDirs[0]"},
		{name: "separator", value: []string{absolute + string(os.PathListSeparator) + second}, field: "_meta.codex.options.extraPathDirs[0]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := sessionMetaFromLifecycle(map[string]any{
				codexMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: test.value}},
			})
			requireInvalidParamsField(t, err, test.field)
		})
	}

	input := []string{absolute, second}
	parsed, err := sessionMetaFromLifecycle(CodexOptions{ExtraPathDirs: input}.Meta())
	require.NoError(t, err)
	input[0] = "/caller/mutated"
	require.Equal(t, absolute, parsed.ExtraPathDirs[0])
}

func TestLifecycleMetaRejectsRawPATH(t *testing.T) {
	_, err := sessionMetaFromLifecycle(CodexOptions{Env: map[string]string{"PATH": "/operation/bin"}}.Meta())
	requireInvalidParamsField(t, err, "_meta.codex.options.env.PATH")

	original := caseInsensitiveEnvKeys
	caseInsensitiveEnvKeys = true
	t.Cleanup(func() { caseInsensitiveEnvKeys = original })
	_, err = sessionMetaFromLifecycle(CodexOptions{Env: map[string]string{"Path": "/operation/bin"}}.Meta())
	requireInvalidParamsField(t, err, "_meta.codex.options.env.Path")
}

// requireInvalidParamsField asserts the whole data object, not just the field
// path: the uniform unsupported-field error carries exactly two keys, so a
// third one is itself a conformance failure.
func requireInvalidParamsField(t *testing.T, err error, field string) {
	t.Helper()

	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32602, requestErr.Code)
	require.Equal(t, map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: field,
	}, requestErr.Data)
}

func TestValidateSchemaObjectRejectsUnmarshalable(t *testing.T) {
	if err := validateSchemaObject(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unmarshalable output schema validated")
	}
}
