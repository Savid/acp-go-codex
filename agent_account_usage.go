package codexacp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/process"
	coreusage "github.com/savid/acp-go-core/usage"
	"github.com/savid/acp-go-core/usage/anthropic"
	"github.com/savid/acp-go-core/usage/gateway"
	"github.com/savid/acp-go-core/usage/openaicodex"
	"github.com/savid/acp-go-core/usage/opencodego"
	"github.com/savid/acp-go-core/usage/openrouter"
	"github.com/savid/acp-go-core/wire"
)

const (
	// The suffixes of the limit ids the read answers with, one per native
	// window.
	windowPrimary   = "primary"
	windowSecondary = "secondary"
)

// accountUsage answers _codex/accountUsage through the shared app-server. A
// sessionId, when given, must name a live session but does not scope the read.
// The request is decoded first so its trace keys open the span.
func (a *Agent) accountUsage(ctx context.Context, params json.RawMessage) (resp wire.AccountUsageResponse, err error) {
	request, refusal := wire.DecodeAccountUsageRequest(params, wire.AccountUsageScopeAgent)

	ctx, finish := a.observe.StartACP(ctx, request.Meta, AccountUsageMethod)
	defer func() { finish(err) }()

	if refusal != nil {
		return wire.AccountUsageResponse{}, refusal
	}

	switch request.ProviderID {
	case "":
		return wire.AccountUsageResponse{}, wire.Missing("providerId")
	case openaicodex.ProviderID, anthropic.ProviderID, opencodego.ProviderID, openrouter.ProviderID:
	default:
		return wire.AccountUsageResponse{}, wire.Unsupported("providerId")
	}

	if request.SessionID != "" {
		if _, lookupErr := a.session(ctx, request.SessionID); lookupErr != nil {
			return wire.AccountUsageResponse{}, lookupErr
		}
	} else if openErr := a.ensureOpen(); openErr != nil {
		return wire.AccountUsageResponse{}, openErr
	}

	rt, err := a.ensureRuntime(ctx)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	readCtx, cancel := context.WithTimeout(ctx, wire.AccountUsageReadTimeout)
	defer cancel()

	// The ChatGPT account is read natively; any provider the home routes
	// through a gateway, ChatGPT included when no login holds it, is read from
	// that gateway's report.
	response := wire.AccountUsageUnavailable(wire.AccountUsageNotAuthenticated)
	if request.ProviderID == openaicodex.ProviderID {
		response, err = readAccountUsage(readCtx, rt.client)
		if err != nil {
			a.log.ErrorContext(ctx, "codex account usage read failed", slog.String("reason", err.Error()))

			return wire.AccountUsageResponse{}, coreusage.RequestError(vendor, err)
		}

		if response.Available {
			return response, nil
		}
	}

	response, err = gateway.ReadRoutes(readCtx, a.providerTransport, func(context.Context) ([]gateway.Route, error) {
		env, envErr := a.environment().Build()
		if envErr != nil {
			return nil, envErr
		}

		return codex.GatewayRoutes(rt.home, a.options.CodexConfigOverrides, func(key string) (string, bool) { return process.Lookup(env, key) })
	}, request.ProviderID, response)
	if err != nil {
		a.log.ErrorContext(ctx, "codex account usage read failed", slog.String("reason", err.Error()))

		return wire.AccountUsageResponse{}, coreusage.RequestError(vendor, err)
	}

	return response, nil
}

// readAccountUsage reads the account first. A home with no login is
// not_authenticated, and a login that is not a ChatGPT account has no
// allowance windows and is not_reported; the rate-limit read, which the
// app-server refuses for both, is attempted for neither.
func readAccountUsage(ctx context.Context, client *codex.Client) (wire.AccountUsageResponse, error) {
	account, err := client.ReadAccount(ctx)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	if account.Account == nil {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotAuthenticated), nil
	}

	if account.Account.Type != codex.AccountTypeChatGPT {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotReported), nil
	}

	usage, err := client.ReadAccountUsage(ctx)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	return accountUsageResponse(account.Account.PlanType, usage, time.Now())
}

// accountWindow pairs one native window with the id suffix it answers under.
type accountWindow struct {
	suffix string
	window *codex.UsageWindow
}

// accountUsageResponse maps the native limits to the contract shape: one limit
// per present window, keyed <limitId>/primary or <limitId>/secondary, with the
// account's plan type as plan.
func accountUsageResponse(plan string, usage codex.AccountUsage, now time.Time) (wire.AccountUsageResponse, error) {
	response := wire.AccountUsageResponse{Available: true, Plan: strings.TrimSpace(plan), UsageAllowed: usage.OrdinaryUsageAllowed}

	for _, limit := range usage.Sorted() {
		if limit.ID == "" || limit.ID != strings.TrimSpace(limit.ID) {
			return wire.AccountUsageResponse{}, fmt.Errorf("native limit id %q is not a key", limit.ID)
		}

		for _, w := range []accountWindow{{windowPrimary, limit.Primary}, {windowSecondary, limit.Secondary}} {
			if w.window == nil {
				continue
			}

			entry := wire.AccountUsageLimit{ObservedAt: wire.AccountUsageTime(now), ID: limit.ID + "/" + w.suffix, Label: strings.TrimSpace(limit.Name), UsedPercent: w.window.UsedPercent}
			if minutes := w.window.WindowDurationMins; minutes > 0 && minutes <= math.MaxInt64/60 {
				entry.WindowSeconds = minutes * 60
			}

			if reset := w.window.ResetsAt; reset > 0 {
				entry.ResetsAt = wire.AccountUsageTime(time.Unix(reset, 0))
			}

			response.Limits = append(response.Limits, entry)
		}
	}

	if len(response.Limits) == 0 {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotReported), nil
	}

	if err := response.Validate(); err != nil {
		return wire.AccountUsageResponse{}, err
	}

	return response, nil
}
