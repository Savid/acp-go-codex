package codexacp

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

var (
	// These values are host-facing menus, not adapter allowlists. A non-empty
	// selection outside a menu still travels to Codex, which owns resolution.
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

func (a *Agent) sessionConfigOptions(ctx context.Context, session *session, models []codex.Model) []acp.SessionConfigOption {
	session.mu.Lock()
	model := session.model
	mode := session.mode
	effort := session.reasoningEffort
	tier := session.serviceTier
	personality := session.personality
	cwd := session.cwd
	provider := session.modelProvider
	client := session.client
	session.mu.Unlock()

	native := a.nativeModelCatalog(ctx, client, provider, cwd)

	return codexConfigOptions(model, mode, effort, tier, personality, models, native)
}

func (a *Agent) sessionUnstableConfigOptions(ctx context.Context, session *session, models []codex.Model) []acp.UnstableSessionConfigOption {
	options := a.sessionConfigOptions(ctx, session, models)

	out := make([]acp.UnstableSessionConfigOption, 0, len(options))
	for _, option := range options {
		out = append(out, unstableConfigOption(option))
	}

	return out
}

func codexConfigOptions(model string, mode acp.SessionModeId, effort string, tier string, personality string, models []codex.Model, nativeCatalog bool) []acp.SessionConfigOption {
	var options []acp.SessionConfigOption

	// Every menu built from model/list describes the same presets, so the model
	// menu, the effort menu, and the published model metadata are trustworthy
	// together or not at all.
	if !nativeCatalog {
		models = nil
	}

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

// modelConfigValues builds the model menu from the models this session can
// reach plus its own current model. Selection stays with Codex: a value absent
// from the menu still travels to the native harness.
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

// nativeAuthEnvAPIKey is the one variable Codex signs its own requests with
// when no account is stored: it becomes the request's bearer, while the other
// credential-shaped names in its environment authorize nothing. An account read
// reports none of them, so a process authenticated this way is only visible
// here.
const nativeAuthEnvAPIKey = "CODEX_API_KEY" // #nosec G101 -- variable name, not a credential.

// nativeModelCatalogTimeout bounds the native reads the menu depends on. They
// are local app-server calls, and they run inside the session's turn slot on a
// config-option change, where a prompt arriving behind them is refused rather
// than queued.
const nativeModelCatalogTimeout = 2 * time.Second

// nativeModelCatalog reports whether Codex's built-in model presets describe
// models this session can actually reach. `model/list` answers from the CLI
// build rather than from the endpoint in use, so the presets are published only
// when requests reach Codex's own OpenAI provider at its own endpoint, signed
// by a credential that exists. Routed through another provider, pointed
// elsewhere, or signed out, the presets name model ids the endpoint never
// serves, and the menu carries the session's current model alone.
//
// Routing is asked twice because the two answers age differently: the thread
// carries the provider it started on, and the workspace configuration carries
// the provider a turn started now would use. Either naming a provider other
// than OpenAI withdraws the presets, so an edited route is honoured in both
// directions. Configuration is layered — seed files, the operator's own config,
// process overrides, the launch environment — and only the app-server resolves
// it, so a route it will not report withdraws them too.
func (a *Agent) nativeModelCatalog(ctx context.Context, client codex.Client, provider, cwd string) bool {
	if provider != "" && provider != authProviderOpenAI {
		return false
	}

	reader, ok := client.(codex.ProviderRouteClient)
	if !ok {
		return false
	}

	readCtx, cancel := context.WithTimeout(ctx, nativeModelCatalogTimeout)
	defer cancel()

	route, err := reader.ReadProviderRoute(readCtx, cwd)
	if err != nil {
		a.log.DebugContext(ctx, "native model catalog withheld: provider route unavailable")

		return false
	}

	if route.ProviderID != authProviderOpenAI || route.Custom {
		return false
	}

	// An account the app-server cannot report is not evidence of a credential,
	// but neither is it evidence against one: environment auth is invisible to
	// this read, so the environment still answers for itself.
	account, err := client.AccountRead(readCtx)
	if err != nil {
		a.log.DebugContext(ctx, "native model catalog falling back to environment auth: account unavailable")
	} else if nativeOpenAIAccount(account) {
		return true
	}

	return a.nativeEnvironmentAuth()
}

// nativeOpenAIAccount reports whether a read account signs OpenAI requests.
// The account mode names the credential's own provider, and Codex brokers
// accounts it does not route to OpenAI.
func nativeOpenAIAccount(account codex.Account) bool {
	return account.AuthMode == codex.AuthModeChatGPT || account.AuthMode == codex.AuthModeAPIKey
}

// nativeEnvironmentAuth reports whether the shared app-server's environment
// carries a credential of its own. The static launch block is consulted the way
// the launcher applies it: an entry it names wins over the ambient block even
// when its value is empty, because that is how an operator withdraws an
// inherited key.
func (a *Agent) nativeEnvironmentAuth() bool {
	key := codex.EnvironmentKey(nativeAuthEnvAPIKey)
	if value, ok := a.options.Env[key]; ok {
		return value != ""
	}

	return a.nativeAmbientEnvironment()[key] != ""
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

// selectedModelImageSupport answers from Codex's own model list, which
// describes the presets the CLI build ships. The caller establishes first that
// those presets describe this session; where they do not, a model id colliding
// with one proves nothing about the model in use and the decision is left to
// Codex.
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

	values := stringConfigValues(codexEffortValues)
	if !slices.ContainsFunc(values, func(option acp.SessionConfigSelectOption) bool {
		return string(option.Value) == currentEffort
	}) {
		values = append(values, acp.SessionConfigSelectOption{
			Name:  currentEffort,
			Value: acp.SessionConfigValueId(currentEffort),
		})
	}

	return acp.SessionConfigValueId(currentEffort), values
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
		if mode == "" {
			return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
		}

		session.mu.Lock()
		session.mode = mode
		session.updatedAt = nowRFC3339()
		session.mu.Unlock()
	case configEffort:
		if params.Value == "" {
			return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
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
		if params.Value == "" {
			return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
		}

		session.mu.Lock()
		session.personality = string(params.Value)
		session.updatedAt = nowRFC3339()
		session.mu.Unlock()
	default:
		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldConfigID)
	}

	models := modelList(ctx, session.client)

	options := a.sessionConfigOptions(ctx, session, models)
	if err := session.emitUpdates(ctx, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}}); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}
