package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func restoreAuthClock(t *testing.T) {
	t.Helper()

	now := authNow
	t.Cleanup(func() { authNow = now })
}

func TestAuthorizeMintsADeviceFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	result := fixture.authorize(t, authMethodDeviceCode, "request-1")

	if result.Interaction != authInteractionWait {
		t.Fatalf("interaction = %q, want %q", result.Interaction, authInteractionWait)
	}

	if result.CallbackInput != "" {
		t.Fatalf("codex advertised a callback input: %q", result.CallbackInput)
	}

	if result.URL != "https://auth.openai.com/codex/device" || result.UserCode != "U9KH-GPDJ7" {
		t.Fatalf("presentation = %+v", result)
	}

	if result.Message == "" || result.FlowID == "" || result.FlowExpiresAt == 0 {
		t.Fatalf("presentation = %+v", result)
	}

	if result.PollIntervalMs != 0 {
		t.Fatalf("codex synthesized a poll hint: %d", result.PollIntervalMs)
	}

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("ledger read: ok=%v err=%v", ok, err)
	}

	if record.State != authLedgerIntent || record.FlowID != result.FlowID || record.AuthorizeRequestID != "request-1" {
		t.Fatalf("ledger record = %+v", record)
	}
}

func TestAuthorizeMintsASecretFlowWithoutANativeCall(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	result := fixture.authorize(t, authMethodAPIKey, "request-1")

	if result.Interaction != authInteractionSecret {
		t.Fatalf("interaction = %q", result.Interaction)
	}

	if result.URL != "" || result.UserCode != "" {
		t.Fatalf("a secret flow carried a url or code: %+v", result)
	}

	if result.Message != "OpenAI API key" {
		t.Fatalf("message = %q, want the method label", result.Message)
	}

	if fixture.client.deviceCalls != 0 {
		t.Fatal("a secret flow minted a native login")
	}
}

func TestAuthorizeReplaysTheSameRequestIDVerbatim(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")
	generation := fixture.mintGeneration(t)

	result, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if replayed, _ := result.(authAuthorizeResult); replayed != first {
		t.Fatal("a replayed request id did not answer verbatim")
	}

	if fixture.client.deviceCalls != 1 {
		t.Fatalf("a replay drove %d native mints", fixture.client.deviceCalls)
	}
}

// TestAuthorizeReplaysTheSameRequestIDAfterTheFlowTerminalized pins the whole
// point of the idempotency key: the repeat that matters is the one a caller
// sends after the first answer was lost, which is exactly when the flow it
// names has already completed.
func TestAuthorizeReplaysTheSameRequestIDAfterTheFlowTerminalized(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	if state := fixture.status(t, first.FlowID).State; state != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", state)
	}

	before, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	generation := fixture.mintGeneration(t)

	result, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if replayed, _ := result.(authAuthorizeResult); replayed != first {
		t.Fatal("a replayed request id did not answer verbatim after the flow terminalized")
	}

	// No second native login, and no ledger revision: the repeat consumed
	// nothing the first call had earned.
	if fixture.client.deviceCalls != 1 {
		t.Fatalf("a replay drove %d native mints", fixture.client.deviceCalls)
	}

	after, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	if after != before {
		t.Fatal("a replay rewrote the ledger entry")
	}

	if state := fixture.status(t, first.FlowID).State; state != authStateAuthenticated {
		t.Fatalf("state after replay = %q, want authenticated", state)
	}
}

// TestAuthorizeStopsReplayingOnceTheSessionCloses pins the other half: the
// record lives exactly as long as the session that owns it.
func TestAuthorizeStopsReplayingOnceTheSessionCloses(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")

	fixture.broker.closeSession(acp.SessionId(fixture.sessionID))

	if second := fixture.authorize(t, authMethodDeviceCode, "request-1"); second.FlowID == first.FlowID {
		t.Fatal("a closed session still replayed its idempotency key")
	}
}

