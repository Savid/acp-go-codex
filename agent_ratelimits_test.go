package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-codex/internal/codex"
)

type quotaCodexClient struct {
	*spyCodexClient
	quotaMu   sync.Mutex
	account   codex.Account
	config    codex.RateLimitsContext
	configErr error
	started   chan struct{}
	release   chan struct{}
	cwd       string
}

func (c *quotaCodexClient) AccountRead(context.Context) (codex.Account, error) {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()

	return c.account, nil
}

func (c *quotaCodexClient) ReadRateLimitsContext(_ context.Context, cwd string) (codex.RateLimitsContext, error) {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	c.cwd = cwd

	return c.config, c.configErr
}

func (c *quotaCodexClient) ReadRateLimits(ctx context.Context) (codex.RateLimitSnapshot, error) {
	if c.started != nil {
		close(c.started)
		select {
		case <-ctx.Done():
			return codex.RateLimitSnapshot{}, ctx.Err()
		case <-c.release:
		}
	}

	return c.spyCodexClient.ReadRateLimits(ctx)
}

func newQuotaCodexAgent(t *testing.T) (*Agent, *quotaCodexClient) {
	t.Helper()
	client := &quotaCodexClient{spyCodexClient: newSpyCodexClient(),
		account: codex.Account{ID: "account-1", AuthMode: codex.AuthModeChatGPT},
		config:  codex.RateLimitsContext{ProviderID: "openai"}}
	client.rateLimits = codex.RateLimitSnapshot{AccountID: "account-1", ObservedAt: time.Now().UTC(), Pools: []codex.RateLimitPool{
		{ID: "codex", PlanType: "pro", Windows: []codex.RateLimitWindow{{ID: "primary", UsedPercent: 0, DurationSeconds: new(int64(604800))}}},
		{ID: "spark", Label: "Spark", Windows: []codex.RateLimitWindow{{ID: "primary", UsedPercent: 125}}},
	}}
	agent := NewAgent()
	agent.runtimeClient = client
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	return agent, client
}

func storeRateLimitsSession(t *testing.T, agent *Agent, id string, client codex.Client) {
	t.Helper()
	session := newSession(agent, acp.SessionId(id), absTestPath("tmp", "project"), nil,
		codex.Thread{ID: id, Provider: "openai"}, client, sessionMeta{}, nil)
	require.NoError(t, agent.storeStartedSession(session))
}

func newRateLimitsFixtureAgent(t *testing.T) *Agent {
	t.Helper()
	agent, client := newQuotaCodexAgent(t)
	storeRateLimitsSession(t, agent, "session-1", client)
	storeRateLimitsSession(t, agent, "😀: session ", client)

	return agent
}

func TestRateLimitsFreshQuery(t *testing.T) {
	agent, client := newQuotaCodexAgent(t)
	for range 2 {
		result, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`{}`))
		require.NoError(t, err)
		response, ok := result.(RateLimitsResponse)
		require.True(t, ok)
		require.Equal(t, "openai", response.ProviderID)
		require.Equal(t, "available", response.Availability)
		require.Len(t, response.Pools, 2)
		require.Equal(t, "pro", response.Pools[0].PlanType)
		require.Equal(t, float64(0), *response.Pools[0].Windows[0].UsedPercent)
		require.Equal(t, int64(604800), *response.Pools[0].Windows[0].DurationSeconds)
		require.Equal(t, float64(125), *response.Pools[1].Windows[0].UsedPercent)
		require.Empty(t, response.Pools[1].Windows[0].Status)
		require.Equal(t, client.rateLimits.ObservedAt.Format(time.RFC3339Nano), response.Pools[0].Windows[0].ObservedAt)
	}
	require.Equal(t, 2, client.rateLimitsReads)
}

