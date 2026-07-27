package codexacp

import (
	"context"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

var (
	codexEffortValues = []string{effortValueNone, "minimal", effortValueLow, effortValueMedium, effortValueHigh, "xhigh"}
	codexTierValues   = []string{tierValueAuto, valueDefault, tierValueFlex, tierValuePriority}
	codexPersonality  = []string{effortValueNone, personalityFriendly, personalityPragmatic}
)

type imageInputSupport uint8

const (
	imageInputUnknown imageInputSupport = iota
	imageInputUnsupported
	imageInputSupported
)

const (
	configCategoryModelConfig acp.SessionConfigOptionCategory = "model_config"

	effortValueHigh     = "high"
	personalityFriendly = "friendly"

	effortValueNone      = "none"
	effortValueLow       = "low"
	effortValueMedium    = "medium"
	tierValueAuto        = "auto"
	tierValueFlex        = "flex"
	tierValuePriority    = "priority"
	personalityPragmatic = "pragmatic"
)

func sessionConfigOptions(session *session, models []codex.Model) []acp.SessionConfigOption {
	session.mu.Lock()
	model := session.model
	mode := session.mode
	effort := session.reasoningEffort
	tier := session.serviceTier
	personality := session.personality
	session.mu.Unlock()

	return codexConfigOptions(model, mode, effort, tier, personality, models)
}

func sessionUnstableConfigOptions(session *session, models []codex.Model) []acp.UnstableSessionConfigOption {
	options := sessionConfigOptions(session, models)

	out := make([]acp.UnstableSessionConfigOption, 0, len(options))
	for _, option := range options {
		out = append(out, unstableConfigOption(option))
	}

	return out
}

func codexConfigOptions(model string, mode acp.SessionModeId, effort string, tier string, personality string, models []codex.Model) []acp.SessionConfigOption {
	var options []acp.SessionConfigOption

	if model == "" {
		model = valueDefault
	}

	if mode == "" {
		mode = modeDefault
	}

	if values := modelConfigValues(model, models); len(values) > 0 {
		options = append(options, selectConfigOption(configModel, "Model", acp.SessionConfigOptionCategoryModel, acp.SessionConfigValueId(model), values))
	}

	options = append(options, selectConfigOption(configMode, "Mode", acp.SessionConfigOptionCategoryMode, acp.SessionConfigValueId(mode), []acp.SessionConfigSelectOption{
		{Name: "Default", Value: acp.SessionConfigValueId(modeDefault)},
		{Name: "Plan", Value: acp.SessionConfigValueId(modePlan)},
	}))
	if currentEffort, values := effortConfigValues(model, effort, models); len(values) > 0 {
		options = append(options, selectConfigOption(configEffort, "Effort", acp.SessionConfigOptionCategoryThoughtLevel, currentEffort, values))
	}

	if tier != "" {
		options = append(options, selectConfigOption(configServiceTier, "Service Tier", configCategoryModelConfig, acp.SessionConfigValueId(tier), stringConfigValues(codexTierValues)))
	}

	if personality != "" {
		options = append(options, selectConfigOption(configPersonality, "Personality", configCategoryModelConfig, acp.SessionConfigValueId(personality), stringConfigValues(codexPersonality)))
	}

	return options
}

func modelConfigValues(current string, models []codex.Model) []acp.SessionConfigSelectOption {
	seen := map[string]struct{}{}

	values := make([]acp.SessionConfigSelectOption, 0, len(models)+1)
	for index := range models {
		model := &models[index]

		id := firstNonEmpty(model.ID, model.Name)
		if id == "" {
			continue
		}

		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}
		values = append(values, acp.SessionConfigSelectOption{
			Name:        firstNonEmpty(model.Name, id),
			Value:       acp.SessionConfigValueId(id),
			Description: stringPtrIfNotEmpty(model.Description),
			Meta:        modelMeta(*model, id),
		})
	}

	if current != "" {
		if _, ok := seen[current]; !ok {
			values = append(values, acp.SessionConfigSelectOption{Name: current, Value: acp.SessionConfigValueId(current)})
		}
	}

	return values
}

func modelMeta(model codex.Model, id string) map[string]any {
	codexMeta := map[string]any{"modelId": id}
	if model.Context > 0 {
		codexMeta["contextWindow"] = model.Context
	}

	efforts := make([]string, 0, len(model.ReasoningEfforts))
	for _, effort := range model.ReasoningEfforts {
		if effort.ID != "" {
			efforts = append(efforts, effort.ID)
		}
	}

	if len(efforts) > 0 {
		codexMeta["supportedEffortLevels"] = efforts
	}

	return map[string]any{codexMetaKey: codexMeta}
}

func selectedModelImageSupport(models []codex.Model, selected string) imageInputSupport {
	model := modelByID(selected, models)
	if model == nil || len(model.InputModalities) == 0 {
		return imageInputUnknown
	}

	for _, modality := range model.InputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return imageInputSupported
		}
	}

	return imageInputUnsupported
}

