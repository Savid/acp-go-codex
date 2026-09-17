package codexacp

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/wire"
)

// accountUsageSessionField is the request member naming the session.
const accountUsageSessionField = "sessionId"

func callAccountUsage(t *testing.T, h *harness, params any) (wire.AccountUsageResponse, error) {
	t.Helper()

	raw, err := h.conn.CallExtension(h.ctx(), AccountUsageMethod, params)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	var response wire.AccountUsageResponse

	require.NoError(t, json.Unmarshal(raw, &response))
	require.NoError(t, response.Validate())

	return response, nil
}

// The read needs no session: it starts the shared app-server if none is live
// and maps every window of every limit in limit-id order.
func TestAccountUsageReadsThroughTheSharedRuntime(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()

	before := time.Now()
	response, err := callAccountUsage(t, h, map[string]any{})
	require.NoError(t, err)

	observed, parseErr := time.Parse(time.RFC3339, response.ObservedAt)
	require.NoError(t, parseErr)
	require.False(t, observed.Before(before.Truncate(time.Second)))

	response.ObservedAt = ""
	require.Equal(t, wire.AccountUsageResponse{Available: true, Plan: "pro", UsageAllowed: new(true), Limits: []wire.AccountUsageLimit{
		{ID: "codex/primary", WindowSeconds: 604800, UsedPercent: 23, ResetsAt: "2026-09-21T03:22:18Z"},
		{ID: "codex_bengalfox/primary", Label: "GPT-5.3-Codex-Spark", WindowSeconds: 18000, UsedPercent: 0, ResetsAt: "2026-09-17T03:43:31Z"},
		{ID: "codex_bengalfox/secondary", Label: "GPT-5.3-Codex-Spark", WindowSeconds: 604800, UsedPercent: 0, ResetsAt: "2026-09-23T22:43:31Z"},
	}}, response)

	sessionID := h.newSession().SessionId
	withSession, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.NoError(t, err, "a live session id is accepted and does not scope the read")
	require.Len(t, withSession.Limits, 3)

	_, err = callAccountUsage(t, h, nil)
	require.NoError(t, err, "absent params are the empty request")
}

func TestAccountUsageRefusals(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	sessionID := h.newSession().SessionId

	cases := []struct {
		name   string
		params any
		data   map[string]any
	}{
		{"unknown session", map[string]any{accountUsageSessionField: "nope"}, map[string]any{"error": "unknown session", "field": accountUsageSessionField}},
		{"empty session", map[string]any{accountUsageSessionField: ""}, map[string]any{"error": "unsupported", "field": accountUsageSessionField}},
		{"unknown member", map[string]any{"providerId": "openai"}, map[string]any{"error": "unsupported", "field": "providerId"}},
		{"lifecycle key", map[string]any{"_meta": map[string]any{wire.LifecycleKey: map[string]any{}}}, map[string]any{"error": "unsupported", "field": `_meta["` + wire.LifecycleKey + `"]`}},
	}

	for _, tc := range cases {
		_, err := callAccountUsage(t, h, tc.params)
		require.Equal(t, -32602, requestErrorCode(t, err), tc.name)
		require.Equal(t, tc.data, requestErrorData(t, err), tc.name)
	}

	_, err := h.conn.UnstableDeleteSession(h.ctx(), wire.DeleteSessionRequest(sessionID))
	require.NoError(t, err)

	_, err = callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.Equal(t, -32602, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "unknown session", "field": accountUsageSessionField}, requestErrorData(t, err), "a tombstoned session")
}

// A home with no login is not_authenticated and an API-key login is
// not_reported, both before any rate-limit read; a ChatGPT account with no
// window is not_reported; a native refusal is the account_usage failure class.
func TestAccountUsageUnavailableAndRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		account  string
		expected wire.AccountUsageResponse
	}{
		{fakeCodexAccountNone, wire.AccountUsageUnavailable(wire.AccountUsageNotAuthenticated)},
		{fakeCodexAccountAPIKey, wire.AccountUsageUnavailable(wire.AccountUsageNotReported)},
		{fakeCodexAccountEmpty, wire.AccountUsageUnavailable(wire.AccountUsageNotReported)},
	} {
		h := newHarness(t, WithEnv(map[string]string{fakeCodexEnv: "1", fakeCodexEnvAccount: tc.account}))
		h.initialize()

		response, err := callAccountUsage(t, h, map[string]any{})
		require.NoError(t, err, tc.account)
		require.Equal(t, tc.expected, response, tc.account)
	}

	refused := newHarness(t, WithEnv(map[string]string{fakeCodexEnv: "1", fakeCodexEnvAccount: fakeCodexAccountRefuse}))
	refused.initialize()

	_, err := callAccountUsage(t, refused, map[string]any{})
	require.Equal(t, -32603, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "codex_internal_failure", "class": "account_usage"}, requestErrorData(t, err))
}

