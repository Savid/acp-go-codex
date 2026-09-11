package codexacp

import (
	"context"
	"maps"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	rateLimitsReadTimeout            = 30 * time.Second
	rateLimitsReasonReadFailed       = "read_failed"
	rateLimitsReasonNotObserved      = "not_observed"
	rateLimitsReasonNotAuthenticated = "not_authenticated"
)

type rateLimitsTarget struct {
	session  *session
	provider string
	cwd      string
	custom   bool
	env      map[string]string
}

type rateLimitsFence struct {
	client  codex.Client
	runtime uint64
	auth    uint64
}

func (a *Agent) rateLimits(ctx context.Context, request RateLimitsRequest) (RateLimitsResponse, error) {
	target, err := a.rateLimitsTarget(request)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	response, err := a.rateLimitsForTarget(ctx, target)
	if ctx.Err() != nil {
		return RateLimitsResponse{}, ctx.Err()
	}

	// Native failures and unavailable results still complete an Agent/session
	// operation. Teardown or poisoning that won while a native call blocked
	// must keep its lifecycle error instead of becoming missing quota data.
	if _, lifecycleErr := a.rateLimitsTargetCurrent(target); lifecycleErr != nil {
		return RateLimitsResponse{}, lifecycleErr
	}

	return response, err
}

func (a *Agent) rateLimitsForTarget(ctx context.Context, target rateLimitsTarget) (RateLimitsResponse, error) {
	if target.provider != "" && target.provider != authProviderOpenAI {
		return rateLimitsUnsupported(target.provider), nil
	}

	if target.custom && target.provider != "" {
		return rateLimitsUnsupported(authProviderOpenAI), nil
	}

	readCtx, cancel := context.WithTimeout(ctx, rateLimitsReadTimeout)
	defer cancel()

	client, err := a.sharedRuntime(readCtx)
	if err != nil {
		if ctx.Err() != nil {
			return RateLimitsResponse{}, ctx.Err()
		}

		if readCtx.Err() != nil && target.provider != "" {
			return rateLimitsUnavailable(target.provider, rateLimitsReasonReadFailed), nil
		}

		return RateLimitsResponse{}, err
	}

	a.mu.Lock()
	fence := rateLimitsFence{client: client, runtime: a.runtimeEpoch, auth: a.rateLimitsAuthEpoch}
	a.mu.Unlock()

	reader, ok := client.(codex.ProviderRouteClient)
	if !ok {
		return unresolvedRateLimits(target.provider)
	}

	configuration, err := reader.ReadProviderRoute(readCtx, target.cwd)
	if err != nil {
		if ctx.Err() != nil {
			return RateLimitsResponse{}, ctx.Err()
		}

		return a.rateLimitsReadFailure(ctx, target, fence, err)
	}

	if target.provider == "" {
		target.provider = configuration.ProviderID
	}

	if target.provider == "" {
		return unresolvedRateLimits("")
	}

	if target.provider != authProviderOpenAI || configuration.ProviderID != target.provider ||
		configuration.Custom || configuration.EnvironmentBaseURL || target.custom {
		return rateLimitsUnsupported(target.provider), nil
	}

	return a.readRateLimits(readCtx, ctx, target, fence, reader, configuration)
}

func unresolvedRateLimits(provider string) (RateLimitsResponse, error) {
	if provider == "" {
		return RateLimitsResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: valMissing, jsonFieldField: authFieldProviderID})
	}

	return rateLimitsUnavailable(provider, rateLimitsReasonReadFailed), nil
}

func (a *Agent) rateLimitsTarget(request RateLimitsRequest) (rateLimitsTarget, error) {
	target := rateLimitsTarget{provider: request.ProviderID}
	if request.SessionID != "" {
		session, err := a.session(request.SessionID)
		if err != nil {
			return target, err
		}

		session.mu.Lock()
		failure := session.lifecycleFailure
		session.mu.Unlock()

		if failure != nil {
			return target, failure
		}

		snapshot := session.snapshot()

		target.session, target.cwd, target.env = session, snapshot.cwd, snapshot.env
		if target.provider == "" {
			target.provider = snapshot.modelProvider
		}

		target.custom = snapshot.modelProvider != "" && target.provider != snapshot.modelProvider || rateLimitsCustomEnv(snapshot.env)
	}

	if target.provider == "" {
		target.provider, _ = a.options.Config["model_provider"].(string)
	}

	target.custom = target.custom || rateLimitsCustomEnv(a.options.Env)

	return target, nil
}

func rateLimitsCustomEnv(env map[string]string) bool {
	return env["OPENAI_BASE_URL"] != ""
}

