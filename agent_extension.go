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

	if method == SteerTurnMethod {
		return a.handleSteerExtension(ctx, params)
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

		return a.rateLimits(ctx)
	default:
		if result, handled, err := a.handleAuthExtensionMethod(ctx, method, params); handled {
			return result, err
		}

		return nil, acp.NewMethodNotFound(method)
	}
}

func (a *Agent) handleSteerExtension(ctx context.Context, params json.RawMessage) (any, error) {
	var request acp.PromptRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: "invalid steer request"})
	}

	// Exact route ownership is decided before the reserved lifecycle literal,
	// matching prompt and cancel admission on this turn-scoped surface.
	route, err := parseInboundRoute(request.Meta)
	if err != nil {
		return nil, routeInvalidParams(err)
	}

	session, err := a.session(request.SessionId)
	if err != nil {
		return nil, err
	}

	if route.TurnNonce != session.activeTurnNonce() {
		return nil, routeInvalidParams(errTurnRouteMismatch)
	}

	if err := rejectLifecycleKey(request.Meta); err != nil {
		return nil, err
	}

	if err := request.Validate(); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: "invalid steer request"})
	}

	if err := session.steerTurn(ctx, route.TurnNonce, request.Prompt); err != nil {
		return nil, codexThreadACPError(err, session.accountMetaSnapshot())
	}

	return struct{}{}, nil
}
