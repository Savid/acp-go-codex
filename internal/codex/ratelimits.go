package codex

import (
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	rateLimitsDefaultPool     = "codex"
	rateLimitsWindowPrimary   = "primary"
	rateLimitsWindowSecondary = "secondary"
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
