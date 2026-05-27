package codexacp

import (
	"encoding/json"
	"fmt"

	"github.com/coder/acp-go-sdk"
)

type sessionMeta struct {
	Model           string
	ReasoningEffort string
	ServiceTier     string
	Personality     string
	OutputSchema    any
	RawMessages     rawMessageConfig
}

func sessionMetaFromLifecycle(meta map[string]any) (sessionMeta, error) {
	codexOptions, err := codexOptionsFromMeta(meta)
	if err != nil {
		return sessionMeta{}, err
	}

	return sessionMeta{
		Model:           codexOptions.Model,
		ReasoningEffort: codexOptions.ReasoningEffort,
		ServiceTier:     codexOptions.ServiceTier,
		Personality:     codexOptions.Personality,
		OutputSchema:    codexOptions.OutputSchema,
		RawMessages:     rawMessageConfigFromMeta(meta),
	}, nil
}

type codexOptions struct {
	Model           string
	ReasoningEffort string
	ServiceTier     string
	Personality     string
	OutputSchema    any
}

func codexOptionsFromMeta(meta map[string]any) (codexOptions, error) {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)
	optionsMap, _ := codexMeta["options"].(map[string]any)
	if optionsMap == nil {
		return codexOptions{}, nil
	}

	options := codexOptions{}
	if model, _ := optionsMap["model"].(string); model != "" {
		options.Model = model
	}
	if effort, _ := optionsMap["effort"].(string); effort != "" {
		if !validReasoningEffort(effort) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.effort is unsupported")
		}
		options.ReasoningEffort = effort
	}
	if tier, _ := optionsMap["serviceTier"].(string); tier != "" {
		options.ServiceTier = tier
	}
	if personality, _ := optionsMap["personality"].(string); personality != "" {
		if !validPersonality(personality) {
			return codexOptions{}, fmt.Errorf("_meta.codex.options.personality is unsupported")
		}
		options.Personality = personality
	}
	if schema, ok := optionsMap["outputSchema"]; ok {
		if err := validateSchemaObject(schema); err != nil {
			return codexOptions{}, err
		}
		options.OutputSchema = cloneAny(schema)
	}

	return options, nil
}

func validateSchemaObject(schema any) error {
	obj, ok := schema.(map[string]any)
	if !ok || len(obj) == 0 {
		return fmt.Errorf("output schema must be a non-empty JSON object")
	}
	if _, err := json.Marshal(obj); err != nil {
		return fmt.Errorf("output schema must be JSON serializable: %w", err)
	}

	return nil
}

func validReasoningEffort(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func validPersonality(value string) bool {
	switch value {
	case "none", "friendly", "pragmatic":
		return true
	default:
		return false
	}
}

func sessionResponseMeta(snapshot sessionSnapshot) map[string]any {
	codexMeta := map[string]any{
		codexThreadIDMetaKey: snapshot.codexThreadID,
	}
	if snapshot.modelProvider != "" {
		codexMeta["modelProvider"] = snapshot.modelProvider
	}
	if snapshot.model != "" {
		codexMeta["model"] = snapshot.model
	}
	if snapshot.reasoningEffort != "" {
		codexMeta["effort"] = snapshot.reasoningEffort
	}
	if snapshot.serviceTier != "" {
		codexMeta["serviceTier"] = snapshot.serviceTier
	}
	if snapshot.personality != "" {
		codexMeta["personality"] = snapshot.personality
	}
	if len(snapshot.accountMeta) > 0 {
		codexMeta[codexAccountMetaKey] = cloneAnyMap(snapshot.accountMeta)
	}

	return map[string]any{
		// The reference docs intentionally advertise both the short provider
		// namespace and the package namespace for host lookup convenience.
		codexMetaKey:   codexMeta,
		packageMetaKey: cloneAnyMap(codexMeta),
	}
}

func modeState(mode acp.SessionModeId) *acp.SessionModeState {
	if mode == "" {
		mode = modeDefault
	}

	return &acp.SessionModeState{
		CurrentModeId: mode,
		AvailableModes: []acp.SessionMode{
			{Id: modeDefault, Name: "Default"},
			{Id: modePlan, Name: "Plan"},
		},
	}
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneAny(value)
	}

	return cloned
}

func cloneAnySlice(values []any) []any {
	if values == nil {
		return nil
	}
	cloned := make([]any, len(values))
	for i, value := range values {
		cloned[i] = cloneAny(value)
	}

	return cloned
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		return cloneAnySlice(typed)
	default:
		return value
	}
}
