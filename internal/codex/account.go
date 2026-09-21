package codex

import (
	"context"
	"maps"
	"slices"
)

// Account methods the adapter calls.
const (
	methodAccountRead           = "account/read"
	methodAccountRateLimitsRead = "account/rateLimits/read"
)

// AccountTypeChatGPT is the one account type with allowance windows; the
// app-server refuses the rate-limit read for an API-key or Bedrock login.
const AccountTypeChatGPT = "chatgpt"

// Account is the account/read result. A nil Account means the home holds no
// login, and account/rateLimits/read refuses in that state.
type Account struct {
	Account *AccountIdentity `json:"account"`
}

// AccountIdentity is the logged-in account: its type selects whether a
// rate-limit read is possible and its plan type names the plan.
type AccountIdentity struct {
	Type     string `json:"type"`
	PlanType string `json:"planType"`
}

// AccountUsage is the account/rateLimits/read result: one limit per limit id
// and the app-server's own statement of whether ordinary usage is allowed,
// nil when it makes none.
type AccountUsage struct {
	OrdinaryUsageAllowed *bool                 `json:"ordinaryUsageAllowed"`
	Limits               map[string]UsageLimit `json:"rateLimitsByLimitId"`
}

// UsageLimit is one allowance with up to two windows. ID is its map key.
type UsageLimit struct {
	ID        string       `json:"-"`
	Name      string       `json:"limitName"`
	Primary   *UsageWindow `json:"primary"`
	Secondary *UsageWindow `json:"secondary"`
}

// UsageWindow is one window's utilization, length in minutes, and reset as
// Unix seconds.
type UsageWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int64   `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

// ReadAccount reports the account the app-server is logged in as.
func (c *Client) ReadAccount(ctx context.Context) (Account, error) {
	var account Account

	err := c.Call(ctx, methodAccountRead, map[string]any{}, &account)

	return account, err
}

// ReadAccountUsage performs one fresh account/rateLimits/read. It skips the
// reset-credit lookup, a second backend request whose result the adapter never
// reads.
func (c *Client) ReadAccountUsage(ctx context.Context) (AccountUsage, error) {
	var usage AccountUsage

	err := c.Call(ctx, methodAccountRateLimitsRead, map[string]any{"excludeResetCreditDetails": true}, &usage)

	return usage, err
}

// Sorted lists the limits in id order, each carrying its map key as ID.
func (u AccountUsage) Sorted() []UsageLimit {
	limits := make([]UsageLimit, 0, len(u.Limits))

	for _, id := range slices.Sorted(maps.Keys(u.Limits)) {
		limit := u.Limits[id]
		limit.ID = id
		limits = append(limits, limit)
	}

	return limits
}