func (a *Agent) readRateLimits(
	ctx, caller context.Context,
	target rateLimitsTarget,
	fence rateLimitsFence,
	reader codex.ProviderRouteClient,
	configuration codex.ProviderRoute,
) (RateLimitsResponse, error) {
	account, err := fence.client.AccountRead(ctx)
	if err != nil {
		return a.rateLimitsReadFailure(caller, target, fence, err)
	}

	if account.AuthMode == "" {
		return rateLimitsUnavailable(target.provider, rateLimitsReasonNotAuthenticated), nil
	}

	// Native account mode is authoritative even when API-key variables exist.
	if account.AuthMode != codex.AuthModeChatGPT {
		return rateLimitsUnsupported(target.provider), nil
	}

	snapshot, err := fence.client.ReadRateLimits(ctx)
	if err != nil {
		return a.rateLimitsReadFailure(caller, target, fence, err)
	}

	after, err := fence.client.AccountRead(ctx)
	if err != nil {
		return a.rateLimitsReadFailure(caller, target, fence, err)
	}

	currentConfig, err := reader.ReadProviderRoute(ctx, target.cwd)
	if err != nil {
		return a.rateLimitsReadFailure(caller, target, fence, err)
	}

	current, err := a.rateLimitsFenceCurrent(target, fence)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	if !current || currentConfig != configuration || account.ID != after.ID || account.Email != after.Email ||
		account.AuthMode != after.AuthMode || snapshot.AccountID != "" && account.ID != "" && snapshot.AccountID != account.ID {
		return rateLimitsUnavailable(target.provider, rateLimitsReasonReadFailed), nil
	}

	if caller.Err() != nil {
		return RateLimitsResponse{}, caller.Err()
	}

	return rateLimitsResponseFromSnapshot(target.provider, snapshot), nil
}

func (a *Agent) rateLimitsTargetCurrent(target rateLimitsTarget) (bool, error) {
	if err := a.ensureOpen(); err != nil {
		return false, err
	}

	if target.session != nil {
		current, err := a.session(target.session.id)
		if err != nil {
			return false, err
		}

		current.mu.Lock()
		failure := current.lifecycleFailure
		provider, cwd := current.modelProvider, current.cwd
		envChanged := !maps.Equal(current.env, target.env)
		current.mu.Unlock()

		if failure != nil {
			return false, failure
		}

		if current != target.session || provider != "" && provider != target.provider || cwd != target.cwd || envChanged {
			return false, nil
		}
	}

	return true, nil
}

func (a *Agent) rateLimitsFenceCurrent(target rateLimitsTarget, fence rateLimitsFence) (bool, error) {
	current, err := a.rateLimitsTargetCurrent(target)
	if err != nil || !current {
		return false, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return false, newAgentClosedError()
	}

	return !a.runtimeDead && a.runtimeClient == fence.client &&
		a.runtimeEpoch == fence.runtime && a.rateLimitsAuthEpoch == fence.auth, nil
}

func (a *Agent) rateLimitsReadFailure(ctx context.Context, target rateLimitsTarget, fence rateLimitsFence, err error) (RateLimitsResponse, error) {
	if ctx.Err() != nil {
		return RateLimitsResponse{}, ctx.Err()
	}

	current, lifecycleErr := a.rateLimitsFenceCurrent(target, fence)
	if lifecycleErr != nil {
		return RateLimitsResponse{}, lifecycleErr
	}

	if target.provider == "" {
		return unresolvedRateLimits("")
	}

	reason := rateLimitsReasonReadFailed
	if current && codex.IsRateLimitsAuthError(err) {
		reason = rateLimitsReasonNotAuthenticated
	}

	return rateLimitsUnavailable(target.provider, reason), nil
}

func (a *Agent) invalidateRateLimitsAuth() {
	a.mu.Lock()
	a.rateLimitsAuthEpoch++
	a.mu.Unlock()
}

func rateLimitsResponseFromSnapshot(provider string, snapshot codex.RateLimitSnapshot) RateLimitsResponse {
	now := time.Now()
	if snapshot.ObservedAt.IsZero() || now.Sub(snapshot.ObservedAt) > time.Minute {
		return rateLimitsUnavailable(provider, rateLimitsReasonNotObserved)
	}

	response := RateLimitsResponse{ProviderID: provider, Availability: "available", Pools: []RateLimitPool{}}
	for _, nativePool := range snapshot.Pools {
		pool := RateLimitPool{ID: nativePool.ID, Label: nativePool.Label, PlanType: nativePool.PlanType, Windows: []RateLimitWindow{}}
		for _, window := range nativePool.Windows {
			if reset, err := time.Parse(time.RFC3339, window.ResetsAt); err == nil && !reset.After(now) {
				continue
			}

			pool.Windows = append(pool.Windows, RateLimitWindow{ID: window.ID, UsedPercent: new(window.UsedPercent),
				DurationSeconds: window.DurationSeconds, ObservedAt: snapshot.ObservedAt.UTC().Format(time.RFC3339Nano), ResetsAt: window.ResetsAt})
		}

		if len(pool.Windows) != 0 {
			response.Pools = append(response.Pools, pool)
		}
	}

	if len(response.Pools) == 0 {
		return rateLimitsUnavailable(provider, rateLimitsReasonNotObserved)
	}

	return response
}
