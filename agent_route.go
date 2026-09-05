package codexacp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	routeMetaKey           = "acp-go.dev/route"
	routeMetaPath          = `_meta["` + routeMetaKey + `"]`
	routeVersion           = 1
	routeTurnNonceMaxBytes = 4 * 1024
	routeVersionKey        = "version"
	routeTurnNonceKey      = "turnNonce"
	routeToolCallIDKey     = "toolCallId"
	routeRequestIDKey      = "requestId"
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

func requestInboundRouteMeta(turnNonce string) map[string]any {
	if !validRouteTurnNonce(turnNonce) {
		return nil
	}

	return inboundRouteMeta(turnNonce)
}

func validRouteTurnNonce(turnNonce string) bool {
	return strings.TrimSpace(turnNonce) != "" && len(turnNonce) <= routeTurnNonceMaxBytes
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

// routeParamError refuses the reserved route envelope. It carries the verdict
// and the exact member at fault so a host reads one fact from one field path:
// `missing` means it left the key out, `unsupported` means the value it sent is
// refused — on the bare path when the value as a whole is not an object, and on
// the member path otherwise.
type routeParamError struct {
	verdict string
	member  string
}

// Error implements error.
func (e *routeParamError) Error() string { return e.verdict + " " + e.field() }

func (e *routeParamError) field() string {
	if e.member == "" {
		return routeMetaPath
	}

	return routeMetaPath + "." + e.member
}

func routeMissing() error {
	return &routeParamError{verdict: errValueMissing}
}

func routeUnsupported(member string) error {
	return &routeParamError{verdict: errValueUnsupported, member: member}
}

func parseInboundRoute(meta map[string]any) (inboundRoute, error) {
	raw, ok := meta[routeMetaKey]
	if !ok {
		return inboundRoute{}, routeMissing()
	}

	value, ok := raw.(map[string]any)
	if !ok {
		return inboundRoute{}, routeUnsupported("")
	}

	for key := range value {
		if key != routeVersionKey && key != routeTurnNonceKey {
			return inboundRoute{}, routeUnsupported(key)
		}
	}

	version, ok := routeInteger(value[routeVersionKey])
	if !ok || version != routeVersion {
		return inboundRoute{}, routeUnsupported(routeVersionKey)
	}

	nonce, ok := value[routeTurnNonceKey].(string)
	if !ok || strings.TrimSpace(nonce) == "" || len(nonce) > routeTurnNonceMaxBytes {
		return inboundRoute{}, routeUnsupported(routeTurnNonceKey)
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

	if len(scope.TurnNonce) > routeTurnNonceMaxBytes {
		return nil, fmt.Errorf("elicitation route turnNonce exceeds the maximum size")
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
		requestID, err := elicitationRouteRequestID(scope.RequestID)
		if err != nil {
			return nil, err
		}

		correlations++
		route[routeRequestIDKey] = requestID
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

func elicitationRouteRequestID(requestID *acp.RequestId) (string, error) {
	variants := 0
	if requestID.Null != nil {
		variants++
	}

	if requestID.Number != nil {
		variants++
	}

	if requestID.Str != nil {
		variants++
	}

	if variants != 1 || requestID.Str == nil || strings.TrimSpace(string(*requestID.Str)) == "" {
		return "", fmt.Errorf("elicitation route requestId must contain exactly one non-empty string variant")
	}

	return string(*requestID.Str), nil
}

// routeInvalidParams renders one route refusal. A nonce that does not name the
// addressed session's current turn is a present, refused value like any other,
// so it names the `turnNonce` member rather than the bare key.
func routeInvalidParams(err error) error {
	if err == nil {
		return nil
	}

	var routeErr *routeParamError
	if !errors.As(err, &routeErr) {
		routeErr = &routeParamError{verdict: errValueUnsupported, member: routeTurnNonceKey}
	}

	return acp.NewInvalidParams(map[string]any{jsonFieldError: routeErr.verdict, jsonFieldField: routeErr.field()})
}