// TestAuthorizeMintFailureAddressesTheFlowItNames pins the flowId a failed mint
// returns against a record a caller can actually address, and pins that the
// same key retries rather than replaying a presentation that was never
// published.
func TestAuthorizeMintFailureAddressesTheFlowItNames(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	fixture.client.deviceErr = errors.New("device mint refused")

	generation := fixture.mintGeneration(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	requireAuthCause(t, err, authCauseTransport)

	flowID, _ := authErrorData(t, err)[authFieldFlowID].(string)
	if flowID == "" {
		t.Fatal("a mint failure carried no flow id")
	}

	if status := fixture.status(t, flowID); status.State != authStateFailed || status.Reason != authReasonTransport {
		t.Fatalf("state/reason = %q/%q, want failed/transport", status.State, status.Reason)
	}

	fixture.client.deviceErr = nil

	retried := fixture.authorize(t, authMethodDeviceCode, "request-1")
	if retried.FlowID == flowID || retried.URL == "" {
		t.Fatal("the same key after a failed mint replayed an unpublished presentation")
	}
}

func TestAuthorizeSupersedesTheEarlierFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")
	second := fixture.authorize(t, authMethodDeviceCode, "request-2")

	if first.FlowID == second.FlowID {
		t.Fatal("the superseding authorize reused the flow id")
	}

	_, err := fixture.call(t, AuthStatusMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     first.FlowID,
	})
	if err == nil {
		t.Fatal("a superseded flow id still addressed a flow")
	}

	requireInvalidField(t, err, authFieldFlowID)
}

// TestAuthorizeDropsASupersededTerminalFlow covers the flow a supersede finds
// already terminal. It was addressable for its whole life, and being replaced
// is what ends that life rather than the transition it happened to end on, so
// its id stops addressing anything and stops being held.
func TestAuthorizeDropsASupersededTerminalFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodAPIKey, "request-1")

	if _, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     first.FlowID,
		"input":      "sk-canary",
	}); err != nil {
		t.Fatalf("callback: %v", err)
	}

	if state := fixture.status(t, first.FlowID).State; state != authStateSaved {
		t.Fatalf("state = %q, want saved", state)
	}

	fixture.authorize(t, authMethodAPIKey, "request-2")

	_, err := fixture.call(t, AuthStatusMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     first.FlowID,
	})
	if err == nil {
		t.Fatal("a superseded terminal flow id still addressed a flow")
	}

	requireInvalidField(t, err, authFieldFlowID)

	fixture.broker.mu.Lock()
	addressable := len(fixture.broker.byID)
	fixture.broker.mu.Unlock()

	if addressable != 1 {
		t.Fatalf("the addressing map holds %d flows after one supersede", addressable)
	}
}

// TestAuthorizeRefusesAKeyASupersedeRetired pins the idempotency key's whole
// promise: a repeat never destroys. Once a later authorize replaced the flow a
// key named, that key has no presentation to replay and no live flow to speak
// for, so a repeat of it is a caller addressing failure rather than a fresh
// mint that would cancel the flow the caller never asked about.
func TestAuthorizeRefusesAKeyASupersedeRetired(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	fixture.authorize(t, authMethodDeviceCode, "request-1")
	live := fixture.authorize(t, authMethodDeviceCode, "request-2")

	generation := fixture.mintGeneration(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("a retired idempotency key minted a new flow")
	}

	requireInvalidField(t, err, authFieldAuthorizeRequestID)

	if status := fixture.status(t, live.FlowID); status.State != authStatePending {
		t.Fatalf("the live flow is %q/%q after a retired key was repeated", status.State, status.Reason)
	}

	if fixture.client.deviceCalls != 2 {
		t.Fatalf("a retired key drove %d native mints", fixture.client.deviceCalls)
	}
}

func TestAuthorizeBumpsTheRevisionOnAPriorLedgerEntry(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	fixture.authorize(t, authMethodDeviceCode, "request-1")

	first, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	fixture.authorize(t, authMethodDeviceCode, "request-2")

	second, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if second.Revision != first.Revision+1 {
		t.Fatalf("revision went %d -> %d", first.Revision, second.Revision)
	}

	if second.CreatedAt != first.CreatedAt {
		t.Fatal("the superseding authorize reset the creation timestamp")
	}
}

func TestAuthorizeAddressingFailures(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	base := map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	}

	for _, field := range []string{"sessionId", "providerId", "connectionId", "methodsGeneration", "method", "authorizeRequestId"} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		_, err := fixture.call(t, AuthAuthorizeMethod, params)
		if err == nil {
			t.Fatalf("authorize accepted a request missing %q", field)
		}

		if field != "sessionId" {
			requireInvalidField(t, err, field)
		}
	}
}