func TestRateLimitsAvailability(t *testing.T) {
	for _, tc := range []struct {
		name         string
		configure    func(*Agent, *quotaCodexClient)
		params       string
		availability string
		reason       string
		reads        int
	}{
		{name: "unknown provider", params: `{"providerId":"unknown"}`, availability: "unsupported"},
		{name: "API key", configure: func(_ *Agent, c *quotaCodexClient) { c.account.AuthMode = codex.AuthModeAPIKey }, availability: "unsupported"},
		{name: "logged out", configure: func(_ *Agent, c *quotaCodexClient) { c.account = codex.Account{} }, availability: "unavailable", reason: "not_authenticated"},
		{name: "custom endpoint", configure: func(_ *Agent, c *quotaCodexClient) { c.config.Custom = true }, availability: "unsupported"},
		{name: "custom provider", configure: func(_ *Agent, c *quotaCodexClient) { c.config.ProviderID = "custom" }, availability: "unsupported"},
		{name: "reader timeout", configure: func(_ *Agent, c *quotaCodexClient) { c.rateLimitsErr = context.DeadlineExceeded }, availability: "unavailable", reason: "read_failed", reads: 1},
		{name: "read failure", configure: func(_ *Agent, c *quotaCodexClient) { c.rateLimitsErr = errors.New("secret native response") }, availability: "unavailable", reason: "read_failed", reads: 1},
		{name: "config failure", configure: func(_ *Agent, c *quotaCodexClient) { c.configErr = errors.New("config read failed") }, params: `{"providerId":"openai"}`, availability: "unavailable", reason: "read_failed"},
		{name: "no observations", configure: func(_ *Agent, c *quotaCodexClient) { c.rateLimits = codex.RateLimitSnapshot{} }, availability: "unavailable", reason: "not_observed", reads: 1},
		{name: "account mismatch", configure: func(_ *Agent, c *quotaCodexClient) { c.rateLimits.AccountID = "another-account" }, availability: "unavailable", reason: "read_failed", reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, client := newQuotaCodexAgent(t)
			if tc.configure != nil {
				tc.configure(agent, client)
			}
			result, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(tc.params))
			require.NoError(t, err)
			response, ok := result.(RateLimitsResponse)
			require.True(t, ok)
			require.Equal(t, tc.availability, response.Availability)
			require.Equal(t, tc.reason, response.Reason)
			require.NotNil(t, response.Pools)
			require.Empty(t, response.Pools)
			require.Equal(t, tc.reads, client.rateLimitsReads)
		})
	}
}

func TestRateLimitsUsesNativeAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        map[string]string
		sessionEnv map[string]string
	}{
		{name: "OpenAI API key", env: map[string]string{"OPENAI_API_KEY": "test-key"}},
		{name: "Codex API key", env: map[string]string{"CODEX_API_KEY": "test-key"}},
		{name: "session shell API key", sessionEnv: map[string]string{"OPENAI_API_KEY": "test-key"}},
	} {
		for _, mode := range []string{codex.AuthModeChatGPT, codex.AuthModeAPIKey} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				agent, client := newQuotaCodexAgent(t)
				agent.options.Env = tc.env
				client.account.AuthMode = mode
				request := RateLimitsRequest{ProviderID: "openai"}
				if tc.sessionEnv != nil {
					storeRateLimitsSession(t, agent, "session-1", client)
					session, err := agent.session("session-1")
					require.NoError(t, err)
					session.env = tc.sessionEnv
					request.SessionID = "session-1"
				}
				response, err := agent.rateLimits(context.Background(), request)
				require.NoError(t, err)
				if mode == codex.AuthModeAPIKey {
					require.Equal(t, "unsupported", response.Availability)
					require.Empty(t, response.Pools)
					require.Zero(t, client.rateLimitsReads)

					return
				}
				require.Equal(t, "available", response.Availability)
				require.Len(t, response.Pools, 2)
				require.Equal(t, 1, client.rateLimitsReads)
			})
		}
	}
}

func TestRateLimitsSessionValidationAndResolution(t *testing.T) {
	agent, client := newQuotaCodexAgent(t)
	_, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`{"providerId":"unknown","sessionId":"missing"}`))
	require.Error(t, err)
	require.Zero(t, client.rateLimitsReads)
	storeRateLimitsSession(t, agent, "session-1", client)
	result, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`{"sessionId":"session-1"}`))
	require.NoError(t, err)
	response, ok := result.(RateLimitsResponse)
	require.True(t, ok)
	require.Equal(t, "available", response.Availability)
	require.Equal(t, absTestPath("tmp", "project"), client.cwd)

	client.configErr = errors.New("missing config")
	_, err = agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`{}`))
	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, map[string]any{"error": "missing", "field": "providerId"}, requestErr.Data)
}

func TestRateLimitsDiscardInFlightChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Agent, *quotaCodexClient)
	}{
		{name: "auth epoch", mutate: func(a *Agent, _ *quotaCodexClient) { a.invalidateRateLimitsAuth() }},
		{name: "runtime epoch", mutate: func(a *Agent, _ *quotaCodexClient) { a.mu.Lock(); a.runtimeEpoch++; a.mu.Unlock() }},
		{name: "account", mutate: func(_ *Agent, c *quotaCodexClient) {
			c.quotaMu.Lock()
			c.account.ID = "replacement"
			c.quotaMu.Unlock()
		}},
		{name: "endpoint", mutate: func(_ *Agent, c *quotaCodexClient) { c.quotaMu.Lock(); c.config.Custom = true; c.quotaMu.Unlock() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, client := newQuotaCodexAgent(t)
			client.started, client.release = make(chan struct{}), make(chan struct{})
			done := make(chan RateLimitsResponse, 1)
			go func() {
				response, err := agent.rateLimits(context.Background(), RateLimitsRequest{ProviderID: "openai"})
				if err != nil {
					response.Reason = err.Error()
				}
				done <- response
			}()
			select {
			case <-client.started:
			case <-time.After(5 * time.Second):
				t.Fatal("quota read did not start")
			}
			tc.mutate(agent, client)
			close(client.release)
			select {
			case response := <-done:
				require.Equal(t, "read_failed", response.Reason)
				require.Empty(t, response.Pools)
			case <-time.After(5 * time.Second):
				t.Fatal("quota read did not finish")
			}
		})
	}
}

func TestRateLimitsCallerCancellation(t *testing.T) {
	agent, client := newQuotaCodexAgent(t)
	client.started, client.release = make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := agent.rateLimits(ctx, RateLimitsRequest{ProviderID: "openai"}); done <- err }()
	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("quota read did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not finish")
	}
}

func TestRateLimitsExpiredObservations(t *testing.T) {
	snapshot := codex.RateLimitSnapshot{ObservedAt: time.Now().UTC(), Pools: []codex.RateLimitPool{{ID: "codex", Windows: []codex.RateLimitWindow{{ID: "primary", UsedPercent: 15, ResetsAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339)}}}}}
	response := rateLimitsResponseFromSnapshot("openai", snapshot)
	require.Equal(t, "not_observed", response.Reason)
	snapshot.Pools[0].Windows[0].ResetsAt = ""
	snapshot.ObservedAt = time.Now().Add(-61 * time.Second)
	require.Equal(t, "not_observed", rateLimitsResponseFromSnapshot("openai", snapshot).Reason)
}

func TestHandleExtensionMethodRateLimitsClosedAgent(t *testing.T) {
	agent := NewAgent()
	require.NoError(t, agent.Close())
	_, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, json.RawMessage(`{}`))
	require.Error(t, err)
}

func TestRateLimitsDiscardSessionConfigurationChange(t *testing.T) {
	agent, client := newQuotaCodexAgent(t)
	storeRateLimitsSession(t, agent, "session-1", client)
	client.started, client.release = make(chan struct{}), make(chan struct{})
	done := make(chan RateLimitsResponse, 1)
	go func() {
		response, err := agent.rateLimits(context.Background(), RateLimitsRequest{SessionID: "session-1"})
		if err != nil {
			response.Reason = err.Error()
		}
		done <- response
	}()
	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("quota read did not start")
	}
	session, err := agent.session("session-1")
	require.NoError(t, err)
	session.mu.Lock()
	session.env = map[string]string{"OPENAI_API_KEY": "replacement-test-key"}
	session.mu.Unlock()
	close(client.release)
	select {
	case response := <-done:
		require.Equal(t, "read_failed", response.Reason)
		require.Empty(t, response.Pools)
	case <-time.After(5 * time.Second):
		t.Fatal("quota read did not finish")
	}
}

// Each native operation can fail after its owning lifecycle changes. The
// numbered call sequence includes configuration/account checks on both sides
// of the quota read, so every acquisition failure path gets the same proof.
type failingQuotaReadClient struct {
	*quotaCodexClient
	failAt         int
	calls          int
	failure        error
	failureStarted chan struct{}
	failureRelease chan struct{}
}

