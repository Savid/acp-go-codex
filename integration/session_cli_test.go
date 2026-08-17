//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
	"github.com/stretchr/testify/require"
)

const (
	sessionCLIHelperEnv  = "WAGIE_SESSION_CLI_HELPER"
	sessionCLIURL        = "WAGIE_API_URL"
	sessionCLIToken      = "WAGIE_API_TOKEN"
	sessionCLIOperation  = "WAGIE_OPERATION_ID"
	sessionCLIAuthHeader = "Authorization"
)

func TestCodexCLISessionCLICarrierRotation(t *testing.T) {
	requireLiveTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	grants := newSessionCLIGrantServer()
	server := httptest.NewServer(grants)
	t.Cleanup(server.Close)

	const (
		operationA1 = "operation-a-1"
		operationA2 = "operation-a-2"
		operationB  = "operation-b"
		tokenA1     = "bearer-a-1"
		tokenA2     = "bearer-a-2"
		tokenB      = "bearer-b"
	)
	grants.rotate(operationA1, tokenA1)
	grants.rotate(operationB, tokenB)

	store := newRecordingSessionStore()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, codexacp.WithSessionStore(store))
	cwd := t.TempDir()
	pathA1 := writeSessionCLIShim(t)
	pathA2 := writeSessionCLIShim(t)
	pathB := writeSessionCLIShim(t)

	first, err := conn.NewSession(ctx, sessionCLINewRequest(cwd, server.URL, operationA1, tokenA1, pathA1))
	require.NoError(t, err)
	second, err := conn.NewSession(ctx, sessionCLINewRequest(cwd, server.URL, operationB, tokenB, pathB))
	require.NoError(t, err)

	requireSessionCLITurn(t, ctx, conn, client, first.SessionId, pathA1, operationA1)

	grants.rotate(operationA2, tokenA2)
	rotated := sessionCLINewRequest(cwd, server.URL, operationA2, tokenA2, pathA2)
	resume := acp.ResumeSessionRequest{
		SessionId:             first.SessionId,
		Cwd:                   rotated.Cwd,
		AdditionalDirectories: rotated.AdditionalDirectories,
		McpServers:            rotated.McpServers,
		Meta:                  rotated.Meta,
	}
	_, err = conn.ResumeSession(ctx, resume)
	require.NoError(t, err)
	requireSessionCLITurn(t, ctx, conn, client, first.SessionId, pathA2, operationA2)

	requireSessionCLITurn(t, ctx, conn, client, second.SessionId, pathB, operationB)

	status, body := sessionCLIRequest(server.URL, operationA2, tokenA1)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, body, "rejected")
	grants.requireIsolated(t, map[string]string{
		operationA1: tokenA1,
		operationA2: tokenA2,
		operationB:  tokenB,
	})
	requireStoreOmitsValues(t, store, tokenA1, tokenA2, tokenB)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)
	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: second.SessionId})
	require.NoError(t, err)
}

func sessionCLINewRequest(cwd string, url string, operation string, token string, path string) acp.NewSessionRequest {
	return codexacp.NewSessionRequest(
		cwd,
		codexacp.WithSessionCodexOptions(codexacp.NewCodexOptions(
			codexacp.WithCodexEnv(map[string]string{
				sessionCLIHelperEnv: "1",
				sessionCLIURL:       url,
				sessionCLIOperation: operation,
				sessionCLIToken:     token,
			}),
			codexacp.WithCodexExtraPathDirs(path),
			codexacp.WithCodexApprovalPolicy("never"),
			codexacp.WithCodexSandboxPolicy("danger-full-access"),
		)),
	)
}