func TestAuthorizeRejectsAnswersForPromptsNobodyPublished(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	for _, inputs := range []any{map[string]any{"account": "x"}, "not-an-object"} {
		_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
			"sessionId":          fixture.sessionID,
			"providerId":         authProviderOpenAI,
			"connectionId":       "connection-1",
			"methodsGeneration":  generation,
			"method":             authMethodDeviceCode,
			"authorizeRequestId": "request-1",
			"inputs":             inputs,
		})
		if err == nil {
			t.Fatalf("authorize accepted inputs %v", inputs)
		}

		requireInvalidField(t, err, authFieldInputs)
	}

	if _, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
		"inputs":             map[string]any{},
	}); err != nil {
		t.Fatalf("authorize rejected an empty inputs object: %v", err)
	}
}

func TestAuthorizeFencesTheMethodsGeneration(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	stale := fixture.mintGeneration(t)
	fresh := fixture.mintGeneration(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  stale,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("a superseded generation was accepted")
	}

	requireInvalidField(t, err, authFieldMethodsGeneration)

	_, err = fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  fresh,
		"method":             "chatgpt",
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("an unpublished method was accepted")
	}

	requireInvalidField(t, err, authFieldMethod)
}

func TestAuthorizeRejectsAMethodBeforeAnyCatalogExists(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  "never-minted",
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("authorize answered with no catalog")
	}

	requireInvalidField(t, err, authFieldMethodsGeneration)
}

func TestAuthorizeMintFailures(t *testing.T) {
	cases := map[string]struct {
		install func(*providerAuthFixture)
		cause   string
	}{
		"native": {
			install: func(f *providerAuthFixture) { f.client.deviceErr = errors.New("refused") },
			cause:   authCauseTransport,
		},
		"process": {
			install: func(f *providerAuthFixture) { f.client.deviceErr = codex.ErrConnectionClosed },
			cause:   authCauseProcess,
		},
		"loopback": {
			install: func(f *providerAuthFixture) {
				f.client.deviceLogin.VerificationURL = "https://127.0.0.1/device"
			},
			cause: authCauseUnsupportedVariant,
		},
		"badurl": {
			install: func(f *providerAuthFixture) {
				f.client.deviceLogin.VerificationURL = "http://auth.openai.com/device"
			},
			cause: authCauseNativeVeto,
		},
		"badcode": {
			install: func(f *providerAuthFixture) {
				f.client.deviceLogin.UserCode = "</script>ABCD"
			},
			cause: authCauseNativeVeto,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t)
			testCase.install(fixture)

			generation := fixture.mintGeneration(t)

			_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
				"sessionId":          fixture.sessionID,
				"providerId":         authProviderOpenAI,
				"connectionId":       "connection-1",
				"methodsGeneration":  generation,
				"method":             authMethodDeviceCode,
				"authorizeRequestId": "request-1",
			})
			if err == nil {
				t.Fatal("a failed mint reported success")
			}

			requireAuthCause(t, err, testCase.cause)
		})
	}
}

func TestAuthorizeFailsWithoutTheNativeAuthSurface(t *testing.T) {
	home := t.TempDir()
	agent := NewAgent(WithHome(home), WithProviderAuthRoot(t.TempDir()), WithProviderAuthDirectHome(home))
	storeRateLimitsSession(t, agent, "plain", newSpyCodexClient())

	result, err := agent.HandleExtensionMethod(t.Context(), AuthMethodsMethod, json.RawMessage(`{"sessionId":"plain"}`))
	if err != nil {
		t.Fatalf("methods: %v", err)
	}

	methods, _ := result.(authMethodsResult)
	generation := methods.Generation
	params, err := json.Marshal(map[string]any{
		"sessionId":          "plain",
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = agent.HandleExtensionMethod(t.Context(), AuthAuthorizeMethod, params)
	if err == nil {
		t.Fatal("authorize answered without the native auth surface")
	}

	requireAuthCause(t, err, authCauseTransport)
}

func TestAuthorizeFailsWhenTheFlowIDCannotBeMinted(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	restore := authRandRead
	authRandRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }

	defer func() { authRandRead = restore }()

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("authorize succeeded without a flow id")
	}

	requireAuthCause(t, err, authCauseProcess)
}

