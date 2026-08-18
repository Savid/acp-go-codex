//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestCodexNativeTwoThreadMCPRebindIsolation(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	probe := newNativeMCPRebindProbe(t)
	home := isolatedCodexHome(t)
	codexPath := integrationCodexPath(t)
	launch := func() *codex.AppServerClient {
		client, err := codex.NewAppServerClient(ctx, codex.Options{
			NativeVersion:    "0.144.1",
			CLIPath:          codexPath,
			CodexHome:        home,
			SupervisorRoot:   t.TempDir(),
			Logger:           integrationLogger,
			DarwinBestEffort: runtime.GOOS == "darwin",
		})
		require.NoError(t, err)

		return client
	}

	routes := []string{"alpha", "beta"}
	configs := map[string]map[string]any{}
	cwds := map[string]string{}
	threads := map[string]codex.Thread{}
	for _, route := range routes {
		configs[route] = nativeMCPRebindThreadConfig(probe.url(route), "Bearer "+route)
		cwds[route] = t.TempDir()
	}

	first := launch()
	for _, route := range routes {
		thread, err := first.StartThread(ctx, codex.ThreadStartRequest{
			Cwd:    cwds[route],
			Config: configs[route],
		})
		require.NoError(t, err)
		require.NotEmpty(t, thread.ID)
		threads[route] = thread

		// A native thread is not durable until its first turn. This adapter-owned
		// canary materializes the rollout without replaying any ACP user prompt.
		runNativeMCPRebindCanary(t, ctx, first, thread.ID, route, "seed-"+route, oppositeRoute(route))
	}
	require.NoError(t, first.Close(context.Background()))

	probe.reset()
	replacement := launch()
	t.Cleanup(func() { require.NoError(t, replacement.Close(context.Background())) })

	// Reverse order makes accidental last-config-wins behavior deterministic.
	for _, route := range []string{"beta", "alpha"} {
		original := threads[route]
		resumed, err := replacement.ResumeThread(ctx, codex.ThreadResumeRequest{
			ThreadID: original.ID,
			Path:     original.Path,
			Cwd:      cwds[route],
			Config:   configs[route],
		})
		require.NoError(t, err)
		require.Equal(t, original.ID, resumed.ID)

		// mcpServerStatus/list remains telemetry only. The marker turn below is
		// the verdict because it exercises native tool visibility and execution.
		statusCtx, statusCancel := context.WithTimeout(ctx, 5*time.Second)
		status, statusErr := replacement.MCPServerStatusList(statusCtx, resumed.ID)
		statusCancel()
		t.Logf("%s resumed MCP status telemetry: servers=%d err=%v", route, len(status.Servers), statusErr)

		nonce := "rebound-" + route
		runNativeMCPRebindCanary(t, ctx, replacement, resumed.ID, route, nonce, oppositeRoute(route))
		probe.requireToolCall(t, route, "Bearer "+route, nonce)
	}

	probe.requireNoCrossRouteCredentials(t)
}

func nativeMCPRebindThreadConfig(url string, bearer string) map[string]any {
	return map[string]any{
		"mcp_servers": map[string]any{
			"marker": map[string]any{
				"url":                         url,
				"http_headers":                map[string]string{"Authorization": bearer},
				"default_tools_approval_mode": "approve",
			},
		},
	}
}

func runNativeMCPRebindCanary(
	t *testing.T,
	parent context.Context,
	client *codex.AppServerClient,
	threadID string,
	route string,
	nonce string,
	crossRoute string,
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()

	turn, err := client.RunTurn(ctx, codex.TurnStartRequest{
		ThreadID: threadID,
		Prompt: []codex.UserInput{{
			"type": "text",
			"text": "Call the side-effect-free MCP tool named runtime_ready exactly once with nonce " + nonce +
				". Do not call any other tool. Reply only after the tool returns.",
		}},
		ApprovalPolicy: "never",
	})
	require.NoError(t, err)

	expected := route + "_marker:" + nonce
	forbidden := crossRoute + "_marker:"
	var observed strings.Builder
	completed := false
	for event := range turn.Events {
		observed.WriteString(event.Text)
		observed.WriteString(event.Tool.Content)
		if len(event.Tool.Raw) > 0 {
			raw, marshalErr := json.Marshal(event.Tool.Raw)
			require.NoError(t, marshalErr)
			observed.Write(raw)
		}
		if event.Kind == codex.EventError {
			require.NoError(t, event.Err)
		}
		if event.Kind == codex.EventCompleted {
			require.Equal(t, codex.StopReasonEndTurn, event.StopReason)
			completed = true
		}
	}

	require.True(t, completed, "native marker turn did not complete; events=%s", observed.String())
	require.Contains(t, observed.String(), expected, "native marker result was not returned to its owning thread")
	require.NotContains(t, observed.String(), forbidden, "native marker result crossed thread configuration")
}

