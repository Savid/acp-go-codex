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

// rateLimits resolves the current rate-limit view. It prefers a fresh
// account/rateLimits/read against a live session whose native version supports
// the request, falls back to the latest cached snapshot, and otherwise reports
// an empty window set. Absence is never an error.
func (a *Agent) rateLimits(ctx context.Context) RateLimitsResponse {
	if client, ok := a.liveRateLimitsClient(); ok {
		if snapshot, err := client.ReadRateLimits(ctx); err == nil && snapshot.HasData() {
			a.cacheRateLimits(snapshot)

			return rateLimitsResponseFromSnapshot(snapshot)
		}
	}

	if snapshot, ok := a.cachedRateLimits(); ok {
		return rateLimitsResponseFromSnapshot(snapshot)
	}

	return RateLimitsResponse{Windows: []RateLimitWindow{}}
}

// liveRateLimitsClient returns a live session client whose native version
// supports the account/rateLimits/read request, if one exists.
func (a *Agent) liveRateLimitsClient() (codex.Client, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, false
	}

	for _, session := range a.sessions {
		if session.client != nil && session.client.RateLimitsSupported() {
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