func (c *failingQuotaReadClient) waitForQuotaFailure(ctx context.Context) error {
	c.calls++
	if c.calls != c.failAt {
		return nil
	}

	close(c.failureStarted)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.failureRelease:
		return c.failure
	}
}

func (c *failingQuotaReadClient) AccountRead(ctx context.Context) (codex.Account, error) {
	if err := c.waitForQuotaFailure(ctx); err != nil {
		return codex.Account{}, err
	}

	return c.quotaCodexClient.AccountRead(ctx)
}

func (c *failingQuotaReadClient) ReadRateLimitsContext(ctx context.Context, cwd string) (codex.RateLimitsContext, error) {
	if err := c.waitForQuotaFailure(ctx); err != nil {
		return codex.RateLimitsContext{}, err
	}

	return c.quotaCodexClient.ReadRateLimitsContext(ctx, cwd)
}

func (c *failingQuotaReadClient) ReadRateLimits(ctx context.Context) (codex.RateLimitSnapshot, error) {
	if err := c.waitForQuotaFailure(ctx); err != nil {
		return codex.RateLimitSnapshot{}, err
	}

	return c.quotaCodexClient.ReadRateLimits(ctx)
}

func TestRateLimitsFailurePreservesLifecycle(t *testing.T) {
	stages := []string{"initial config", "initial account", "quota", "final account", "final config"}
	for stage, name := range stages {
		t.Run(name, func(t *testing.T) {
			for _, change := range []struct {
				name  string
				apply func(*testing.T, *Agent, *session, context.CancelFunc) error
			}{
				{name: "agent closed", apply: func(t *testing.T, a *Agent, _ *session, _ context.CancelFunc) error {
					t.Helper()
					require.NoError(t, a.Close())

					return newAgentClosedError()
				}},
				{name: "session removed", apply: func(t *testing.T, a *Agent, s *session, _ context.CancelFunc) error {
					t.Helper()
					require.Same(t, s, a.removeSession(s.id))
					t.Cleanup(func() { a.mu.Lock(); a.sessions[s.id] = s; a.mu.Unlock() })

					return newUnknownSession()
				}},
				{name: "session closing", apply: func(t *testing.T, _ *Agent, s *session, _ context.CancelFunc) error {
					t.Helper()
					s.mu.Lock()
					s.closing = true
					s.mu.Unlock()
					t.Cleanup(func() { s.mu.Lock(); s.closing = false; s.mu.Unlock() })

					return newSessionCloseInProgress()
				}},
				{name: "session poisoned", apply: func(t *testing.T, _ *Agent, s *session, _ context.CancelFunc) error {
					t.Helper()
					failure := acp.NewInternalError(map[string]any{"error": "quota-test-lifecycle-failure"})
					s.mu.Lock()
					s.lifecycleFailure = failure
					s.mu.Unlock()
					t.Cleanup(func() { s.mu.Lock(); s.lifecycleFailure = nil; s.mu.Unlock() })

					return failure
				}},
				{name: "caller cancelled during shutdown", apply: func(t *testing.T, a *Agent, _ *session, cancel context.CancelFunc) error {
					t.Helper()
					require.NoError(t, a.Close())
					cancel()

					return context.Canceled
				}},
			} {
				t.Run(change.name, func(t *testing.T) {
					agent, base := newQuotaCodexAgent(t)
					client := &failingQuotaReadClient{quotaCodexClient: base, failAt: stage + 1,
						failure: codex.ErrConnectionClosed, failureStarted: make(chan struct{}), failureRelease: make(chan struct{})}
					agent.runtimeClient = client
					storeRateLimitsSession(t, agent, "session-1", client)
					session, err := agent.session("session-1")
					require.NoError(t, err)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan error, 1)
					go func() {
						_, err := agent.HandleExtensionMethod(ctx, RateLimitsMethod, json.RawMessage(`{"sessionId":"session-1"}`))
						done <- err
					}()
					select {
					case <-client.failureStarted:
					case <-time.After(5 * time.Second):
						t.Fatal("native quota stage did not start")
					}
					want := change.apply(t, agent, session, cancel)
					close(client.failureRelease)
					select {
					case err := <-done:
						require.Equal(t, want, err)
					case <-time.After(5 * time.Second):
						t.Fatal("failed quota stage did not finish")
					}
				})
			}
		})
	}
}
