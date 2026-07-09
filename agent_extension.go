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
		return nil, acp.NewMethodNotFound(method)
	}
}