func TestAuthorizeFailsWhenTheLedgerCannotRecord(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	restoreLedgerHooks(t)

	ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("authorize returned before recording the flow")
	}

	requireAuthCause(t, err, authCauseProcess)

	if fixture.client.deviceCalls != 0 {
		t.Fatal("authorize minted natively before recording")
	}
}

func TestAuthCallbackAppliesASecret(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	result, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}

	if applied, _ := result.(authFlowIDResult); applied.FlowID != flow.FlowID {
		t.Fatalf("callback returned %+v", result)
	}

	if fixture.client.apiKeyValue != "sk-canary" {
		t.Fatalf("native apply received %q", fixture.client.apiKeyValue)
	}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateSaved {
		t.Fatalf("state = %q, want saved", status.State)
	}

	if status.ExpiresAt != 0 {
		t.Fatal("a saved flow claimed a credential expiry")
	}

	record, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if record.State != authLedgerConfirmed {
		t.Fatalf("ledger state = %q", record.State)
	}
}

func (f *providerAuthFixture) status(t *testing.T, flowID string) authStatusResult {
	t.Helper()

	result, err := f.call(t, AuthStatusMethod, map[string]any{
		"sessionId":  f.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flowID,
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	status, _ := result.(authStatusResult)

	return status
}

func TestAuthCallbackRejectionPaths(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	secret := fixture.authorize(t, authMethodAPIKey, "request-secret")

	base := map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     secret.FlowID,
		"input":      "sk-canary",
	}

	for _, field := range []string{"sessionId", "providerId", "method", "flowId", "input"} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		_, err := fixture.call(t, AuthCallbackMethod, params)
		if err == nil {
			t.Fatalf("callback accepted a request missing %q", field)
		}
	}

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodDeviceCode,
		"flowId":     secret.FlowID,
		"input":      "sk-canary",
	})
	if err == nil {
		t.Fatal("callback accepted a method the flow did not record")
	}

	requireInvalidField(t, err, authFieldMethod)

	_, err = fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     secret.FlowID,
		"input":      strings.Repeat("k", authMaxSecretBytes+1),
	})
	if err == nil {
		t.Fatal("callback accepted an oversize secret")
	}

	requireInvalidField(t, err, authFieldInput)
}

func TestAuthCallbackRefusesADeviceFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodDeviceCode,
		"flowId":     flow.FlowID,
		"input":      "code",
	})
	if err == nil {
		t.Fatal("callback accepted a submission on a wait flow")
	}

	requireInvalidField(t, err, authFieldInput)
}

func TestAuthCallbackOnATerminalFlowIsAFlowStateFailure(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	if _, err := fixture.call(t, AuthCancelMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flow.FlowID,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	if err == nil {
		t.Fatal("callback answered a terminal flow")
	}

	requireAuthCause(t, err, authCauseFlowState)
}

func TestAuthCallbackNativeFailures(t *testing.T) {
	cases := map[string]struct {
		install func(*providerAuthFixture)
		cause   string
		reason  string
	}{
		"apply": {
			install: func(f *providerAuthFixture) { f.client.apiKeyErr = errors.New("refused") },
			cause:   authCauseTransport,
			reason:  authReasonAcceptanceUnknown,
		},
		"ledger": {
			install: func(f *providerAuthFixture) {
				restoreLedgerHooksForFixture(f)
			},
			cause:  authCauseProcess,
			reason: authReasonAcceptanceUnknown,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t)
			flow := fixture.authorize(t, authMethodAPIKey, "request-1")

			if name == "ledger" {
				restoreLedgerHooks(t)

				ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }
			} else {
				testCase.install(fixture)
			}

			_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
				"sessionId":  fixture.sessionID,
				"providerId": authProviderOpenAI,
				"method":     authMethodAPIKey,
				"flowId":     flow.FlowID,
				"input":      "sk-canary",
			})
			if err == nil {
				t.Fatal("a failed apply reported success")
			}

			requireAuthCause(t, err, testCase.cause)

			status := fixture.status(t, flow.FlowID)
			if status.State != authStateFailed || status.Reason != testCase.reason {
				t.Fatalf("status = %+v, want failed/%s", status, testCase.reason)
			}
		})
	}
}

func restoreLedgerHooksForFixture(*providerAuthFixture) {}

