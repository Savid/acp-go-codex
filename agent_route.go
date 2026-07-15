package codexacp

import (
	"context"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	routeMetaKey       = "acp-go.dev/route"
	routeVersion       = 1
	routeVersionKey    = "version"
	routeVersionsKey   = "versions"
	routeTurnNonceKey  = "turnNonce"
	routeToolCallIDKey = "toolCallId"
	routeRequestIDKey  = "requestId"
)

type inboundRoute struct {
	TurnNonce string
}

type turnRouteContextKey struct{}

func inboundRouteMeta(turnNonce string) map[string]any {
	return map[string]any{routeMetaKey: map[string]any{
		routeVersionKey:   routeVersion,
		routeTurnNonceKey: turnNonce,
	}}
}

func withTurnRoute(ctx context.Context, turnNonce string) context.Context {
	return context.WithValue(ctx, turnRouteContextKey{}, turnNonce)
}

func turnRouteMetaFromContext(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}

	turnNonce, _ := ctx.Value(turnRouteContextKey{}).(string)
	if turnNonce == "" {
		return nil
	}

	return inboundRouteMeta(turnNonce)
}

func parseInboundRoute(meta map[string]any) (inboundRoute, error) {
	raw, ok := meta[routeMetaKey]
	if !ok {
		return inboundRoute{}, fmt.Errorf("_meta.%s is required", routeMetaKey)
	}

	value, ok := raw.(map[string]any)
	if !ok || len(value) != 2 {
		return inboundRoute{}, fmt.Errorf("_meta.%s must contain exactly version and turnNonce", routeMetaKey)
	}

	version, ok := routeInteger(value[routeVersionKey])
	if !ok || version != routeVersion {
		return inboundRoute{}, fmt.Errorf("_meta.%s.version must be %d", routeMetaKey, routeVersion)
	}

	nonce, ok := value[routeTurnNonceKey].(string)
	if !ok || strings.TrimSpace(nonce) == "" {
		return inboundRoute{}, fmt.Errorf("_meta.%s.turnNonce must be a non-empty string", routeMetaKey)
	}

	return inboundRoute{TurnNonce: nonce}, nil
}

func routeInteger(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case float64:
		integer := int(typed)

		return integer, typed == float64(integer)
	default:
		return 0, false
	}
}

func stampElicitationRoute(meta map[string]any, scope elicitationScope) (map[string]any, error) {
	if _, collision := meta[routeMetaKey]; collision {
		return nil, fmt.Errorf("reserved _meta.%s collision", routeMetaKey)
	}

	if scope.SessionID == "" || strings.TrimSpace(scope.TurnNonce) == "" {
		return nil, fmt.Errorf("elicitation route requires sessionId and turnNonce")
	}

	correlations := 0

	route := map[string]any{
		routeVersionKey:    routeVersion,
		jsonFieldSessionID: string(scope.SessionID),
		routeTurnNonceKey:  scope.TurnNonce,
	}
	if scope.ToolCallID != "" {
		correlations++
		route[routeToolCallIDKey] = string(scope.ToolCallID)
	}

	if scope.RequestID != nil {
		correlations++
		route[routeRequestIDKey] = *scope.RequestID
	}

	if correlations != 1 {
		return nil, fmt.Errorf("elicitation route requires exactly one native correlation")
	}

	out := cloneAnyMap(meta)
	if out == nil {
		out = map[string]any{}
	}

	out[routeMetaKey] = route

	return out, nil
}

func routeInvalidParams(err error) error {
	if err == nil {
		return nil
	}

	return acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error(), jsonFieldField: "_meta." + routeMetaKey})
}
