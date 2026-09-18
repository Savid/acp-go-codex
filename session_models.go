package codexacp

import (
	"context"
	"slices"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/wire"
)

const (
	configModel       acp.SessionConfigId = "model"
	configMode        acp.SessionConfigId = "mode"
	configEffort      acp.SessionConfigId = "effort"
	configServiceTier acp.SessionConfigId = "service_tier"
	configPersonality acp.SessionConfigId = "personality"

	configTypeSelect = "select"

	modeDefault  = "default"
	modePlan     = "plan"
	effortMedium = "medium"

	// configCategoryModelConfig is the category of the model parameters
	// listed beside the model and effort selectors.
	configCategoryModelConfig acp.SessionConfigOptionCategory = "model_config"
)

// Host-facing menus for values the app-server does not enumerate. A value
// outside one of these menus still travels to Codex, which owns resolution;
// the collaboration mode is the adapter's own closed set and is not one of
// them.
var (
	effortMenu      = []string{"none", "minimal", "low", effortMedium, "high", "xhigh"}
	serviceTierMenu = []string{"auto", "default", "flex", "priority"}
	personalityMenu = []string{"none", "friendly", "pragmatic"}
)

// configOptions renders the session's select options: the model catalog, the
// collaboration mode, the effort menu of the selected model, and the tier and
// personality menus while set.
func (s *session) configOptions() []acp.SessionConfigOption {
	s.mu.Lock()
	rt := s.rt
	model := s.model
	mode := s.mode
	effort := s.effort
	tier := s.serviceTier
	personality := s.personality
	s.mu.Unlock()

	var models []codex.Model
	if rt != nil {
		models = rt.models
	}

	options := make([]acp.SessionConfigOption, 0, 5)

	if values := modelSelectOptions(model, models, s.agent.options.ConfiguredModels); len(values) > 0 {
		options = append(options, selectOption(configModel, "Model", acp.SessionConfigOptionCategoryModel, firstNonEmpty(model, modeDefault), values))
	}

	options = append(options, selectOption(configMode, "Mode", acp.SessionConfigOptionCategoryMode, mode, acp.SessionConfigSelectOptionsUngrouped{
		{Name: "Default", Value: modeDefault},
		{Name: "Plan", Value: modePlan},
	}))

	if current, values := effortSelectOptions(model, effort, models); len(values) > 0 {
		options = append(options, selectOption(configEffort, "Effort", acp.SessionConfigOptionCategoryThoughtLevel, current, values))
	}

	if tier != "" {
		options = append(options, selectOption(configServiceTier, "Service Tier", configCategoryModelConfig, tier, menuOptions(serviceTierMenu, tier)))
	}

	if personality != "" {
		options = append(options, selectOption(configPersonality, "Personality", configCategoryModelConfig, personality, menuOptions(personalityMenu, personality)))
	}

	return options
}

func selectOption(id acp.SessionConfigId, name string, category acp.SessionConfigOptionCategory, current string, values acp.SessionConfigSelectOptionsUngrouped) acp.SessionConfigOption {
	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
		Id:           id,
		Name:         name,
		Type:         configTypeSelect,
		Category:     &category,
		CurrentValue: acp.SessionConfigValueId(current),
		Options:      acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}
}

// modelSelectOptions lists the native catalog, then host-listed ids the
// catalog lacks, then the current model when nothing else names it.
func modelSelectOptions(model string, models []codex.Model, hostListed []string) acp.SessionConfigSelectOptionsUngrouped {
	rows := make([]wire.ModelRow, 0, len(models))
	for index := range models {
		info := &models[index]

		meta := map[string]any{}
		if info.ContextWindow > 0 {
			meta["contextWindow"] = info.ContextWindow
		}

		if len(info.ReasoningEfforts) > 0 {
			meta["supportedEffortLevels"] = slices.Clone(info.ReasoningEfforts)
		}

		rows = append(rows, wire.ModelRow{ID: info.ID, Name: info.Name, Description: info.Description, Meta: meta})
	}

	return wire.ModelSelectOptions(vendor, model, rows, hostListed)
}

// effortSelectOptions renders the selected model's effort menu, falling back
// to the fixed menu once an effort is set.
func effortSelectOptions(model string, effort string, models []codex.Model) (string, acp.SessionConfigSelectOptionsUngrouped) {
	for index := range models {
		if models[index].ID != model || len(models[index].ReasoningEfforts) == 0 {
			continue
		}

		current := firstNonEmpty(effort, models[index].DefaultReasoningEffort)

		return current, menuOptions(models[index].ReasoningEfforts, current)
	}

	if effort == "" {
		return "", nil
	}

	return effort, menuOptions(effortMenu, effort)
}

func menuOptions(menu []string, current string) acp.SessionConfigSelectOptionsUngrouped {
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(menu)+1)

	for _, value := range menu {
		values = append(values, acp.SessionConfigSelectOption{Name: value, Value: acp.SessionConfigValueId(value)})
	}

	if current != "" && !slices.Contains(menu, current) {
		values = append(values, acp.SessionConfigSelectOption{Name: current, Value: acp.SessionConfigValueId(current)})
	}

	return values
}

// setConfigOption applies one select value while no turn is in flight. Values
// forward to the next turn/start unchanged, except mode, which is the
// adapter's own two-value menu; effort and personality reject an empty value.
func (s *session) setConfigOption(ctx context.Context, configID acp.SessionConfigId, value string) ([]acp.SessionConfigOption, error) {
	if err := s.admissionError(); err != nil {
		return nil, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return nil, err
	}
	defer release()

	s.mu.Lock()

	switch configID {
	case configModel:
		s.model = value
		s.contextWindow = 0

		if s.rt != nil {
			s.contextWindow = catalogContextWindow(s.rt.models, value)
		}
	case configMode:
		if value != modeDefault && value != modePlan {
			s.mu.Unlock()

			return nil, wire.Unsupported("value")
		}

		s.mode = value
	case configEffort:
		if value == "" {
			s.mu.Unlock()

			return nil, wire.Unsupported("value")
		}

		s.effort = value
	case configServiceTier:
		s.serviceTier = value
	case configPersonality:
		if value == "" {
			s.mu.Unlock()

			return nil, wire.Unsupported("value")
		}

		s.personality = value
	default:
		s.mu.Unlock()

		return nil, wire.Unsupported("configId")
	}

	s.mu.Unlock()

	if err := s.commitMirror(ctx); err != nil {
		return nil, wire.InternalFailure(vendor, "")
	}

	options := s.configOptions()

	_ = s.emit(ctx, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}})

	return options, nil
}
