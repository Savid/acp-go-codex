package codexacp

import (
	"context"
	"encoding/json"

	"github.com/coder/acp-go-sdk"
)

// HandleExtensionMethod handles Codex-specific ACP extension methods.
func (a *Agent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if err := a.ensureOpen(); err != nil {
		return nil, err
	}

	// Every extension route refuses the reserved literal before its own side
	// effects or its own refusal, fork and every provider-auth leg included, so
	// a configured and an unconfigured leg answer the key the same way.
	if err := rejectLifecycleKeyInParams(params); err != nil {
		return nil, err
	}

	switch method {
	case ForkSessionMethod:
		var req acp.UnstableForkSessionRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
		}

		if err := req.Validate(); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
		}

		return a.forkSession(ctx, req)
	case RateLimitsMethod:
		if err := decodeRateLimitsParams(params); err != nil {
			return nil, err
		}

		return a.rateLimits(ctx), nil
	default:
		if result, handled, err := a.handleAuthExtensionMethod(ctx, method, params); handled {
			return result, err
		}

		return nil, acp.NewMethodNotFound(method)
	}
}