func TestAuthCallbackFailsWithoutTheNativeAuthSurface(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	session, err := fixture.agent.session(acp.SessionId(fixture.sessionID))
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	session.mu.Lock()
	session.client = newSpyCodexClient()
	session.mu.Unlock()

	_, err = fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	if err == nil {
		t.Fatal("callback answered without the native auth surface")
	}

	requireAuthCause(t, err, authCauseTransport)
}

func TestAuthStatusDrivesTheBackstopBehindItsFloor(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}

	if fixture.client.accountRead != 1 {
		t.Fatalf("first status drove %d reads", fixture.client.accountRead)
	}

	fixture.status(t, flow.FlowID)

	if fixture.client.accountRead != 1 {
		t.Fatalf("a second status inside the floor drove %d reads", fixture.client.accountRead)
	}

	base := time.Now()
	authNow = func() time.Time { return base.Add(authPollFloor + time.Second) }

	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}

	if status.ExpiresAt == 0 {
		t.Fatal("an authenticated flow carried no credential expiry")
	}

	wantExpiry := authNow().Add(codexAccessTokenLifetime).UnixMilli()
	if status.ExpiresAt != wantExpiry {
		t.Fatalf("expiresAt = %d, want %d", status.ExpiresAt, wantExpiry)
	}
}

func TestAuthStatusProbeIsInertOutsideAPendingDeviceFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	secret := fixture.authorize(t, authMethodAPIKey, "request-1")

	fixture.status(t, secret.FlowID)

	if fixture.client.accountRead != 0 {
		t.Fatal("a secret flow drove the account backstop")
	}
}

func TestAuthStatusProbeToleratesNativeFailure(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.accountErr = errors.New("read")

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}
}

func TestAuthStatusProbeSkipsAMissingNativeSurface(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	session, err := fixture.agent.session(acp.SessionId(fixture.sessionID))
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	session.mu.Lock()
	session.client = newSpyCodexClient()
	session.mu.Unlock()

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}
}

func TestAuthStatusProbeStopsWhenTheLedgerCannotConfirm(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	restoreLedgerHooks(t)

	ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateFailed || status.Reason != authReasonAcceptanceUnknown {
		t.Fatalf("status = %+v, want failed/acceptance_unknown", status)
	}
}

func TestAuthStatusAddressingFailures(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	cases := []map[string]any{
		{"providerId": authProviderOpenAI, "flowId": flow.FlowID},
		{"sessionId": fixture.sessionID, "flowId": flow.FlowID},
		{"sessionId": fixture.sessionID, "providerId": authProviderOpenAI},
		{"sessionId": fixture.sessionID, "providerId": "other", "flowId": flow.FlowID},
		{"sessionId": fixture.sessionID, "providerId": authProviderOpenAI, "flowId": "absent"},
		{"sessionId": "absent", "providerId": authProviderOpenAI, "flowId": flow.FlowID},
	}

	for _, params := range cases {
		if _, err := fixture.call(t, AuthStatusMethod, params); err == nil {
			t.Fatalf("status accepted %v", params)
		}
	}
}

func TestAuthCancelIsIdempotent(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	params := map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flow.FlowID,
	}

	if _, err := fixture.call(t, AuthCancelMethod, params); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateCancelled || status.Reason != authReasonOwnerCancel {
		t.Fatalf("status = %+v", status)
	}

	result, err := fixture.call(t, AuthCancelMethod, params)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}

	if applied, _ := result.(authFlowIDResult); applied.FlowID != flow.FlowID {
		t.Fatal("the idempotent cancel did not echo the flow id")
	}

	if fixture.status(t, flow.FlowID).Reason != authReasonOwnerCancel {
		t.Fatal("the second cancel rewrote the terminal reason")
	}
}

func TestAuthCancelAddressingFailure(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.call(t, AuthCancelMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     "absent",
	})
	if err == nil {
		t.Fatal("cancel accepted an unknown flow id")
	}

	requireInvalidField(t, err, authFieldFlowID)
}