func requireSessionCLITurn(
	t *testing.T,
	ctx context.Context,
	conn *acp.ClientSideConnection,
	client *recordingClient,
	sessionID acp.SessionId,
	shimPath string,
	operation string,
) {
	t.Helper()

	response := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		client.resetRecordedOutput()
		command := fmt.Sprintf(
			"test \"$(command -v wagie)\" = \"%s\" || exit 71; test \"${PATH%%%%:*}\" = \"%s\" || exit 72; wagie",
			filepath.Join(shimPath, "wagie"),
			shimPath,
		)

		return conn.Prompt(ctx, acp.PromptRequest{
			Meta:      newTurnRouteMeta(),
			SessionId: sessionID,
			Prompt: []acp.ContentBlock{acp.TextBlock(fmt.Sprintf(
				"Use the exec tool exactly once with this JavaScript, without changing it: `const result = await tools.exec_command({cmd: %s, login: false}); text(result.output);`.",
				strconv.Quote(command),
			))},
		})
	})
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.Contains(t, client.text(), operation)
}

func writeSessionCLIShim(t *testing.T) string {
	t.Helper()

	binary, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wagie"), binary, 0o700))

	return dir
}

func runSessionCLIHelper() int {
	status, body := sessionCLIRequest(
		os.Getenv(sessionCLIURL),
		os.Getenv(sessionCLIOperation),
		os.Getenv(sessionCLIToken),
	)
	_, _ = fmt.Fprintln(os.Stdout, body)
	if status != http.StatusOK {
		return 1
	}

	return 0
}

func sessionCLIRequest(url string, operation string, token string) (int, string) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err.Error()
	}
	request.Header.Set(sessionCLIAuthHeader, "Bearer "+token)
	request.Header.Set(sessionCLIOperation, operation)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, err.Error()
	}

	return response.StatusCode, strings.TrimSpace(string(body))
}

type sessionCLIGrantRequest struct {
	operation  string
	token      string
	authorized bool
}

type sessionCLIGrantServer struct {
	mu       sync.Mutex
	accepted map[string]string
	requests []sessionCLIGrantRequest
}

func newSessionCLIGrantServer() *sessionCLIGrantServer {
	return &sessionCLIGrantServer{accepted: make(map[string]string)}
}

func (s *sessionCLIGrantServer) rotate(operation string, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for existing := range s.accepted {
		if strings.HasPrefix(existing, "operation-a-") && strings.HasPrefix(operation, "operation-a-") {
			delete(s.accepted, existing)
		}
	}
	s.accepted[operation] = token
}

func (s *sessionCLIGrantServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	operation := request.Header.Get(sessionCLIOperation)
	token := strings.TrimPrefix(request.Header.Get(sessionCLIAuthHeader), "Bearer ")

	s.mu.Lock()
	authorized := s.accepted[operation] == token && token != ""
	s.requests = append(s.requests, sessionCLIGrantRequest{
		operation: operation, token: token, authorized: authorized,
	})
	s.mu.Unlock()

	if !authorized {
		http.Error(writer, "rejected bearer", http.StatusUnauthorized)

		return
	}

	_, _ = fmt.Fprintf(writer, "%s|accepted", operation)
}

func (s *sessionCLIGrantServer) requireIsolated(t *testing.T, expected map[string]string) {
	t.Helper()

	s.mu.Lock()
	requests := append([]sessionCLIGrantRequest(nil), s.requests...)
	s.mu.Unlock()

	seen := make(map[string]bool, len(expected))
	for _, request := range requests {
		want, known := expected[request.operation]
		if request.authorized {
			require.True(t, known)
			require.Equal(t, want, request.token)
			seen[request.operation] = true
		}
	}
	for operation := range expected {
		require.True(t, seen[operation], "missing authorized CLI call for %s", operation)
	}
}

func requireStoreOmitsValues(t *testing.T, store *recordingSessionStore, values ...string) {
	t.Helper()

	store.mu.Lock()
	defer store.mu.Unlock()

	for _, entries := range store.entries {
		for _, entry := range entries {
			for _, value := range values {
				require.False(t, bytes.Contains(entry, []byte(value)), "session store persisted a carrier bearer")
			}
		}
	}
}
