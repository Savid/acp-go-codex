package codexacp

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

	observed, parseErr := time.Parse(time.RFC3339, response.Limits[0].ObservedAt)
	require.NoError(t, parseErr)
	require.False(t, observed.Before(before.Truncate(time.Second)))

	for i := range response.Limits {
		response.Limits[i].ObservedAt = ""
	}
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
		{"unadvertised provider", map[string]any{"providerId": "openai"}, map[string]any{"error": "unsupported", "field": "providerId"}},
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
	require.Equal(t, wire.AccountUsageResponse{Available: true, Plan: "pro", Limits: []wire.AccountUsageLimit{{ObservedAt: "2026-09-17T02:41:03Z", ID: "codex/secondary", UsedPercent: 130}}}, response, "a window with no length or reset carries neither; the account's plan is used and trimmed")

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

type gatewayTransport func(*http.Request) (*http.Response, error)

func (f gatewayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const gatewayReport = `{"generatedAt":1,"reports":[{"provider":"anthropic","fetchedAt":1789807237831,"limits":[{"id":"anthropic:5h","label":"Claude 5 Hour","window":{"id":"5h","durationMs":18000000,"resetsAt":1789817399682},"amount":{"usedFraction":0.25,"unit":"percent"},"status":"ok"}],"metadata":{}}]}`

// A provider the home's config.toml routes through a gateway is read from
// that gateway's report with the key named by env_key.
func TestAccountUsageReadsThroughConfiguredGateway(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t,
		WithEnv(map[string]string{fakeCodexEnv: "1", "OMP_GATEWAY_KEY": "gateway-key", "PROXY_KEY": "proxy-key"}),
		WithCodexConfigOverrides(map[string]any{"model_provider": "omp", "model_providers.omp.base_url": "https://gateway.example/v1", "model_providers.omp.env_key": "OMP_GATEWAY_KEY"}),
	)...)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.NoError(t, os.MkdirAll(a.options.Home, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(a.options.Home, "config.toml"), []byte("[model_providers.proxy]\nname = \"proxy\"\nbase_url = \"https://proxy.example/v1\"\nenv_key = \"PROXY_KEY\"\n"), 0o600))
	var asked []string
	a.providerTransport = gatewayTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/usage" {
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: http.NoBody}, nil
		}

		asked = append(asked, r.URL.Host+r.URL.Path+" "+r.Header.Get("Authorization"))
		if r.URL.Host != "gateway.example" {
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: http.NoBody}, nil
		}

		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(gatewayReport))}, nil
	})
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	params, err := json.Marshal(map[string]any{"providerId": "anthropic"})
	require.NoError(t, err)
	response, err := a.accountUsage(t.Context(), params)
	require.NoError(t, err)
	require.True(t, response.Available)
	require.Equal(t, "session", response.Limits[0].ID)
	require.Equal(t, []string{"gateway.example/v1/usage Bearer gateway-key", "proxy.example/v1/usage Bearer proxy-key"}[:1], asked[:1], "the launch override route is asked first, in name order")
	require.Equal(t, []string{"gateway.example/v1/usage Bearer gateway-key"}, asked, "a covering route ends the walk")

	params, err = json.Marshal(map[string]any{"providerId": "openrouter"})
	require.NoError(t, err)
	response, err = a.accountUsage(t.Context(), params)
	require.NoError(t, err)
	require.Equal(t, wire.AccountUsageUnavailable(wire.AccountUsageNotAuthenticated), response)

	params, err = json.Marshal(map[string]any{"providerId": "xai"})
	require.NoError(t, err)
	_, err = a.accountUsage(t.Context(), params)
	require.Equal(t, map[string]any{"error": "unsupported", "field": "providerId"}, requestErrorData(t, err))
}

// A model provider routed through a gateway that publishes its model list
// advertises that list, each id naming its upstream, in place of the
// app-server's presets.
func TestGatewayModelListReplacesThePresets(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t,
		WithEnv(map[string]string{fakeCodexEnv: "1", "OMP_GATEWAY_KEY": "gateway-key"}),
		WithCodexConfigOverrides(map[string]any{"model_provider": "omp", "model_providers.omp.base_url": "https://gateway.example/v1", "model_providers.omp.env_key": "OMP_GATEWAY_KEY"}),
		WithConfiguredModels([]string{"opencode-go/qwen3.8-flash"}),
	)...)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	a.providerTransport = gatewayTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "gateway.example" && r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer gateway-key" {
			body := `{"object":"list","data":[{"id":"openai-codex/gpt-5.6-luna","display_name":"GPT-5.6-Luna","context_length":400000},{"id":"opencode-go/qwen3.8-flash"}]}`

			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}

		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: http.NoBody}, nil
	})
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	session, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	var values []acp.SessionConfigValueId
	for _, option := range session.ConfigOptions {
		if option.Select != nil && option.Select.Id == configModel {
			for _, value := range *option.Select.Options.Ungrouped {
				values = append(values, value.Value)
			}
		}
	}
	require.Equal(t, []acp.SessionConfigValueId{"openai-codex/gpt-5.6-luna", "opencode-go/qwen3.8-flash"}, values, "the gateway's ids replace the presets; a configured id already listed is not repeated")
}