func TestAuthFlowExpiresOnItsDeadline(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	authNow = func() time.Time { return time.Now().Add(-authSafetyDeadline - time.Second) }

	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.status(t, flow.FlowID).State == authStateExpired {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateExpired || status.Reason != authReasonDeadline {
		t.Fatalf("status = %+v, want expired/deadline", status)
	}
}

func TestLoginCompletedConfirmsAndRefuses(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	if status := fixture.status(t, flow.FlowID); status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}

	refused := newProviderAuthFixture(t)
	refusedFlow := refused.authorize(t, authMethodDeviceCode, "request-1")

	refused.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1"})

	status := refused.status(t, refusedFlow.FlowID)
	if status.State != authStateFailed || status.Reason != authReasonProviderRefused {
		t.Fatalf("status = %+v, want failed/provider_refused", status)
	}
}

func TestLoginCompletedIgnoresUnmatchedCompletions(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{})
	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "other", Success: true})

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}
}

func TestLoginCompletedStopsWhenTheSessionIsGone(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	fixture.authorize(t, authMethodDeviceCode, "request-1")

	fixture.agent.removeSession(acp.SessionId(fixture.sessionID))

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	for _, flow := range fixture.broker.flows {
		if authTerminal(flow.state) {
			t.Fatalf("a flow was terminalized without its session: %+v", flow.state)
		}
	}
}

func TestProviderAuthEventRoutesLoginCompletion(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	fixture.agent.applyCodexClientEvent(t.Context(), fixture.client, codex.Event{
		Kind:  codex.EventLoginCompleted,
		Login: codex.LoginCompletion{LoginID: "login-1", Success: true},
	})

	if status := fixture.status(t, flow.FlowID); status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}
}

func TestProviderAuthEventIsInertWithoutABroker(t *testing.T) {
	agent := NewAgent()
	agent.applyCodexClientEvent(t.Context(), newSpyCodexClient(), codex.Event{
		Kind:  codex.EventLoginCompleted,
		Login: codex.LoginCompletion{LoginID: "login-1", Success: true},
	})
}

func TestAuthDisconnectClearsTheFencedAccount(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	if _, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	}); err != nil {
		t.Fatalf("callback: %v", err)
	}

	record, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if _, disconnectErr := fixture.call(t, AuthDisconnectMethod, map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      record.ConnectionID,
		"bindingGeneration": record.BindingGeneration,
	}); disconnectErr != nil {
		t.Fatalf("disconnect: %v", disconnectErr)
	}

	if fixture.client.logoutCalls != 1 {
		t.Fatalf("disconnect drove %d native removals", fixture.client.logoutCalls)
	}

	after, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if after.State != authLedgerRemoved {
		t.Fatalf("ledger state = %q", after.State)
	}

	if after.BindingGeneration != record.BindingGeneration+1 {
		t.Fatalf("binding generation went %d -> %d", record.BindingGeneration, after.BindingGeneration)
	}

	if _, repeatErr := fixture.call(t, AuthDisconnectMethod, map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      after.ConnectionID,
		"bindingGeneration": after.BindingGeneration,
	}); repeatErr == nil {
		t.Fatal("disconnect answered against a record it had already removed")
	} else {
		requireAuthCause(t, repeatErr, authCausePolicy)
	}

	if fixture.client.logoutCalls != 1 {
		t.Fatalf("a repeat drove %d native account removals", fixture.client.logoutCalls)
	}
}

func TestAuthDisconnectFencing(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	base := map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      "connection-1",
		"bindingGeneration": 1,
	}

	_, err := fixture.call(t, AuthDisconnectMethod, base)
	if err == nil {
		t.Fatal("disconnect answered with no ledger entry")
	}

	requireAuthCause(t, err, authCausePolicy)

	if writeErr := fixture.broker.ledger.write(sampleLedgerRecord()); writeErr != nil {
		t.Fatalf("seed ledger: %v", writeErr)
	}

	_, err = fixture.call(t, AuthDisconnectMethod, base)
	if err == nil {
		t.Fatal("disconnect accepted a differently fenced connection")
	}

	requireAuthCause(t, err, authCausePolicy)

	for _, field := range []string{"sessionId", "providerId", "connectionId", "bindingGeneration"} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		if _, err := fixture.call(t, AuthDisconnectMethod, params); err == nil {
			t.Fatalf("disconnect accepted a request missing %q", field)
		}
	}
}