func effortConfigValues(currentModel string, currentEffort string, models []codex.Model) (acp.SessionConfigValueId, []acp.SessionConfigSelectOption) {
	if model := modelByID(currentModel, models); model != nil && len(model.ReasoningEfforts) > 0 {
		seen := map[string]struct{}{}

		values := make([]acp.SessionConfigSelectOption, 0, len(model.ReasoningEfforts)+1)
		for _, effort := range model.ReasoningEfforts {
			if effort.ID == "" {
				continue
			}

			if _, ok := seen[effort.ID]; ok {
				continue
			}

			seen[effort.ID] = struct{}{}
			values = append(values, acp.SessionConfigSelectOption{
				Name:        effort.ID,
				Value:       acp.SessionConfigValueId(effort.ID),
				Description: stringPtrIfNotEmpty(effort.Description),
				Meta:        effort.Raw,
			})
		}

		selected := firstNonEmpty(currentEffort, model.DefaultReasoningEffort)
		if selected != "" {
			if _, ok := seen[selected]; !ok {
				values = append(values, acp.SessionConfigSelectOption{Name: selected, Value: acp.SessionConfigValueId(selected)})
			}
		}

		return acp.SessionConfigValueId(selected), values
	}

	if currentEffort == "" {
		return "", nil
	}

	return acp.SessionConfigValueId(currentEffort), stringConfigValues(codexEffortValues)
}

func modelByID(id string, models []codex.Model) *codex.Model {
	for i := range models {
		if firstNonEmpty(models[i].ID, models[i].Name) == id {
			return &models[i]
		}
	}

	return nil
}

func stringConfigValues(values []string) []acp.SessionConfigSelectOption {
	out := make([]acp.SessionConfigSelectOption, 0, len(values))
	for _, value := range values {
		out = append(out, acp.SessionConfigSelectOption{Name: value, Value: acp.SessionConfigValueId(value)})
	}

	return out
}

func stringPtrIfNotEmpty(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

func selectConfigOption(id acp.SessionConfigId, name string, category acp.SessionConfigOptionCategory, current acp.SessionConfigValueId, values []acp.SessionConfigSelectOption) acp.SessionConfigOption {
	ungrouped := acp.SessionConfigSelectOptionsUngrouped(values)

	option := acp.SessionConfigOption{
		Select: &acp.SessionConfigOptionSelect{
			Id:           id,
			Name:         name,
			Type:         configTypeSelect,
			CurrentValue: current,
			Options: acp.SessionConfigSelectOptions{
				Ungrouped: &ungrouped,
			},
		},
	}
	if category != "" {
		option.Select.Category = &category
	}

	return option
}

func unstableConfigOption(option acp.SessionConfigOption) acp.UnstableSessionConfigOption {
	if option.Select != nil {
		value := acp.UnstableSessionConfigOptionSelect(*option.Select)

		return acp.UnstableSessionConfigOption{Select: &value}
	}

	if option.Boolean != nil {
		value := acp.UnstableSessionConfigOptionBoolean(*option.Boolean)

		return acp.UnstableSessionConfigOption{Boolean: &value}
	}

	return acp.UnstableSessionConfigOption{}
}

func (a *Agent) setSessionConfigValue(ctx context.Context, params *acp.SetSessionConfigOptionValueId) (acp.SetSessionConfigOptionResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	releaseTurn, err := session.acquireTurn(ctx)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	defer releaseTurn()

	switch params.ConfigId {
	case configModel:
		session.mu.Lock()
		session.model = string(params.Value)
		session.updatedAt = nowRFC3339()
		session.mu.Unlock()
	case configMode:
		mode := acp.SessionModeId(params.Value)
		if mode != modeDefault && mode != modePlan {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldConfigID: params.ConfigId, jsonFieldValue: params.Value})
		}

		session.mu.Lock()
		session.mode = mode
		session.updatedAt = nowRFC3339()
		session.mu.Unlock()
	case configEffort:
		if !validReasoningEffort(string(params.Value)) {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldConfigID: params.ConfigId, jsonFieldValue: params.Value})
		}

		session.mu.Lock()
		session.reasoningEffort = string(params.Value)
		session.updatedAt = nowRFC3339()
		session.mu.Unlock()
	case configServiceTier:
		session.mu.Lock()
		session.serviceTier = string(params.Value)
		session.updatedAt = nowRFC3339()
		session.mu.Unlock()
	case configPersonality:
		if !validPersonality(string(params.Value)) {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldConfigID: params.ConfigId, jsonFieldValue: params.Value})
		}

		session.mu.Lock()
		session.personality = string(params.Value)
		session.updatedAt = nowRFC3339()
		session.mu.Unlock()
	default:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldConfigID: params.ConfigId})
	}

	models := modelList(ctx, session.client)

	options := sessionConfigOptions(session, models)
	if err := session.emitUpdates(ctx, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}}); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}