func oppositeRoute(route string) string {
	if route == "alpha" {
		return "beta"
	}

	return "alpha"
}

type nativeMCPRebindProbe struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []nativeMCPRebindRequest
}

type nativeMCPRebindRequest struct {
	route     string
	bearer    string
	method    string
	toolName  string
	arguments map[string]any
}

func newNativeMCPRebindProbe(t *testing.T) *nativeMCPRebindProbe {
	t.Helper()

	probe := &nativeMCPRebindProbe{}
	probe.server = httptest.NewServer(http.HandlerFunc(probe.handle))
	t.Cleanup(probe.server.Close)

	return probe
}

func (p *nativeMCPRebindProbe) url(route string) string {
	return p.server.URL + "/" + route
}

func (p *nativeMCPRebindProbe) handle(writer http.ResponseWriter, request *http.Request) {
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	route := strings.TrimPrefix(request.URL.Path, "/")
	bearer := request.Header.Get("Authorization")
	p.mu.Lock()
	p.requests = append(p.requests, nativeMCPRebindRequest{
		route:     route,
		bearer:    bearer,
		method:    message.Method,
		toolName:  message.Params.Name,
		arguments: cloneNativeMCPArguments(message.Params.Arguments),
	})
	p.mu.Unlock()

	if route != "alpha" && route != "beta" {
		http.Error(writer, "unknown route", http.StatusNotFound)

		return
	}
	if bearer != "Bearer "+route {
		http.Error(writer, "wrong bearer", http.StatusUnauthorized)

		return
	}
	if message.Method == "notifications/initialized" {
		writer.WriteHeader(http.StatusAccepted)

		return
	}

	var result any
	switch message.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "codex-rebind-" + route, "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{{
			"name":        "runtime_ready",
			"description": "Return the route marker and supplied nonce without side effects.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"nonce": map[string]any{"type": "string"}},
				"required":   []string{"nonce"},
			},
		}}}
	case "tools/call":
		if message.Params.Name != "runtime_ready" {
			writeNativeMCPError(writer, message.ID, -32602, "unexpected tool")

			return
		}
		nonce, _ := message.Params.Arguments["nonce"].(string)
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": route + "_marker:" + nonce}},
			"isError": false,
		}
	default:
		writeNativeMCPError(writer, message.ID, -32601, "method not found")

		return
	}

	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      message.ID,
		"result":  result,
	})
}

func writeNativeMCPError(writer http.ResponseWriter, id json.RawMessage, code int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func cloneNativeMCPArguments(arguments map[string]any) map[string]any {
	if arguments == nil {
		return nil
	}

	cloned := make(map[string]any, len(arguments))
	for key, value := range arguments {
		cloned[key] = value
	}

	return cloned
}

func (p *nativeMCPRebindProbe) reset() {
	p.mu.Lock()
	p.requests = nil
	p.mu.Unlock()
}

func (p *nativeMCPRebindProbe) requireToolCall(t *testing.T, route string, bearer string, nonce string) {
	t.Helper()

	p.mu.Lock()
	requests := append([]nativeMCPRebindRequest(nil), p.requests...)
	p.mu.Unlock()

	for _, request := range requests {
		if request.route == route && request.bearer == bearer && request.method == "tools/call" &&
			request.toolName == "runtime_ready" && request.arguments["nonce"] == nonce {
			return
		}
	}

	t.Fatalf("missing native MCP marker call route=%q bearer=%q nonce=%q; requests=%s", route, bearer, nonce, formatNativeMCPRequests(requests))
}

func (p *nativeMCPRebindProbe) requireNoCrossRouteCredentials(t *testing.T) {
	t.Helper()

	p.mu.Lock()
	requests := append([]nativeMCPRebindRequest(nil), p.requests...)
	p.mu.Unlock()

	for _, request := range requests {
		require.Equal(t, "Bearer "+request.route, request.bearer, "thread-scoped native MCP bearer crossed routes")
	}
}

func formatNativeMCPRequests(requests []nativeMCPRebindRequest) string {
	parts := make([]string, 0, len(requests))
	for _, request := range requests {
		parts = append(parts, fmt.Sprintf("%s %s %s %#v", request.route, request.bearer, request.method, request.arguments))
	}

	return strings.Join(parts, "; ")
}
