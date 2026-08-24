package codexacp

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// RateLimitsMethod is the Codex extension request that reports the harness's
// subscription rate-limit usage. It is agent-level: the request takes an empty
// params object and carries no sessionId.
const RateLimitsMethod = "_codex/rateLimits"

// RateLimitsResponse is the payload returned by [RateLimitsMethod]. Windows is
// always present; it is an empty slice when the harness has reported no usage.
type RateLimitsResponse struct {
	Windows  []RateLimitWindow `json:"windows"`
	PlanType string            `json:"planType,omitempty"`
}

// RateLimitWindow is a single harness-reported usage window. UsedPercent is
// passed through exactly as the harness reports it (0-100 by protocol). ResetsAt
// is an RFC3339 instant, present only when the harness supplies it.
type RateLimitWindow struct {
	ID          string  `json:"id"`
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    string  `json:"resetsAt,omitempty"`
}

// rateLimits reads the current app-server snapshot through any live session.
// The supported Codex floor always exposes account/rateLimits/read, so a live
// native client is queried directly and a read failure is returned. An agent
// without a live session, including a placeholder-only agent, reports an empty
// window set.
func (a *Agent) rateLimits(ctx context.Context) (RateLimitsResponse, error) {
	client, ok := a.liveRateLimitsClient()
	if !ok {
		return RateLimitsResponse{Windows: []RateLimitWindow{}}, nil
	}

	snapshot, err := client.ReadRateLimits(ctx)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	return rateLimitsResponseFromSnapshot(snapshot), nil
}

// liveRateLimitsClient returns a live session client, if one exists.
func (a *Agent) liveRateLimitsClient() (codex.Client, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, false
	}

	for _, session := range a.sessions {
		if session.client != nil {
			return session.client, true
		}
	}

	return nil, false
}

func rateLimitsResponseFromSnapshot(snapshot codex.RateLimitSnapshot) RateLimitsResponse {
	windows := make([]RateLimitWindow, 0, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		windows = append(windows, RateLimitWindow{
			ID:          window.ID,
			UsedPercent: window.UsedPercent,
			ResetsAt:    window.ResetsAt,
		})
	}

	return RateLimitsResponse{Windows: windows, PlanType: snapshot.PlanType}
}

// decodeRateLimitsParams applies the shared extension-method validation: an
// absent, null, or empty params object is accepted, while anything else —
// including unknown fields — is rejected as invalid params. The request
// carries no fields, so it is validated inline against an empty object.
func decodeRateLimitsParams(params json.RawMessage) error {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()

	var req struct{}
	if err := decoder.Decode(&req); err != nil {
		return acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	return nil
}
