package codex

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	rateLimitsProviderOpenAI   = "openai"
	rateLimitsDefaultPool      = "codex"
	rateLimitsWindowPrimary    = "primary"
	rateLimitsWindowSecondary  = "secondary"
	rateLimitsChatGPTBaseURL   = "https://chatgpt.com/backend-api"
	rateLimitsEnvOpenAIBaseURL = "OPENAI_BASE_URL"
)

// RateLimitWindow contains only measured native utilization and timing facts.
type RateLimitWindow struct {
	ID              string
	UsedPercent     float64
	DurationSeconds *int64
	ResetsAt        string
}

// RateLimitPool is one native allowance, independently of its window duration.
type RateLimitPool struct {
	ID       string
	Label    string
	PlanType string
	Windows  []RateLimitWindow
}

// RateLimitSnapshot records when the complete native response was received.
type RateLimitSnapshot struct {
	AccountID  string
	ObservedAt time.Time
	Pools      []RateLimitPool
}

// RateLimitsContextClient resolves native provider configuration without
// starting a thread or model turn. Only the relevant configuration is returned.
type RateLimitsContextClient interface {
	ReadRateLimitsContext(context.Context, string) (RateLimitsContext, error)
}

// RateLimitsContext identifies the native account reader's effective target.
type RateLimitsContext struct {
	ProviderID string
	Custom     bool
}

// ReadRateLimitsContext reads layered native config in the selected workspace.
func (c *AppServerClient) ReadRateLimitsContext(ctx context.Context, cwd string) (RateLimitsContext, error) {
	params := map[string]any{"includeLayers": false}
	if cwd != "" {
		params["cwd"] = cwd
	}

	var response struct {
		Config map[string]any `json:"config"`
	}
	if err := c.rpc.Call(ctx, "config/read", params, &response); err != nil {
		return RateLimitsContext{}, err
	}

	if response.Config == nil {
		return RateLimitsContext{}, errors.New("missing native quota configuration")
	}

	configuration := rateLimitsContextFromConfig(response.Config)

	environment, err := buildMergedEnv(c.options)
	if err != nil {
		return RateLimitsContext{}, err
	}

	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, rateLimitsEnvOpenAIBaseURL) {
			configuration.Custom = configuration.Custom || value != ""
		}
	}

	return configuration, nil
}

func rateLimitsContextFromConfig(config map[string]any) RateLimitsContext {
	provider := stringValue(config, "model_provider")
	if provider == "" {
		provider = rateLimitsProviderOpenAI // Codex's built-in default when no provider is selected.
	}

	// config/read includes Codex's resolved ChatGPT URL even without an override.
	chatGPTBaseURL := strings.TrimSpace(stringValue(config, "chatgpt_base_url"))
	custom := provider != rateLimitsProviderOpenAI || strings.TrimSpace(stringValue(config, "openai_base_url")) != "" ||
		chatGPTBaseURL != "" && chatGPTBaseURL != rateLimitsChatGPTBaseURL && chatGPTBaseURL != rateLimitsChatGPTBaseURL+"/"

	providerConfig := mapValue(mapValue(config, "model_providers"), provider)
	for _, key := range []string{"base_url", "env_key", "experimental_bearer_token", "auth", "http_headers", "env_http_headers"} {
		custom = custom || providerConfig[key] != nil
	}

	return RateLimitsContext{ProviderID: provider, Custom: custom}
}

func rateLimitSnapshotFromMap(response map[string]any) (RateLimitSnapshot, error) {
	out := RateLimitSnapshot{AccountID: stringValue(response, "accountId"), ObservedAt: time.Now().UTC(), Pools: []RateLimitPool{}}

	pools := mapValue(response, "rateLimitsByLimitId")
	if len(pools) == 0 {
		if snapshot := mapValue(response, "rateLimits"); snapshot != nil {
			id := stringValue(snapshot, "limitId")
			if strings.TrimSpace(id) == "" {
				id = rateLimitsDefaultPool
			}

			pools = map[string]any{id: snapshot}
		}
	}

	ids := make([]string, 0, len(pools))
	for id := range pools {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	for _, id := range ids {
		snapshot, ok := pools[id].(map[string]any)
		if !ok || strings.TrimSpace(id) == "" {
			return RateLimitSnapshot{}, errors.New("invalid native quota pool")
		}

		pool, err := rateLimitPoolFromMap(id, snapshot)
		if err != nil {
			return RateLimitSnapshot{}, err
		}

		if len(pool.Windows) != 0 {
			out.Pools = append(out.Pools, pool)
		}
	}

	return out, nil
}

func rateLimitPoolFromMap(id string, snapshot map[string]any) (RateLimitPool, error) {
	pool := RateLimitPool{ID: id, Label: strings.TrimSpace(stringValue(snapshot, "limitName")),
		PlanType: strings.TrimSpace(stringValue(snapshot, "planType")), Windows: []RateLimitWindow{}}
	for _, windowID := range []string{rateLimitsWindowPrimary, rateLimitsWindowSecondary} {
		if snapshot[windowID] == nil {
			continue
		}

		window, ok := snapshot[windowID].(map[string]any)
		if !ok {
			return RateLimitPool{}, errors.New("invalid native quota window")
		}

		used, ok := window["usedPercent"].(float64)
		if !ok || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
			return RateLimitPool{}, errors.New("invalid native quota utilization")
		}

		decoded := RateLimitWindow{ID: windowID, UsedPercent: used}
		if minutes, ok := window["windowDurationMins"].(float64); ok && minutes > 0 &&
			minutes < float64(math.MaxInt64/60) && math.Trunc(minutes) == minutes {
			decoded.DurationSeconds = new(int64(minutes) * 60)
		}

		if reset, ok := window["resetsAt"].(float64); ok && reset > 0 && reset < 253402300800 && math.Trunc(reset) == reset {
			decoded.ResetsAt = time.Unix(int64(reset), 0).UTC().Format(time.RFC3339)
		}

		pool.Windows = append(pool.Windows, decoded)
	}

	return pool, nil
}

// IsRateLimitsAuthError recognizes native authentication refusals without
// exporting the native response text, which can contain sensitive material.
func IsRateLimitsAuthError(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}

	message := strings.ToLower(rpcErr.Message)

	return rpcErr.Code == 401 || rpcErr.Code == 403 || strings.Contains(message, "not authenticated") ||
		strings.Contains(message, "requires chatgpt authentication") || strings.Contains(message, "401 unauthorized")
}