func TestAuthDisconnectFailurePaths(t *testing.T) {
	seed := func(f *providerAuthFixture) map[string]any {
		record := sampleLedgerRecord()
		if err := f.broker.ledger.write(record); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}

		return map[string]any{
			"sessionId":         f.sessionID,
			"providerId":        authProviderOpenAI,
			"connectionId":      record.ConnectionID,
			"bindingGeneration": record.BindingGeneration,
		}
	}

	t.Run("logout", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)
		fixture.client.logoutErr = errors.New("refused")

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed removal reported success")
		}

		requireAuthCause(t, err, authCauseTransport)
	})

	t.Run("residue", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)
		fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("disconnect reported success with a resident account")
		}

		requireAuthCause(t, err, authCauseHarvestFailed)
	})

	t.Run("nosurface", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		session, err := fixture.agent.session(acp.SessionId(fixture.sessionID))
		if err != nil {
			t.Fatalf("session: %v", err)
		}

		session.mu.Lock()
		session.client = newSpyCodexClient()
		session.mu.Unlock()

		_, err = fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("disconnect answered without the native auth surface")
		}

		requireAuthCause(t, err, authCauseTransport)
	})

	t.Run("ledgerread", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		restoreLedgerHooks(t)

		ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed ledger read was swallowed")
		}

		requireAuthCause(t, err, authCauseHarvestFailed)
	})

	t.Run("intentwrite", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		restoreLedgerHooks(t)

		ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed ledger write was swallowed")
		}

		requireAuthCause(t, err, authCauseProcess)
	})

	t.Run("removalwrite", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		restoreLedgerHooks(t)

		writes := 0
		realCreateTemp := ledgerCreateTemp
		ledgerCreateTemp = func(dir string, pattern string) (ledgerFile, error) {
			writes++
			if writes > 1 {
				return nil, errors.New("temp")
			}

			return realCreateTemp(dir, pattern)
		}

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed removal record was swallowed")
		}

		requireAuthCause(t, err, authCauseProcess)
	})
}

func TestAuthCloseSessionCancelsPendingFlows(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	other := fixture.authorizeOtherSession(t)

	fixture.broker.closeSession(acp.SessionId(fixture.sessionID))

	fixture.broker.mu.Lock()
	closed := fixture.broker.byID[flow.FlowID]
	survivor := fixture.broker.byID[other]
	fixture.broker.mu.Unlock()

	if closed != nil {
		t.Fatal("a closed session left its flow addressable")
	}

	if survivor == nil {
		t.Fatal("closing one session cancelled a peer's flow")
	}
}

func (f *providerAuthFixture) authorizeOtherSession(t *testing.T) string {
	t.Helper()

	storeRateLimitsSession(t, f.agent, "thread-2", newAuthSpyClient())

	generation := f.mintGeneration(t)

	raw, err := json.Marshal(map[string]any{
		"sessionId":          "thread-2",
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-2",
		"methodsGeneration":  generation,
		"method":             authMethodAPIKey,
		"authorizeRequestId": "request-other",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := f.agent.HandleExtensionMethod(t.Context(), AuthAuthorizeMethod, raw)
	if err != nil {
		t.Fatalf("authorize peer: %v", err)
	}

	presentation, _ := result.(authAuthorizeResult)

	return presentation.FlowID
}

func TestAuthCloseSessionRunsFromCloseSession(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	if _, err := fixture.agent.CloseSession(t.Context(), acp.CloseSessionRequest{
		SessionId: acp.SessionId(fixture.sessionID),
	}); err != nil {
		t.Fatalf("close session: %v", err)
	}

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	if _, present := fixture.broker.byID[flow.FlowID]; present {
		t.Fatal("close session left the flow addressable")
	}
}

func TestAuthSupersedeIsInertOnAnAbsentFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	fixture.broker.supersede(authFlowKey{sessionID: "absent", providerID: authProviderOpenAI}, authReasonSuperseded)

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	if len(fixture.broker.flows) != 0 {
		t.Fatal("supersede invented a flow")
	}
}

func TestAuthNativeCauseClassification(t *testing.T) {
	if got := authNativeCause(context.Background(), errors.New("boom")); got != authCauseTransport {
		t.Fatalf("cause = %s, want transport", got)
	}

	if got := authNativeCause(context.Background(), codex.ErrConnectionClosed); got != authCauseProcess {
		t.Fatalf("cause = %s, want process", got)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	if got := authNativeCause(expired, errors.New("boom")); got != authCauseTimeout {
		t.Fatalf("cause = %s, want timeout", got)
	}
}