func TestAccountUsageResponseMapping(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 17, 2, 41, 3, 900, time.UTC)

	single := codex.AccountUsage{Limits: map[string]codex.UsageLimit{"codex": {Secondary: &codex.UsageWindow{UsedPercent: 130}}}}
	response, err := accountUsageResponse(" pro ", single, now)
	require.NoError(t, err)
	require.Equal(t, wire.AccountUsageResponse{Available: true, ObservedAt: "2026-09-17T02:41:03Z", Plan: "pro", Limits: []wire.AccountUsageLimit{{ID: "codex/secondary", UsedPercent: 130}}}, response, "a window with no length or reset carries neither; the account's plan is used and trimmed")

	response, err = accountUsageResponse("pro", codex.AccountUsage{Limits: map[string]codex.UsageLimit{"a": {}, "b": {}}}, now)
	require.NoError(t, err)
	require.Equal(t, wire.AccountUsageUnavailable(wire.AccountUsageNotReported), response, "snapshots with no window report nothing")

	blocked := codex.AccountUsage{OrdinaryUsageAllowed: new(false), Limits: map[string]codex.UsageLimit{"codex": {Primary: &codex.UsageWindow{UsedPercent: 100}}}}
	response, err = accountUsageResponse("pro", blocked, now)
	require.NoError(t, err)
	require.Equal(t, new(false), response.UsageAllowed, "the app-server's statement is carried as is")

	late := codex.AccountUsage{Limits: map[string]codex.UsageLimit{"codex": {Primary: &codex.UsageWindow{UsedPercent: 1, ResetsAt: 253402300800}}}}
	response, err = accountUsageResponse("pro", late, now)
	require.NoError(t, err)
	require.Empty(t, response.Limits[0].ResetsAt, "a reset RFC 3339 cannot render is dropped, not wrapped into a failed read")

	_, err = accountUsageResponse("pro", codex.AccountUsage{Limits: map[string]codex.UsageLimit{" codex": {Primary: &codex.UsageWindow{UsedPercent: 1}}}}, now)
	require.Error(t, err, "a limit key that is not a key fails closed")

	_, err = accountUsageResponse("pro", codex.AccountUsage{Limits: map[string]codex.UsageLimit{"codex": {Primary: &codex.UsageWindow{UsedPercent: -1}}}}, now)
	require.Error(t, err)

	huge := codex.AccountUsage{Limits: map[string]codex.UsageLimit{"codex": {Primary: &codex.UsageWindow{UsedPercent: 1, WindowDurationMins: math.MaxInt64}}}}
	response, err = accountUsageResponse("pro", huge, now)
	require.NoError(t, err)
	require.Zero(t, response.Limits[0].WindowSeconds, "a window length that cannot be expressed in seconds is dropped, not wrapped")
}

// The request's trace keys open the read's span, as on every other request.
func TestAccountUsagePropagatesTraceContext(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	h := newHarness(t, WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))))
	h.initialize()

	const traceID = "0af7651916cd43dd8448eb211c80319c"

	_, err := callAccountUsage(t, h, map[string]any{"_meta": map[string]any{"traceparent": "00-" + traceID + "-b7ad6b7169203331-01"}})
	require.NoError(t, err)

	var traced bool

	for _, span := range exporter.GetSpans() {
		traced = traced || span.SpanContext.TraceID().String() == traceID
	}

	require.True(t, traced, "the read's span continues the host's trace")
}

// An unknown extension method is still method-not-found beside the served one.
func TestAccountUsageLeavesOtherExtensionsUnserved(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()

	_, err := h.conn.CallExtension(h.ctx(), "_codex/accountUsages", map[string]any{})
	require.Equal(t, -32601, requestErrorCode(t, err))
	require.Equal(t, "_codex/accountUsages", requestErrorData(t, err)["method"])
}
