package codex

// rateLimitsMinVersion is the earliest native codex version that exposes the
// account/rateLimits/read request. The app-server carried the rate-limit
// snapshot on the token-count notification path before this, but the explicit
// read method only arrived in 0.142.0.
const rateLimitsMinVersion = "0.142.0"

// Codex-native rate-limit window identifiers.
const (
	windowPrimary   = "primary"
	windowSecondary = "secondary"
)

// rateLimitWindowIDs are the codex-native window identifiers, decoded in a
// stable order so the emitted payload is deterministic.
var rateLimitWindowIDs = []string{windowPrimary, windowSecondary}

// RateLimitWindow is a single harness-reported usage window.
type RateLimitWindow struct {
	// ID is the codex-native window identifier ("primary" or "secondary").
	ID string
	// UsedPercent is the harness-reported utilisation, passed through as
	// reported. The app-server protocol constrains it to 0-100, so no clamping
	// is applied.
	UsedPercent float64
	// ResetsAt is the RFC3339 reset instant, empty when the harness does not
	// supply one.
	ResetsAt string
}

// RateLimitSnapshot is a decoded rate-limit snapshot. Every field is populated
// only from harness-reported values; nothing is derived from token counts or
// fabricated.
type RateLimitSnapshot struct {
	Windows  []RateLimitWindow
	PlanType string
}

// HasData reports whether the snapshot carries any harness-reported content.
func (s RateLimitSnapshot) HasData() bool {
	return len(s.Windows) > 0 || s.PlanType != ""
}

// rateLimitSnapshotPayload extracts the RateLimitSnapshot object from an
// account/rateLimits/read response or account/rateLimits/updated notification,
// both of which nest it under "rateLimits".
func rateLimitSnapshotPayload(values map[string]any) map[string]any {
	if snapshot := mapValue(values, "rateLimits"); snapshot != nil {
		return snapshot
	}

	return values
}

// rateLimitSnapshotFromMap decodes a codex app-server v2 RateLimitSnapshot.
//
// The v2 app-server wire uses camelCase field names: each window carries
// usedPercent (int, 0-100) and resetsAt (Unix epoch seconds), and the snapshot
// carries planType. A window is emitted only when the harness supplies its
// object; absent windows are skipped rather than fabricated.
func rateLimitSnapshotFromMap(snapshot map[string]any) RateLimitSnapshot {
	out := RateLimitSnapshot{PlanType: stringValue(snapshot, "planType")}

	out.Windows = make([]RateLimitWindow, 0, len(rateLimitWindowIDs))
	for _, id := range rateLimitWindowIDs {
		window := mapValue(snapshot, id)
		if window == nil {
			continue
		}

		out.Windows = append(out.Windows, RateLimitWindow{
			ID:          id,
			UsedPercent: float64Value(window, "usedPercent"),
			ResetsAt:    timestampValue(window, "resetsAt"),
		})
	}

	return out
}
